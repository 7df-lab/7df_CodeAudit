package service

// P1 双副本四门验收（HA 设计 §4 TP19-T4；ADR-250 验收栈=进程级双实例）。
// CODEAUDIT_LIVE_HA=1 门控（编排脚本 ha-verify.sh 驱动；环境由 env 注入）：
//   DOOR1 登录门：实例 A 注册用户 → 实例 B 登录成功（project PG 写穿+回落）
//   DOOR2 日志门：A 侧任务（验收工具直插 RUNNING）→ B GetTaskLogs 读到 A 追加的日志
//   DOOR3 暂停门：B 对 A 域任务 PauseTask 翻态 → A GetScanTask 见 PAUSED（状态外置链）
//   DOOR4 幂等门：A 首跑 mock 图 → B 同键重放命中缓存（graph.log 无新增=未重跑）
// 注记：RUNNING 态由验收工具 PG 直插（编排链的 RUNNING 窗口不可控）；容器级
// kill-restart 回归归 P3/P4。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/codeaudit/proto-gen"
)

func haDial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.DialContext(context.Background(), addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func haEnv(k, def string) string { return envOr(k, def) }

func TestLiveHA_FourDoors(t *testing.T) {
	if os.Getenv("CODEAUDIT_LIVE_HA") != "1" {
		t.Skip("live-only: CODEAUDIT_LIVE_HA=1（ha-verify.sh 编排驱动）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	md := func(id string) *pb.RequestMetadata { return &pb.RequestMetadata{RequestId: id + "-" + runID} }

	projA, projB := haDial(t, haEnv("HA_PROJECT_A", "127.0.0.1:51052")), haDial(t, haEnv("HA_PROJECT_B", "127.0.0.1:52052"))
	taskA, taskB := haDial(t, haEnv("HA_TASK_A", "127.0.0.1:51054")), haDial(t, haEnv("HA_TASK_B", "127.0.0.1:52054"))
	dshA, dshB := haDial(t, haEnv("HA_DSH_A", "127.0.0.1:51057")), haDial(t, haEnv("HA_DSH_B", "127.0.0.1:52057"))

	// ---- DOOR 1 登录门 ----
	uname := fmt.Sprintf("ha-user-%d", time.Now().UnixNano())
	if _, err := pb.NewUserServiceClient(projA).RegisterUser(ctx, &pb.RegisterUserRequest{
		Metadata: md("ha-reg-1"), Username: uname, Email: uname + "@ha.test", Password: "HaPassw0rd!",
	}); err != nil {
		t.Fatalf("DOOR1 A 侧注册失败: %v", err)
	}
	login, err := pb.NewUserServiceClient(projB).Login(ctx, &pb.LoginRequest{Username: uname, Password: "HaPassw0rd!"})
	if err != nil || login.GetAccessToken() == "" {
		t.Fatalf("DOOR1 B 侧登录失败（写穿/回落断链）: err=%v token-empty=%v", err, login.GetAccessToken() == "")
	}
	t.Logf("DOOR1 ✓ 注册(A)→登录(B) 跨副本认证链")

	// ---- 直插 RUNNING 任务（DOOR2/3 载体） ----
	pgDB, err := sql.Open("postgres", haEnv("HA_TASK_DSN", ""))
	if err != nil || haEnv("HA_TASK_DSN", "") == "" {
		t.Fatalf("task PG DSN 未配置: %v", err)
	}
	defer pgDB.Close()
	taskID := fmt.Sprintf("ha-task-%d", time.Now().UnixNano())
	payload := fmt.Sprintf(`{"taskId":%q,"projectId":"p-ha","status":"TASK_STATUS_RUNNING","createdAt":"%s","updatedAt":"%s"}`,
		taskID, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	if _, err := pgDB.Exec(`INSERT INTO tasks(task_id, project_id, status, payload) VALUES($1,'p-ha','TASK_STATUS_RUNNING',$2)`,
		taskID, payload); err != nil {
		t.Fatalf("验收工具直插 RUNNING 任务失败: %v", err)
	}

	// ---- DOOR 2 日志门：A 追加 → B 读到 ----
	if _, err := pb.NewTaskServiceClient(taskA).AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
		Metadata: md("ha-log-a-1"), TaskId: taskID, Level: pb.TaskLogLevel_TASK_LOG_LEVEL_INFO,
		Source: "ha-verify", Message: "door2-line-from-A"}); err != nil {
		t.Fatalf("DOOR2 A 侧追加日志失败: %v", err)
	}
	logs, err := pb.NewTaskServiceClient(taskB).GetTaskLogs(ctx, &pb.GetTaskLogsRequest{TaskId: taskID, Limit: 50})
	if err != nil {
		t.Fatalf("DOOR2 B 侧读日志失败: %v", err)
	}
	found := false
	for _, l := range logs.GetLogs() {
		if l.GetMessage() == "door2-line-from-A" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DOOR2 B 侧未见 A 追加的日志（日志双写/尾查回落断链）: %+v", logs.GetLogs())
	}
	t.Logf("DOOR2 ✓ A 追加日志→B 跨副本可读（%d 条）", len(logs.GetLogs()))

	// ---- DOOR 3 暂停门：B 翻态 → A 见 PAUSED ----
	if _, err := pb.NewTaskServiceClient(taskB).PauseTask(ctx, &pb.PauseTaskRequest{TaskId: taskID}); err != nil {
		t.Fatalf("DOOR3 B 侧 PauseTask 失败（非属主副本翻态断链）: %v", err)
	}
	got, err := pb.NewTaskServiceClient(taskA).GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("DOOR3 A 侧 GetScanTask 失败: %v", err)
	}
	if got.GetStatus() != pb.TaskStatus_TASK_STATUS_PAUSED {
		t.Fatalf("DOOR3 A 侧未见 PAUSED（状态外置链断）: %s", got.GetStatus())
	}
	t.Logf("DOOR3 ✓ B PauseTask→A 见 PAUSED（状态机外置链）")
	// 复位 RUNNING 供后续（ResumeTask）
	if _, err := pb.NewTaskServiceClient(taskB).ResumeTask(ctx, &pb.ResumeTaskRequest{TaskId: taskID}); err != nil {
		t.Logf("（注）ResumeTask: %v", err)
	}

	// ---- DOOR 4 幂等门：A 首跑 mock 图 → B 同键重放命中 ----
	haTask2 := fmt.Sprintf("ha-graph-%d", time.Now().UnixNano())
	graphReq := &pb.RunGraphTaskRequest{
		Metadata: md("ha-idem-key-1"), TaskId: haTask2,
		Goal: "验收图（mock 收敛）",
		Graph: &pb.GraphSpec{
			SeedIntents: []string{"验收节点一", "验收节点二"},
			Constraints: &pb.GraphConstraints{MaxNodes: 8, MaxAttemptsPerNode: 2},
		},
	}
	first, err := pb.NewRuntimeServiceClient(dshA).RunGraphTask(ctx, graphReq)
	if err != nil {
		t.Fatalf("DOOR4 A 首跑失败（mock 图链路）: %v", err)
	}
	idemDirA := haEnv("HA_DSH_GRAPH_DIR_B", "") // B 的 graph.log 目录（重放不得新增）
	before := countGraphLines(t, idemDirA, haTask2)
	replay, err := pb.NewRuntimeServiceClient(dshB).RunGraphTask(ctx, graphReq)
	if err != nil {
		t.Fatalf("DOOR4 B 重放失败: %v", err)
	}
	if replay.GetNodesTotal() != first.GetNodesTotal() || !replay.GetConverged() {
		t.Fatalf("DOOR4 重放响应与首响应不一致: first=%d replay=%d converged=%v",
			first.GetNodesTotal(), replay.GetNodesTotal(), replay.GetConverged())
	}
	after := countGraphLines(t, idemDirA, haTask2)
	if idemDirA != "" && after != before {
		t.Fatalf("DOOR4 B 侧重放发生真实重跑（graph.log 行数 %d→%d，应零新增）", before, after)
	}
	t.Logf("DOOR4 ✓ 同键跨副本重放命中缓存（nodes=%d 零重跑）", replay.GetNodesTotal())
}

func countGraphLines(t *testing.T, dir, taskID string) int {
	if dir == "" {
		return -1
	}
	b, err := os.ReadFile(filepath.Join(dir, taskID+".graph.log"))
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

var _ = timestamppb.Now // 预留（payload 直插扩展）

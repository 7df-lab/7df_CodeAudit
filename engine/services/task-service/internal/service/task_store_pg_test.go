package service

// R-31 锁定测试：任务实体持久化的可离线验证面。
// PG 读写回路属集成面（部署后真库验证），此处锁：payload 往返一致性 + 坏 DSN fail-loud。

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/codeaudit/proto-gen"
)

// TestTaskStorePayloadRoundTrip — upsert/hydrate 两侧共用 protojson 全量 payload，
// 往返必须保真（含 stages/sast_tools/config/时间戳），否则重启回放即数据变形。
func TestTaskStorePayloadRoundTrip(t *testing.T) {
	src := &pb.ScanTask{
		TaskId:     "gw-roundtrip-1",
		ProjectId:  "proj-1",
		Status:     pb.TaskStatus_TASK_STATUS_COMPLETED,
		ScanMode:   pb.ScanMode_SCAN_MODE_SAST_ONLY,
		CreatedBy:  "user-001",
		SastTools:  []string{"opengrep", "bandit"},
		CreatedAt:  timestamppb.New(time.Unix(1788827300, 0)),
		UpdatedAt:  timestamppb.New(time.Unix(1788827900, 0)),
		ErrorMessage: "",
	}
	src.Config = map[string]string{"upload_file_id": "file-1"}

	payload, err := protojson.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dst := &pb.ScanTask{}
	if err := protojson.Unmarshal(payload, dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(src, dst) {
		t.Fatalf("payload round-trip 变形:\n src=%+v\n dst=%+v", src, dst)
	}
	if dst.GetStatus().String() != "TASK_STATUS_COMPLETED" || dst.GetScanMode().String() != "SCAN_MODE_SAST_ONLY" {
		t.Fatalf("枚举往返失真: status=%s mode=%s", dst.GetStatus(), dst.GetScanMode())
	}
	if len(dst.GetSastTools()) != 2 || dst.GetConfig()["upload_file_id"] != "file-1" {
		t.Fatalf("列表/映射往返失真: tools=%v config=%v", dst.GetSastTools(), dst.GetConfig())
	}
}

// TestNewPGTaskStoreBadDSN — DSN 已配置但 PG 不可用必须 fail-loud（NewTaskService panic 口径的前置），
// 禁止静默回落内存档把持久化缺口藏进启动日志。
func TestNewPGTaskStoreBadDSN(t *testing.T) {
	if _, err := newPGTaskStore("this-is-not-a-valid-dsn"); err == nil {
		t.Fatal("坏 DSN 未报错——持久化缺口将被静默吞掉（R-31）")
	}
}

// TestPersistTaskNilStoreNoop — 内存档（pgStore=nil）下写穿必须 no-op 不 panic。
func TestPersistTaskNilStoreNoop(t *testing.T) {
	s := &TaskServiceImpl{}
	s.persistTaskLocked(&pb.ScanTask{TaskId: "gw-nil"})
}

// TestPayloadJSONNoSchemeRegression — 防回归锚：payload 是 protojson（camelCase 键），
// 与 kafka 事件（snake_case 手拼 map）是两套序列化，禁止互相"借用"键名。
func TestPayloadJSONNoSchemeRegression(t *testing.T) {
	src := &pb.ScanTask{TaskId: "gw-x", CreatedBy: "user-001"}
	payload, _ := protojson.Marshal(src)
	if strings.Contains(string(payload), `"created_by"`) || strings.Contains(string(payload), `"task_id"`) {
		t.Fatalf("payload 出现 snake_case 键——protojson 口径被手拼 map 污染: %s", payload)
	}
}

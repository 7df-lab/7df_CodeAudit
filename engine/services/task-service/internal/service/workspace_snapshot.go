// Package service — workspace=team 创建时快照（ADR-247 切片3后半）。
//
// 裁决：CreateScanTask 锁外预查 project-service GetProject 拿 Project.team，
// 非空则钉 task.Config["workspace"]（team 名即网关 workspace 名，恒等映射）。
// 时机=创建而非启动：B2 快照为真相 + pack PinLatest"任务创建即钉"同型先例 +
// 重试任务防 project.team 改名漂移（同任务两次运行不同 workspace=计量/隔离
// 归因撕裂）。查询失败/无 team=不写键（回落链：config.workspace →
// runtime.sandbox.workspace 全局缺省）——不引入创建失败面（锁内禁网络
// 调用，预查在锁外；project-service 不可达不应阻断任务创建，与
// project_config_fallback 的启动期兜底语义分层）。
package service

import (
	"context"
	"log"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fetchProjectTeam — 读 Project.team（ADR-247）；失败返回空串（回落链兜底）。
func (s *TaskServiceImpl) fetchProjectTeam(projectID string) string {
	conn, err := grpc.Dial(s.projectAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("[task] workspace snapshot: project dial %s: %v (fallback to default workspace)", s.projectAddr, err)
		return ""
	}
	defer conn.Close()
	client := pb.NewProjectServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.GetProject(ctx, &pb.GetProjectRequest{ProjectId: projectID})
	if err != nil {
		log.Printf("[task] workspace snapshot: GetProject %s: %v (fallback to default workspace)", projectID, err)
		return ""
	}
	return resp.GetTeam() // GetProject 直接返回 Project（proto L1209）
}

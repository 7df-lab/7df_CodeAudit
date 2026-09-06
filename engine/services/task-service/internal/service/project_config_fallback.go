// 项目配置兜底查询：任务未携带源码来源（project_path / upload_file_id）时，
// 向 project-service GetProjectConfig 读取项目 config map（proto L849/L1156）。
// 取档顺序见 task_service.go StartTask：项目 upload_file_id（ADR-203 项目级上传件，
// gateway 零落盘直传 storage）→ project_path（ADR-148 解包目录遗留档）→ repo_url（ADR-163 clone）。
package service

import (
	"context"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const projectConfigTimeout = 5 * time.Second

// fetchProjectConfigValue — 读项目 config map 单键；RPC 失败/键缺省一律空串（调用方降级到下一档）
func (s *TaskServiceImpl) fetchProjectConfigValue(projectID, key string) string {
	conn, err := grpc.Dial(s.projectAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return ""
	}
	defer conn.Close()
	client := pb.NewProjectServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), projectConfigTimeout)
	defer cancel()
	resp, err := client.GetProjectConfig(ctx, &pb.GetProjectConfigRequest{ProjectId: projectID})
	if err != nil {
		return ""
	}
	return resp.GetConfig()[key]
}

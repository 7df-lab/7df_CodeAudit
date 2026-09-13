// 项目配置兜底查询：任务未携带源码来源（project_path / upload_file_id）时，
// 向 project-service GetProjectConfig 读取项目 config map（proto L849/L1156）。
// 取档顺序见 task_service.go StartTask：项目 upload_file_id（ADR-203 项目级上传件，
// gateway 零落盘直传 storage）→ project_path（ADR-148 解包目录遗留档）→ repo_url（ADR-163 clone）。
package service

import (
	"fmt"
	"context"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const projectConfigTimeout = 5 * time.Second

// fetchProjectConfigValue — 读项目 config map 单键；RPC 失败/键缺省返回空串。
// R63失败原因经 fetchProjectConfigErr 带回（StartTask 拼接进
// ErrorMessage）——原静默空串会让"项目本有 upload_file_id 但瞬态失败"的任务静默
// 降级 repo clone 扫错代码。
func (s *TaskServiceImpl) fetchProjectConfigValue(projectID, key string) string {
	v, _ := s.fetchProjectConfigValueErr(projectID, key)
	return v
}

// fetchProjectConfigValueErr — 同上，附带失败原因（成功时为空串）。
func (s *TaskServiceImpl) fetchProjectConfigValueErr(projectID, key string) (string, string) {
	conn, err := grpc.Dial(s.projectAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Sprintf("项目配置读取失败: %v", err)
	}
	defer conn.Close()
	client := pb.NewProjectServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), projectConfigTimeout)
	defer cancel()
	resp, err := client.GetProjectConfig(ctx, &pb.GetProjectConfigRequest{ProjectId: projectID})
	if err != nil {
		return "", fmt.Sprintf("项目配置读取失败（GetProjectConfig）: %v", err)
	}
	return resp.GetConfig()[key], ""
}

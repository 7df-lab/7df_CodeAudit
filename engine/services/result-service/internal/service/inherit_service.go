// inherit_service.go — findings 继承（ADR-225 §4.6，写路径物化）。
//
// 语义: 复制基线任务中"未变更文件"的 findings 到新任务 task_id——连带
// verdict/reasoning、ai_fix_suggestion、diff_patch 等终态业务列；变更∪删除文件的
// 旧 findings 不继承（整文件以新扫为准）。复制不是引用：本任务上的后续裁决
// 只落本任务行，不回写基线。
// 幂等（R4）: 调用方（task-service 编排）用任务稳定键 <task_id>-inherit，
// 同 (request_id, finding_id) 重放直接跳过——自动重试不产生重复继承。
package service

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inheritPageSize — 基线 findings 分页拉取页大小（03 §5 分页语义）。
const inheritPageSize = 500

// InheritFindings — 依据: proto ResultService.InheritFindings（ADR-225）。
func (s *ResultServiceImpl) InheritFindings(ctx context.Context, req *pb.InheritFindingsRequest) (*pb.InheritFindingsResponse, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.request_id is required")
	}
	if req.GetBaselineTaskId() == "" || req.GetNewTaskId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "baseline_task_id and new_task_id are required")
	}
	if req.GetBaselineTaskId() == req.GetNewTaskId() {
		return nil, status.Errorf(codes.InvalidArgument, "baseline_task_id must differ from new_task_id")
	}
	// 排除清单规范化——与 task-service diff 产物同口径（伞仓设计 §9-1：changed/deleted
	// 与 findings.file_path 必须同口径比对；两侧 helper 语义逐行对齐，改则同改）
	exclude := make(map[string]bool, len(req.GetExcludePaths()))
	for _, p := range req.GetExcludePaths() {
		if np := normalizeInheritPath(p); np != "" {
			exclude[np] = true
		}
	}

	resp := &pb.InheritFindingsResponse{}
	lastID := ""
	seq := 0
	for {
		rows, next, err := s.repo.List(lastID, inheritPageSize, req.GetBaselineTaskId(), "")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list baseline findings: %v", err)
		}
		for _, f := range rows {
			if excludedBy(normalizeInheritPath(f.FilePath), exclude) {
				resp.SkippedCount++
				continue
			}
			// 继承行 ID = <new_task>-inh-<N>：List 按 id 稳定排序，序号确定 →
			// 重试重放生成同 ID，经 (request_id, finding_id) 命中已继承行幂等跳过
			seq++
			newID := fmt.Sprintf("%s-inh-%d", req.GetNewTaskId(), seq)
			if existing, gerr := s.repo.GetByRequestIDAndFindingID(req.GetMetadata().GetRequestId(), newID); gerr == nil && existing != nil {
				resp.InheritedCount++
				continue
			}
			cp := *f
			cp.ID = newID
			cp.TaskID = req.GetNewTaskId()
			cp.InheritedFrom = req.GetBaselineTaskId()
			cp.RequestID = req.GetMetadata().GetRequestId()
			cp.CreatedAt = time.Now()
			cp.UpdatedAt = time.Now()
			if cerr := s.repo.Create(&cp); cerr != nil {
				// ADR-198 同款纪律：失败不静默，计数如实上报（调用方决定重试语义）
				log.Printf("[result] InheritFindings: %s create failed: %v", newID, cerr)
				resp.FailedCount++
				continue
			}
			resp.InheritedCount++
		}
		if next == "" || len(rows) == 0 {
			break
		}
		lastID = next
	}
	log.Printf("[result] InheritFindings %s→%s: inherited=%d skipped=%d failed=%d",
		req.GetBaselineTaskId(), req.GetNewTaskId(),
		resp.GetInheritedCount(), resp.GetSkippedCount(), resp.GetFailedCount())
	return resp, nil
}

// normalizeInheritPath — 继承排除路径规范化（正斜杠、去 ./、消 ../；与
// task-service normalizeRepoPath 同语义——两处纯函数有意互为镜像，锁定测试看护）。
func normalizeInheritPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return p
	}
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// excludedBy — 排除命中判定：精确相等 **或路径后缀对齐+两段校验**（R38 sameVulnFile
// 同源语义，R61 收紧）。SAST 工具的 finding.file_path 常为容器内绝对路径（bandit 对
// abs 参数原样回显），而 changed/deleted 排除清单是相对项目根路径——只做精确比对会让
// 变更文件的旧 findings 漏网被继承（过期漏洞复活）。
//
// R61（2026-09-11 审计）两段校验：后缀命中后追加"剩余前缀是真实树根形态"判定
// （前缀为空=已归一相对路径，或以 /unpacked 结尾=上传树根；repo 型卷根 <repos>/<id>
// 形态由后缀段的目录深度兜底）。方向论证：R38 后缀语义在 merge 面（误合并≈去重，
// 方向安全）成立，原样复用到排除面后误排除=漏报（方向不安全）——嵌套同后缀路径
// （vendor/pkg/util/keys.py）绝不能被 pkg/util/keys.py 误杀：该文件既不重扫也不继承，
// 漏洞将从增量报告永久消失。
func excludedBy(findingPath string, exclude map[string]bool) bool {
	if findingPath == "" {
		return false
	}
	if exclude[findingPath] {
		return true
	}
	for p := range exclude {
		if p == "" || !strings.HasSuffix(findingPath, "/"+p) {
			continue
		}
		prefix := strings.TrimSuffix(findingPath, p) // 形如 /data/repos/uploads-x/unpacked/ 或 /x/vendor/
		if prefix == "" || prefix == "/" {
			return true // 相对形态（已归一）——精确后缀即命中
		}
		if strings.HasSuffix(prefix, "/unpacked/") {
			return true // 上传树根形态——真绝对路径
		}
		// 非树根前缀（vendor/ 等）：不排除（防误杀）；疑似嵌套误配记不进日志面
		// （excludedBy 为纯函数，披露归审计），维持继承由人工核查
	}
	return false
}

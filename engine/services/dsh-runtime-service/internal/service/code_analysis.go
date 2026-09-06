// Package service — CodeAnalysisService 实现（dsh-runtime 内嵌模块）。
//
// 依据:
//   - codeaudit_common.proto L976-L982 CodeAnalysisService 定义（AnalyzeCode/QueryCPG/
//     GetCallGraph/GetDataFlow/GetAnalysisProgress）
//   - 04 §3.x 阶段2b AnalyzeCode → CPG+AST+语言统计
//   - 04 §6 降级策略: CPG 失败→AST 降级（本实现返回文件级 AST 统计即降级产物）
//   - 05 §4 Code Analyst 职责: 代码结构分析、依赖追踪
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// codeExt — 语言扩展名 → 语言名映射（01 §7 第一梯队语言）。
var codeExt = map[string]string{
	".go": "go", ".py": "python", ".js": "javascript", ".ts": "typescript",
	".java": "java", ".c": "c", ".cpp": "cpp", ".cc": "cpp", ".h": "c", ".hpp": "cpp",
}

// analysisRecord 记录一次分析产出（QueryCPG/进度查询用）。
type analysisRecord struct {
	result *pb.CodeAnalysisResult
}

// CodeAnalysisServiceImpl implements CodeAnalysisService.
type CodeAnalysisServiceImpl struct {
	pb.UnimplementedCodeAnalysisServiceServer
	mu    sync.RWMutex
	byTsk map[string]*analysisRecord // task_id → 最近一次分析
}

func newCodeAnalysisService() *CodeAnalysisServiceImpl {
	return &CodeAnalysisServiceImpl{byTsk: map[string]*analysisRecord{}}
}

// analyzeProject — 遍历项目目录产出文件级统计（CPG 存储不可用时的 AST 级产物，04 §6）。
func analyzeProject(projectPath string) *pb.CodeAnalysisResult {
	res := &pb.CodeAnalysisResult{
		CpgStoragePath: filepath.Join(projectPath, ".codeaudit", "cpg.json"),
		CpgAccessUrl:   "file://" + filepath.Join(projectPath, ".codeaudit", "cpg.json"),
		LanguageStats:  &pb.LanguageStats{Languages: map[string]*pb.LanguageInfo{}},
	}
	var totalLines, totalFiles int32
	_ = filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && (info.Name() == ".git" || info.Name() == "__pycache__" ||
				info.Name() == "node_modules" || info.Name() == ".toolchain") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		lang, ok := codeExt[ext]
		if !ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := int32(strings.Count(string(data), "\n") + 1)
		totalFiles++
		totalLines += lines

		li, ok := res.GetLanguageStats().Languages[lang]
		if !ok {
			li = &pb.LanguageInfo{}
			res.GetLanguageStats().Languages[lang] = li
		}
		li.Files++
		li.Lines += lines

		res.Files = append(res.Files, &pb.FileInfo{
			FilePath:  path,
			Language:  lang,
			LineCount: lines,
		})
		return nil
	})
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].GetFilePath() < res.Files[j].GetFilePath() })
	res.Features = &pb.ProjectFeatures{TotalFiles: totalFiles, TotalLines: totalLines}

	// 2026-08-27 修复(编造审计): CpgStoragePath/CpgAccessUrl 此前指向从未写出的文件。
	// 现真实落盘 AST 级摘要（文件清单+语言统计; CPG 污点图 M9 前的降级产物, 04 §6）,
	// 保证响应中的路径引用真实存在。
	if summary, err := json.Marshal(map[string]interface{}{
		"generated_at": time.Now().Format(time.RFC3339),
		"level":        "ast-summary",
		"project_path": projectPath,
		"features":     res.GetFeatures(),
		"languages":    res.GetLanguageStats().GetLanguages(),
		"files":        res.GetFiles(),
	}); err == nil {
		if err := os.MkdirAll(filepath.Dir(res.GetCpgStoragePath()), 0o755); err == nil {
			if err := os.WriteFile(res.GetCpgStoragePath(), summary, 0o644); err == nil {
				log.Printf("[code-analysis] cpg(ast-summary) written: %s (%d bytes)",
					res.GetCpgStoragePath(), len(summary))
			}
		}
	}
	return res
}

// AnalyzeCode — 04 §3 阶段2：代码预处理（changed_files 空=全量；04 §5 增量机制 M9 再启用）。
func (s *CodeAnalysisServiceImpl) AnalyzeCode(ctx context.Context, req *pb.AnalyzeCodeRequest) (*pb.AnalyzeCodeResponse, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	if req.GetProjectPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "project_path is required")
	}
	if _, err := os.Stat(req.GetProjectPath()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "project_path inaccessible: %v", err)
	}

	res := analyzeProject(req.GetProjectPath())

	s.mu.Lock()
	s.byTsk[req.GetTaskId()] = &analysisRecord{result: res}
	s.mu.Unlock()

	return &pb.AnalyzeCodeResponse{Result: res}, nil
}

// QueryCPG — proto L1336: 按 cpg_storage_path 读取 AnalyzeCode 落盘的 AST 级摘要。
// 诚实语义: 文件不存在时返回含说明的 JSON（不冒充图数据）。
func (s *CodeAnalysisServiceImpl) QueryCPG(ctx context.Context, req *pb.QueryCPGRequest) (*pb.QueryCPGResponse, error) {
	if p := req.GetCpgStoragePath(); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			return &pb.QueryCPGResponse{ResultJson: string(data)}, nil
		}
		return &pb.QueryCPGResponse{ResultJson: fmt.Sprintf(
			`{"error":"cpg_storage_path not found","path":%q,"level":"ast-summary"}`, p)}, nil
	}
	return &pb.QueryCPGResponse{ResultJson: "{}"}, nil
}

// GetCallGraph — proto L979。
// ADR-134 诚实化: 此前恒返回空 {} 且无标注，调用方易当真；CPG 未接入前显式 Unimplemented。
func (s *CodeAnalysisServiceImpl) GetCallGraph(ctx context.Context, req *pb.GetCallGraphRequest) (*pb.CallGraph, error) {
	return nil, status.Error(codes.Unimplemented, "call graph requires CPG backend (06 OpenShell/CPG not connected); QueryCPG AST-summary is available")
}

// GetDataFlow — proto L980。
func (s *CodeAnalysisServiceImpl) GetDataFlow(ctx context.Context, req *pb.GetDataFlowRequest) (*pb.DataFlowGraph, error) {
	return nil, status.Error(codes.Unimplemented, "data-flow graph requires CPG backend (06 OpenShell/CPG not connected)")
}

// GetAnalysisProgress — proto L981。
// ADR-134 诚实化: 此前恒 100%（冒充终态）。AnalyzeCode 为同步 RPC，无独立进度流，
// 无从汇报真实进度 → 显式 Unimplemented。
func (s *CodeAnalysisServiceImpl) GetAnalysisProgress(ctx context.Context, req *pb.GetAnalysisProgressRequest) (*pb.CodeAnalysisProgress, error) {
	return nil, status.Error(codes.Unimplemented, "AnalyzeCode is synchronous; no independent progress stream")
}

// NewCodeAnalysisService — 导出构造器（cmd/main.go 注册用；依据 proto L976-L982）。
func NewCodeAnalysisService() *CodeAnalysisServiceImpl {
	return newCodeAnalysisService()
}

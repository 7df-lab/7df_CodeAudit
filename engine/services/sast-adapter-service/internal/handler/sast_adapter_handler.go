// Package handler — SASTAdapterService 的 gRPC 实现。
//
// 依据:
//   - codeaudit_common.proto L1022-L1031 SASTAdapterService 六 RPC 定版
//   - 04 §3.2 阶段2a RunMultipleScans（SAST 扫描，UnifiedFinding 输出）
//   - 03 §2 幂等三态（RunSASTScan/RunMultipleScans 必须携带幂等键 R4/R6）
//   - 09 §2 通信矩阵行 sast-adapter→result 落盘 finding（10s/3）
//   - 01 §5 十适配器口径（工具执行 → adapters.Registry 解析 → UnifiedFinding）
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/sast-adapter-service/adapters"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// toolCommand — 工具执行命令映射（实现细节级配置，ADR-121；ADR-137：命令行/兜底
// 一律来自全局配置 sast_adapter.tools，代码内不留工具命令缺省）。
type toolCommand struct {
	argv         []string // 含占位 {project}
	rawFormat    string   // raw 输出的解析器 = Registry key
	pythonModule string   // PATH 无该命令时的 python -m 兜底
	pythonPath   string   // python -m 所需 PYTHONPATH
}

// loadToolCommands — 从全局配置装配工具执行映射（缺键 fail-fast，ADR-137）。
func loadToolCommands() map[string]toolCommand {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("sast-adapter config: %v (ADR-137)", err))
	}
	tools := map[string]toolCommand{}
	// ADR-158: 引擎替代 semgrep→opengrep（同 schema/规则模式；解析器两者均注册）
	for _, id := range []string{"bandit", "opengrep"} {
		argv, err := cfg.StrSlice(fmt.Sprintf("sast_adapter.tools.%s.argv", id))
		if err != nil {
			panic(fmt.Sprintf("sast-adapter config: %v (ADR-137)", err))
		}
		tc := toolCommand{argv: argv, rawFormat: id}
		if m, merr := cfg.Str(fmt.Sprintf("sast_adapter.tools.%s.python_module", id)); merr == nil {
			tc.pythonModule = m
			if pp, perr := cfg.Str(fmt.Sprintf("sast_adapter.tools.%s.pythonpath", id)); perr == nil {
				tc.pythonPath = pp
			}
		}
		tools[id] = tc
	}
	return tools
}

// SASTAdapterHandler implements SASTAdapterService.
type SASTAdapterHandler struct {
	pb.UnimplementedSASTAdapterServiceServer

	mu           sync.Mutex
	store        map[string]*pb.UnifiedFinding // taskID-scoped finding id → entity
	byTask       map[string]map[string]bool    // taskID → toolID → 已执行过扫描（进度按 task+tool 口径, ADR-133）
	idempotency  sync.Map                      // request_id → cached response
	resultAddr   string                        // result-service 地址（落盘用；09 §2 行）
	scanTimeout  time.Duration                 // 单工具扫描超时（07 §8 正式口径 20m 的本地映射；值在全局配置 sast_adapter.scan_timeout_s）
	toolCommands map[string]toolCommand        // 工具执行映射（ADR-137：来自全局配置）
}

// NewSASTAdapterHandler creates the adapter service handler.
// ADR-137: 扫描超时与工具命令来自全局配置（缺键 panic fail-fast）。
func NewSASTAdapterHandler(resultAddr string) *SASTAdapterHandler {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("sast-adapter config: %v (ADR-137)", err))
	}
	scanT, err := cfg.Int("sast_adapter.scan_timeout_s")
	if err != nil {
		panic(fmt.Sprintf("sast-adapter config: %v (ADR-137)", err))
	}
	return &SASTAdapterHandler{
		store:        map[string]*pb.UnifiedFinding{},
		byTask:       map[string]map[string]bool{},
		resultAddr:   resultAddr,
		scanTimeout:  time.Duration(scanT) * time.Second,
		toolCommands: loadToolCommands(),
	}
}

// findingsOf returns stored entities for ids (fusion 同进程直取复用).
func (h *SASTAdapterHandler) findingsOf(ids []string) []*pb.UnifiedFinding {
	out := make([]*pb.UnifiedFinding, 0, len(ids))
	for _, id := range ids {
		if f, ok := h.store[id]; ok {
			out = append(out, f)
		}
	}
	return out
}

// ---- 真实工具执行（exec.Command）----

// toolCommandFor — 全局工具映射查询（loadToolCommands 结果在 handler 初始化时装配）。
var toolCommandsGlobal = loadToolCommands()

func toolCommandFor(tool string) toolCommand {
	return toolCommandsGlobal[tool]
}

// findRulesDir — {rules} 占位解析：从 CWD 向上找 configs/codeaudit.yaml 定位仓库根
// + services/sast-adapter-service/rules（ADR-144；找不到时返回错误=诚实失败）。
func findRulesDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "services", "sast-adapter-service", "rules")
		if _, err := os.Stat(filepath.Join(dir, "configs", "codeaudit.yaml")); err == nil {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			return "", fmt.Errorf("rules dir missing: %s", candidate)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repo root (configs/codeaudit.yaml) not found from CWD")
		}
		dir = parent
	}
}

func resolveToolArgv(argv []string, project string) ([]string, error) {
	out := make([]string, len(argv))
	for i, a := range argv {
		if strings.Contains(a, "{project}") {
			out[i] = strings.ReplaceAll(a, "{project}", project)
		} else if strings.Contains(a, "{rules}") {
			rd, err := findRulesDir()
			if err != nil {
				return nil, err
			}
			out[i] = strings.ReplaceAll(a, "{rules}", rd)
		} else {
			out[i] = a
		}
	}
	// 模块入口兜底：PATH 无命令但配置了 python_module 且 pythonpath 存在时走 python -m（ADR-137）
	if tc := toolCommandFor(out[0]); tc.pythonModule != "" {
		if _, err := exec.LookPath(out[0]); err != nil {
			if _, err := os.Stat(filepath.Join(tc.pythonPath, tc.pythonModule)); err == nil {
				return append([]string{"python3", "-m", tc.pythonModule}, out[1:]...), nil
			}
			return nil, fmt.Errorf("%s not available: install or provide %s", out[0], tc.pythonPath)
		}
	}
	if _, err := exec.LookPath(out[0]); err != nil {
		return nil, fmt.Errorf("tool %q not found in PATH", out[0])
	}
	return out, nil
}

func (h *SASTAdapterHandler) runTool(projectPath string, tool string) ([]byte, error) {
	tc, ok := h.toolCommands[tool]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "no executor mapping for tool %s (ADR-121)", tool)
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return nil, err
	}
	argv, err := resolveToolArgv(tc.argv, abs)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v (04 §6 工具失败→跳过继续)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.scanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// python -m 兜底需要配置的 pythonpath 可导入（ADR-137）
	if argv[0] == "python3" && tc.pythonPath != "" {
		cmd.Env = append(os.Environ(), "PYTHONPATH="+tc.pythonPath)
	}
	start := time.Now()
	out, err := cmd.Output()
	log.Printf("[sast-adapter] executed %s bytes=%d took=%s err=%v",
		tool, len(out), time.Since(start).Round(time.Millisecond), err)

	if ctx.Err() == context.DeadlineExceeded {
		return nil, status.Error(codes.DeadlineExceeded, "tool timeout (07 §8)")
	}
	if err != nil {
		// bandit 发现问题时 exit 1 但仍输出 JSON——区分处理
		if _, ok := err.(*exec.ExitError); ok && len(out) > 0 {
			return out, nil
		}
		return nil, status.Errorf(codes.Unavailable, "tool %s failed: %v (04 §6)", tool, err)
	}
	return out, nil
}

// scanOneTool 执行单工具并解析为 proto UnifiedFinding 列表。
func (h *SASTAdapterHandler) scanOneTool(taskID, projectID, projectPath, tool string) (*pb.ToolScanResult, []*pb.UnifiedFinding, error) {
	parser, err := adapters.GetParser(tool)
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	start := time.Now()
	raw, rerr := h.runTool(projectPath, tool)

	if rerr != nil {
		// 04 §6: SAST 工具失败→跳过继续（记录状态 FAILED，不中断任务）
		code := codes.Unavailable
		if st, ok := status.FromError(rerr); ok {
			code = st.Code()
		}
		return &pb.ToolScanResult{
			ToolName:     tool,
			FindingIds:   []string{},
			Status:       pb.ScanStatus_SCAN_STATUS_FAILED,
			ErrorMessage: rerr.Error(),
			Metrics:      &pb.ScanMetrics{DurationMs: time.Since(start).Milliseconds()},
		}, nil, status.Error(code, rerr.Error())
	}

	parsed, err := parser.Parse(taskID, projectID, raw)
	if err != nil {
		return &pb.ToolScanResult{
			ToolName:     tool,
			FindingIds:   []string{},
			Status:       pb.ScanStatus_SCAN_STATUS_FAILED,
			ErrorMessage: fmt.Sprintf("parse: %v", err),
		}, nil, status.Errorf(codes.Internal, "parse output of %s: %v", tool, err)
	}
	// ADR-144: semgrep 系引擎目录扫描被 git 枚举吞掉时（本仓环境实测 scanned=[]）→ 文件级回退
	// ADR-158: opengrep 同 schema 同缺陷面, 回退条件一并覆盖
	if (tool == "semgrep" || tool == "opengrep") && len(parsed.Findings) == 0 && strings.Contains(string(raw), `"scanned":[]`) {
		log.Printf("[sast-adapter][%s] zero scanned paths (git enumeration), file-level fallback", tool)
		absPath, aerr := filepath.Abs(projectPath) // 回退用绝对路径（进程 CWD 不保证在仓库根）
		if aerr == nil {
			if raw2, ferr := h.semgrepFilesFallback(absPath, tool); ferr == nil {
				if parsed2, perr := parser.Parse(taskID, projectID, raw2); perr == nil && len(parsed2.Findings) > 0 {
					parsed = parsed2
				}
			} else {
				log.Printf("[sast-adapter][%s] fallback failed: %v", tool, ferr)
			}
		}
	}

	ids := make([]string, 0, len(parsed.Findings))
	protoFindings := make([]*pb.UnifiedFinding, 0, len(parsed.Findings))
	for i := range parsed.Findings {
		f := parsed.Findings[i]
		// 必填字段校验接入（ADR-133）：缺失 source_tool/file_path/start_line 的脏数据不入库
		// 依据: adapters/tools.go ValidateRequired
		if verr := adapters.ValidateRequired(f); verr != nil {
			log.Printf("[sast-adapter][%s] skip invalid finding %q: %v", tool, f.FindingID, verr)
			continue
		}
		sev := pb.Severity(pb.Severity_value[f.Severity])
		finding := &pb.UnifiedFinding{
			FindingId:    f.FindingID,
			TaskId:       taskID,
			ProjectId:    projectID,
			SourceTool:   f.SourceTool,
			SourceRuleId: f.SourceRuleID,
			Location: &pb.LocationInfo{
				FilePath:     f.Location.FilePath,
				StartLine:    int32(f.Location.StartLine),
				EndLine:      int32(f.Location.EndLine),
				StartColumn:  int32(f.Location.StartColumn),
				EndColumn:    int32(f.Location.EndColumn),
				FunctionName: f.Location.FunctionName,
				ClassName:    f.Location.ClassName,
			},
			CweId:       f.CWEID,
			Title:       f.Title,
			Description: f.Description,
			Severity:    sev,
			Confidence:  f.Confidence,
			// ADR-141: 代码上下文随 finding 走（proto L67 source_raw="原始输出（JSON序列化）"
			// 的既有字段语义——人工复核与 LLM 审核都需要代码行，此前在此处被丢弃）
			SourceRaw: f.SourceRaw,
			Status:    pb.FindingStatus_FINDING_STATUS_PENDING,
		}
		h.mu.Lock()
		h.store[f.FindingID] = finding
		if h.byTask[taskID] == nil {
			h.byTask[taskID] = map[string]bool{}
		}
		h.byTask[taskID][tool] = true
		h.mu.Unlock()
		ids = append(ids, f.FindingID)
		protoFindings = append(protoFindings, finding)
	}

	tsr := &pb.ToolScanResult{
		ToolName:   tool,
		FindingIds: ids,
		Metrics: &pb.ScanMetrics{
			DurationMs:         time.Since(start).Milliseconds(),
			FilesScanned:       parsed.Metrics.FilesScanned,
			LinesScanned:       parsed.Metrics.LinesScanned,
			FindingsCount:      parsed.Metrics.FindingsCount,
			FindingsBySeverity: sevMap(parsed.Metrics.BySeverity),
		},
		Status: pb.ScanStatus_SCAN_STATUS_COMPLETED,
	}
	return tsr, protoFindings, nil
}

func sevMap(m map[string]int32) map[string]int32 {
	out := map[string]int32{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---- RPC 实现 ----

// RunSASTScan — 单工具扫描。R4: 缺幂等键→INVALID_ARGUMENT。
func (h *SASTAdapterHandler) RunSASTScan(ctx context.Context, req *pb.RunSASTScanRequest) (*pb.RunSASTScanResponse, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	reqID := req.GetMetadata().GetRequestId()

	if cached, ok := h.idempotency.Load(reqID); ok {
		return cached.(*pb.RunSASTScanResponse), nil // 03 §2 同键同体重放
	}

	projectPath := req.GetProjectPath()
	if projectPath == "" {
		return nil, status.Error(codes.InvalidArgument, "project_path is required")
	}
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	tsr, protoFindings, err := h.scanOneTool(req.GetTaskId(), "", projectPath, req.GetToolId())
	if err != nil {
		// 已产出 FAILED ToolScanResult 时正常返回（04 §6），只有系统性错误才返回错误码
		if tsr == nil {
			return nil, err
		}
		resp := &pb.RunSASTScanResponse{Result: tsr}
		return resp, nil
	}

	// 09 §2 行 sast-adapter→result 落盘 finding
	if len(protoFindings) > 0 && h.resultAddr != "" {
		n, perr := persistToResult(h.resultAddr, req.GetTaskId(), reqID+"-"+req.GetToolId(), protoFindings)
		if perr != nil {
			log.Printf("[sast-adapter][%s] persist %d findings FAILED: %v", req.GetToolId(), len(protoFindings), perr)
		} else {
			log.Printf("[sast-adapter][%s] persisted %d findings to result-service", req.GetToolId(), n)
		}
	}

	resp := &pb.RunSASTScanResponse{Result: tsr}
	h.idempotency.Store(reqID, resp)
	return resp, nil
}

// RunMultipleScans — 多工具扫描汇总（04 §3.2 阶段2a）。
func (h *SASTAdapterHandler) RunMultipleScans(ctx context.Context, req *pb.RunMultipleScansRequest) (*pb.RunMultipleScansResponse, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	reqID := req.GetMetadata().GetRequestId()
	if cached, ok := h.idempotency.Load("multi:" + reqID); ok {
		return cached.(*pb.RunMultipleScansResponse), nil
	}

	projectPath := req.GetProjectPath()
	if projectPath == "" {
		return nil, status.Error(codes.InvalidArgument, "project_path is required")
	}
	tools := req.GetToolIds()
	if len(tools) == 0 {
		return nil, status.Error(codes.InvalidArgument, "tool_ids is required")
	}

	results := map[string]*pb.ToolScanResult{}
	allIDs := []string{}
	var allFindings []*pb.UnifiedFinding
	var total int32
	totalStart := time.Now()

	// ADR-182：多工具并行执行（人类定义"多个SAST工具并行审计"）。scanOneTool 无共享
	// 可变状态（解析器值接收者无状态；finding_id=taskID-tool-N 局部序号），exec 各自
	// 独立进程。结果按 tools 声明序汇装，保持响应确定性。
	type toolOut struct {
		tsr      *pb.ToolScanResult
		findings []*pb.UnifiedFinding
		err      error
	}
	outs := make([]toolOut, len(tools))
	var wg sync.WaitGroup
	for i, tool := range tools {
		wg.Add(1)
		go func(i int, tool string) {
			defer wg.Done()
			tsr, protoFindings, err := h.scanOneTool(req.GetTaskId(), "", projectPath, tool)
			outs[i] = toolOut{tsr: tsr, findings: protoFindings, err: err}
		}(i, tool)
	}
	wg.Wait()
	for _, out := range outs {
		if out.tsr != nil {
			results[out.tsr.GetToolName()] = out.tsr
		}
		if out.err == nil {
			allIDs = append(allIDs, out.tsr.GetFindingIds()...)
			allFindings = append(allFindings, out.findings...)
			total += int32(len(out.tsr.GetFindingIds()))
		}
	}

	// 落盘（04 §6: 单工具失败不 fail 整体；落盘失败必须告警——ADR-133）
	if len(allFindings) > 0 && h.resultAddr != "" {
		if _, perr := persistToResult(h.resultAddr, req.GetTaskId(), reqID+"-multi", allFindings); perr != nil {
			log.Printf("[sast-adapter][multi] persist %d findings FAILED: %v", len(allFindings), perr)
		}
	}

	resp := &pb.RunMultipleScansResponse{Result: &pb.SASTScanResult{
		Results:         results,
		TotalFindings:   total,
		TotalDurationMs: time.Since(totalStart).Milliseconds(),
	}}
	h.idempotency.Store("multi:"+reqID, resp)
	return resp, nil
}

// ListAvailableTools — proto L1026。
// ADR-133 诚实化：仅列出有 executor 映射（可真实执行）的工具。此前把 10 个注册的
// 解析器全列为"可用"，但 codeql/spotbugs/eslint 等无执行映射，扫描必然失败——能力虚报。
// 解析器≠执行器：解析器只负责解析工具原始输出。
func (h *SASTAdapterHandler) ListAvailableTools(ctx context.Context, req *pb.ListAvailableToolsRequest) (*pb.ListAvailableToolsResponse, error) {
	resp := &pb.ListAvailableToolsResponse{}
	for id, tc := range h.toolCommands {
		p, err := adapters.GetParser(id)
		if err != nil {
			continue // 有执行映射但无解析器：异常组合，跳过并告警
		}
		resp.Tools = append(resp.Tools, &pb.SASTToolInfo{
			ToolId:             p.ToolID(),
			Name:               p.ToolID(),
			SupportedLanguages: p.SupportedLanguages(),
			OutputFormat:       tc.rawFormat,
		})
	}
	return resp, nil
}

// GetToolInfo — proto L1027。
func (h *SASTAdapterHandler) GetToolInfo(ctx context.Context, req *pb.GetToolInfoRequest) (*pb.SASTToolInfo, error) {
	if req.GetToolId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_id is required")
	}
	p, err := adapters.GetParser(req.GetToolId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	info := &pb.SASTToolInfo{
		ToolId:             p.ToolID(),
		Name:               p.ToolID(),
		SupportedLanguages: p.SupportedLanguages(),
		OutputFormat:       "json",
	}
	if _, exec := h.toolCommands[req.GetToolId()]; !exec {
		// 诚实标注：该工具只有解析器，本服务无法真实执行它（ADR-133）
		info.Name = p.ToolID() + " (parser only; no executor mapping)"
	}
	return info, nil
}

// ValidateToolConfig — proto L1028（校验工具可执行性）。
func (h *SASTAdapterHandler) ValidateToolConfig(ctx context.Context, req *pb.ValidateToolConfigRequest) (*pb.ValidateToolConfigResponse, error) {
	if req.GetToolId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_id is required")
	}
	resp := &pb.ValidateToolConfigResponse{Valid: true}
	tc, ok := h.toolCommands[req.GetToolId()]
	if !ok {
		resp.Valid = false
		resp.Errors = append(resp.Errors, fmt.Sprintf("no executor mapping for %s", req.GetToolId()))
		return resp, nil
	}
	if _, err := resolveToolArgv(tc.argv, "/tmp"); err != nil {
		resp.Valid = false
		resp.Errors = append(resp.Errors, err.Error())
	}
	return resp, nil
}

// GetScanProgress — proto L1030。诚实语义(2026-08-27 编造审计):
// 扫描为同步执行——有本任务落盘记录=已完成(100%); 无记录=未执行(0%, SCAN_STATUS_PENDING),
// 不冒充"已完成"。
func (h *SASTAdapterHandler) GetScanProgress(ctx context.Context, req *pb.GetScanProgressRequest) (*pb.ScanProgress, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	h.mu.Lock()
	tools := h.byTask[req.GetTaskId()]
	done := false
	if req.GetToolId() != "" {
		done = tools[req.GetToolId()]
	} else {
		for _, ran := range tools {
			if ran {
				done = true
				break
			}
		}
	}
	h.mu.Unlock()
	if done {
		return &pb.ScanProgress{TaskId: req.GetTaskId(), ToolId: req.GetToolId(),
			Percent: 100, Status: pb.ScanStatus_SCAN_STATUS_COMPLETED}, nil
	}
	return &pb.ScanProgress{TaskId: req.GetTaskId(), ToolId: req.GetToolId(),
		Percent: 0, Status: pb.ScanStatus_SCAN_STATUS_UNSPECIFIED}, nil // 未执行: 不冒充终态
}

// persistToResult — 09 §2 行 sast-adapter→result：BatchCreateFindings 落盘（幂等键隔离）。
// ADR-133: 落盘失败不再静默返回 0（此前扫描结果可能无声丢失）——错误上抛并留审计日志。
// ADR-137: 落盘超时来自全局配置 result.persist_timeout_s（09 §2 行 10s/3 的宽限口径）。
func persistToResult(resultAddr, taskID, requestID string, findings []*pb.UnifiedFinding) (int, error) {
	conn, err := grpcDial(resultAddr)
	if err != nil {
		return 0, fmt.Errorf("dial result-service: %w", err)
	}
	defer conn.Close()
	client := pb.NewResultServiceClient(conn)
	persistT, perr := cfgPersistTimeoutS()
	if perr != nil {
		return 0, perr
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(persistT)*time.Second)
	defer cancel()
	resp, err := client.BatchCreateFindings(ctx, &pb.BatchCreateFindingsRequest{
		Metadata: &pb.RequestMetadata{RequestId: requestID},
		Findings: findings,
	})
	if err != nil {
		return 0, fmt.Errorf("BatchCreateFindings: %w", err)
	}
	return len(resp.GetFindingIds()), nil
}

// cfgPersistTimeoutS — ADR-137: 全局配置 result.persist_timeout_s。
func cfgPersistTimeoutS() (int, error) {
	cfg, cerr := codeauditcfg.Default()
	if cerr != nil {
		return 0, fmt.Errorf("sast-adapter config: %w (ADR-137)", cerr)
	}
	v, cerr := cfg.Int("result.persist_timeout_s")
	if cerr != nil {
		return 0, fmt.Errorf("sast-adapter config: %w (ADR-137)", cerr)
	}
	return v, nil
}

// walkCodeFiles — 枚举目录下可扫代码文件（上限 maxFiles，防止超大仓库失控）。
func walkCodeFiles(root string, maxFiles int) []string {
	out := []string{}
	codeExt := map[string]bool{".py": true, ".js": true, ".ts": true, ".go": true, ".java": true,
		".rb": true, ".php": true, ".cs": true, ".c": true, ".cpp": true, ".h": true}
	skip := map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".toolchain": true}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if codeExt[strings.ToLower(filepath.Ext(path))] && len(out) < maxFiles {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// semgrepFilesFallback — 目录被 git 枚举吞掉时（本仓环境实测：scanned=[]），
// 改为对枚举出的代码文件分批扫描并合并 results（ADR-144）。
func (h *SASTAdapterHandler) semgrepFilesFallback(projectPath string, tool string) ([]byte, error) {
	tc := h.toolCommands[tool]
	if tc.rawFormat == "" {
		return nil, fmt.Errorf("no executor mapping for %s", tool)
	}
	files := walkCodeFiles(projectPath, 400)
	if len(files) == 0 {
		return nil, fmt.Errorf("no code files under %s", projectPath)
	}
	const chunk = 40
	merged := map[string]interface{}{"results": []interface{}{}}
	total := 0
	for s := 0; s < len(files); s += chunk {
		e := s + chunk
		if e > len(files) {
			e = len(files)
		}
		// 原 argv 去掉 {project} 占位、解析 {rules}（文件列表整体追加到尾部；首元素=工具二进制）
		rulesDir, rerr := findRulesDir()
		if rerr != nil {
			return nil, rerr
		}
		argv := make([]string, 0, len(tc.argv)+chunk)
		for _, a := range tc.argv {
			if strings.Contains(a, "{project}") {
				continue
			}
			argv = append(argv, strings.ReplaceAll(a, "{rules}", rulesDir))
		}
		argv = append(argv, files[s:e]...)
		ctx, cancel := context.WithTimeout(context.Background(), h.scanTimeout)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		if argv[0] == "python3" && tc.pythonPath != "" {
			cmd.Env = append(os.Environ(), "PYTHONPATH="+tc.pythonPath)
		}
		out, err := cmd.Output()
		cancel()
		log.Printf("[sast-adapter] fallback %s files=%d bytes=%d err=%v", tool, e-s, len(out), err)
		if err != nil && len(out) == 0 {
			continue
		}
		var part map[string]interface{}
		if json.Unmarshal(out, &part) == nil {
			if rs, ok := part["results"].([]interface{}); ok {
				existing := merged["results"].([]interface{})
				merged["results"] = append(existing, rs...)
				total += len(rs)
			}
		}
	}
	return json.Marshal(merged)
}

// ShareStore — 把 adapter 实体存储桥接进 fusion handler（同部署单元内共享，01 §4.2）。
// ADR-121: FuseResults 的 ID 引用在本地 store 未命中时由 adapterStore 补全。
func ShareStore(target *SASTFusionHandler, source *SASTAdapterHandler) {
	target.externalResolver = source.findingsOf
}

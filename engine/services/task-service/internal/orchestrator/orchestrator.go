// Package orchestrator — 模式驱动的真实编排执行器。
//
// 设计依据:
//   - 04 §3.1 模式A / §3.2 模式B / §3.3 模式C / §3.4 模式D（阶段划分与分流）
//   - 04 §2 Saga S1-S10（步骤编号与补偿语义）——失败补偿在 Execute 错误路径真实执行（ADR-131）:
//     S8"删除已存储结果"=逐条 ResultService.DeleteFinding（proto L924，唯一删除口径）。
//     S6"取消推理"/S9"重试投递"/S10"删除报告"缺 proto 支持（无 CancelAnalysis/DeleteReport/
//     Kafka 重投 RPC）→ 不发明（R1），设计缺口记录于 ADR-131，待 proto 演进。
//   - 09 §2 通信矩阵: task→dsh-runtime / task→sast-adapter / task→result；
//     findings 实体由 sast-adapter 与 dsh-runtime 各自落盘（行 sast-adapter→result、dsh-runtime→result），
//     编排层只传 ID 引用（proto ToolScanResult 定版口径：ID引用，不内嵌）。
//   - 步骤超时已全撤（ADR-191 补遗，人类指令"都撤掉"2026-09-03）：编排不再设任何
//     外层步骤时限，各步骤 ctx 直通（取消仍可传播）
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sync"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Config carries downstream endpoints.
// 端口依据: ADR-113(task=50054)/ADR-114(dsh=50057)/ADR-117(result=50058)/（ADR-175: ai=50056 已删）
// sast-adapter=50051(ADR-113 前身契约夹具约定端口)
type Config struct {
	SastAdapterAddr string
	DSHRuntimeAddr    string
	ResultAddr      string
}

// StageRecorder receives per-stage events for status board / progress.
type StageRecorder func(stage string, msg string)

// Orchestrator executes one scan task end-to-end following its scan mode.
type Orchestrator struct {
	cfg    Config
	mu     sync.Mutex
	events map[string][]string // taskID → stage log
}

func New(cfg Config) *Orchestrator {
	return &Orchestrator{cfg: cfg, events: map[string][]string{}}
}

// Events returns stage log lines for a task (E2E 断言用).
func (o *Orchestrator) Events(taskID string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events[taskID]...)
}

func (o *Orchestrator) record(taskID, stage, msg string) {
	log.Printf("[orchestrator][%s] %s: %s", taskID, stage, msg)
	o.mu.Lock()
	o.events[taskID] = append(o.events[taskID], fmt.Sprintf("%s: %s", stage, msg))
	o.mu.Unlock()
}

func dial(addr string) (*grpc.ClientConn, func(), error) {
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return conn, func() { _ = conn.Close() }, nil
}

func md(requestID string) *pb.RequestMetadata {
	return &pb.RequestMetadata{RequestId: requestID}
}

// findingCollector 收集已落盘的 finding ID（S8 补偿回滚清单）。
type findingCollector struct {
	mu  sync.Mutex
	ids []string
}

func (c *findingCollector) add(ids ...string) {
	c.mu.Lock()
	c.ids = append(c.ids, ids...)
	c.mu.Unlock()
}

func (c *findingCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

// RunRequest is the normalized input for Execute.
type RunRequest struct {
	TaskID      string
	RequestID   string
	ProjectID   string
	ProjectPath string
	ScanMode    pb.ScanMode
	SastTools   []string
	// Recorder 可选：阶段事件外部消费（task-service 阶段看板接线，ADR-131）
	Recorder StageRecorder
	// collect 可选：已落盘 finding ID 收集（S8 补偿回滚清单）
	collect func(ids ...string)
	// Prepare 可选：编排前置步骤（ADR-163 仓库拉取），返回实际 project_path。
	// 失败=编排失败，走既有 FAILED→QUEUED 重试→DEAD 链；在编排协程内执行，不阻塞 StartTask RPC。
	Prepare func(ctx context.Context) (string, error)
}

// Execute drives the full pipeline for the given scan mode and returns summary.
func (o *Orchestrator) Execute(ctx context.Context, r RunRequest) (map[string]interface{}, error) {
	summary := map[string]interface{}{}
	collector := &findingCollector{}
	r.collect = collector.add
	// ADR-163: 编排前置步骤（仓库拉取/上传件拉包，ADR-209 前缀中性化——旧"repo fetch"口径
	// 对 storage 通道误导排障）——失败即编排失败（走既有重试/DEAD 链）
	if r.Prepare != nil {
		p, perr := r.Prepare(ctx)
		if perr != nil {
			return nil, fmt.Errorf("prepare: %w", perr)
		}
		r.ProjectPath = p
	}
	stage := func(s, m string) {
		o.record(r.TaskID, s, m)
		if r.Recorder != nil {
			r.Recorder(s, m)
		}
	}

	stage("S1", "task accepted")

	var err error
	switch r.ScanMode {
	// ADR-186 五模式矩阵（人类决策 2026-09-03）：A=纯SAST / B=纯AI / C=并行融合(默认推荐) /
	// D=AI增强SAST / E=并行对比（ADR-182 前称"模式D"）
	case pb.ScanMode_SCAN_MODE_SAST_ONLY:
		err = o.runModeSastOnly(ctx, r, stage, summary)
	case pb.ScanMode_SCAN_MODE_AI_ONLY:
		err = o.runModeA(ctx, r, stage, summary)
	case pb.ScanMode_SCAN_MODE_AI_ENHANCED_SAST:
		err = o.runModeDEnhanced(ctx, r, stage, summary)
	case pb.ScanMode_SCAN_MODE_PARALLEL, pb.ScanMode_SCAN_MODE_COMPARE:
		err = o.runModeParallel(ctx, r, stage, summary)
	case pb.ScanMode_SCAN_MODE_TRADITIONAL_FIRST:
		err = o.runModeB(ctx, r, stage, summary) // 已弃用（ADR-182）：历史任务兼容分支
	case pb.ScanMode_SCAN_MODE_SAST_REVIEW:
		err = o.runModeD(ctx, r, stage, summary) // 已弃用（ADR-182）：历史任务兼容分支
	default:
		err = fmt.Errorf("unsupported scan_mode: %s (ADR-186 五模式)", r.ScanMode)
	}
	if err != nil {
		// 04 §2 S8 失败补偿：删除本任务已落盘的发现（真实 gRPC 回滚，ADR-131）
		o.compensateFindings(r, collector.snapshot(), stage)
		return summary, err
	}

	// 收尾统计：以 result-service 权威口径核对落盘数量（09 §2 / proto L1242 ResultStats）
	if stats, serr := o.resultStats(r.TaskID); serr == nil {
		summary["by_verdict"] = stats.GetByVerdict()
		summary["by_severity"] = stats.GetBySeverity()
		summary["by_cwe"] = stats.GetByCwe()
		total := int32(0)
		for _, n := range stats.GetByVerdict() {
			total += n
		}
		summary["stored_total"] = total
	}
	return summary, nil
}

// compensateFindings — 04 §2 S8 补偿：删除已存储结果。
// DeleteFinding 逐条幂等回滚；NotFound 视为已清理。S6/S9/S10 补偿缺 proto 支持（ADR-131）。
func (o *Orchestrator) compensateFindings(r RunRequest, ids []string, stage StageRecorder) {
	if len(ids) == 0 {
		return
	}
	conn, closeFn, err := dial(o.cfg.ResultAddr)
	if err != nil {
		o.record(r.TaskID, "S8-compensate", "dial result-service failed, findings not rolled back: "+err.Error())
		return
	}
	defer closeFn()
	client := pb.NewResultServiceClient(conn)
	deleted, missed := 0, 0
	for _, id := range ids {
		dCtx, dCancel := context.Background(), context.CancelFunc(func() {})
		defer dCancel()
		if _, err := client.DeleteFinding(dCtx,
			&pb.DeleteFindingRequest{FindingId: id}); err != nil {
			if status.Code(err) != status.Code(nil) && status.Code(err).String() == "NotFound" {
				deleted++ // 已不存在=补偿目标已达成
				continue
			}
			missed++
			continue
		}
		deleted++
	}
	o.record(r.TaskID, "S8-compensate", fmt.Sprintf("deleted=%d missed=%d of %d", deleted, missed, len(ids)))
}

// ---- 模式A（ADR-182 重排）: 纯SAST ----
// 多 SAST 工具并行审计（sast-adapter RunMultipleScans 并行执行）→ FuseResults
// 跨工具去重合并（AI 集为空，仅用融合管线的合并/去重/分组段）→ 报告。
// 无 AI 环节；SAST 全部失败=任务失败（唯一内容来源）。
func (o *Orchestrator) runModeSastOnly(ctx context.Context, r RunRequest, stage StageRecorder, summary map[string]interface{}) error {
	sastIDs, allFailed := o.runMultipleScans(ctx, r, stage)
	if allFailed {
		return fmt.Errorf("stage RunMultipleScans produced no results (模式A 纯SAST 唯一内容来源, ADR-182)")
	}
	summary["sast_finding_ids"] = sastIDs
	summary["sast_findings"] = len(sastIDs)
	r.collect(sastIDs...)

	// 跨工具去重合并（AI 集为空，仅用融合管线的合并/去重/分组段）
	o.fuseAndSummarize(ctx, r, stage, summary, sastIDs, nil)
	return o.generateReport(ctx, r, "S9-report", stage)
}

// fuseAndSummarize — 融合去重调用+降级语义+summary 汇总的共享段（ADR-182 补遗②：
// 此块在模式A/模式C 重复，抽取共同调用；07 §10 融合失败→原始结果保留，任务不失败）。
func (o *Orchestrator) fuseAndSummarize(ctx context.Context, r RunRequest, stage StageRecorder, summary map[string]interface{}, sastIDs, aiIDs []string) {
	fusionResp, ferr := o.fuseResults(ctx, r, sastIDs, aiIDs, stage)
	if ferr != nil {
		stage("S7", "fusion degraded, raw results kept: "+ferr.Error())
		summary["fusion_degraded"] = true
		return
	}
	fr := fusionResp.GetResult()
	summary["fusion_total"] = fr.GetTotalCount()
	summary["fusion_removed_fp"] = fr.GetRemovedFalsePositives()
	summary["fusion_added_ai"] = fr.GetAddedAiFindings()
	summary["fusion_merged"] = fr.GetMetrics().GetMergedCount()
}

// ---- 模式B（ADR-182 重排，原"模式A"流程不变）: 纯AI ----
// 阶段2 AnalyzeCode → 阶段3 RunAIAnalysis（五Agent，LLM/RuleScan降级）→
// 阶段4 落盘(dsh-runtime已直写) → 阶段5 报告(S9 Kafka主路径/gRPC降级)
func (o *Orchestrator) runModeA(ctx context.Context, r RunRequest, stage StageRecorder, summary map[string]interface{}) error {
	cpgPath, _ := o.analyzeCode(ctx, r, stage)
	summary["cpg_storage_path"] = cpgPath

	res, fixN, aiIDs, err := o.runAIAnalysis(ctx, r, nil, stage)
	if err != nil {
		return err
	}
	summary["ai_findings"] = res.GetAiFindingsCount()
	summary["verified"] = res.GetVerifiedCount()
	summary["fix_suggestions"] = fixN
	summary["ai_finding_ids"] = aiIDs
	r.collect(aiIDs...)

	return o.generateReport(ctx, r, "S9-report", stage)
}

// ---- 模式B（旧·已弃用 ADR-182）: 04 §3.2 SAST→AI增强 ----
// 仅历史任务兼容（SCAN_MODE_TRADITIONAL_FIRST 不再由 UI 提供）；4a/4b/融合链路保留。
// [2a]RunMultipleScans ∥ [2b]AnalyzeCode → S5汇合 → 4a Verify → 4b Missed →
// 4d Verdict回写 → S7 FuseResults → S9 报告
// S5 部分结果语义（04 §2）: 至少一侧有产出才继续；两侧皆败 → 任务失败（ADR-131）。
func (o *Orchestrator) runModeB(ctx context.Context, r RunRequest, stage StageRecorder, summary map[string]interface{}) error {
	var (
		wg                        sync.WaitGroup
		sastIDs                   []string
		cpgPath                   string
		sastAllFailed, cpgDegrade bool
	)

	wg.Add(2)
	go func() { defer wg.Done(); sastIDs, sastAllFailed = o.runMultipleScans(ctx, r, stage) }()
	go func() { defer wg.Done(); cpgPath, cpgDegrade = o.analyzeCode(ctx, r, stage) }()
	wg.Wait() // S5 并行汇合

	if sastAllFailed && cpgDegrade {
		return fmt.Errorf("stage2 both branches failed: RunMultipleScans produced no results and AnalyzeCode degraded (04 §2 S5)")
	}
	if sastAllFailed {
		stage("S5", "partial results: SAST side failed, continuing with CPG side (04 §2 S5)")
		summary["sast_degraded"] = true
	}
	if cpgDegrade {
		summary["cpg_degraded"] = true
	}
	stage("S5", "parallel join done (2a SAST + 2b CPG)")
	summary["cpg_storage_path"] = cpgPath
	summary["sast_finding_ids"] = sastIDs
	summary["sast_findings"] = len(sastIDs)
	r.collect(sastIDs...)

	// 4a 逐条验证（语义判据: 读代码→数据流→净化器→可达性；实际形态见 dsh-runtime:
	// 沙箱内 DSH 整项目逐条审查（ADR-173），沙箱不可用如实降级 NEEDS_MANUAL（07 §10）
	// — 04 §3.2 ★修订注 / ADR-176）
	verifiedResp, err := o.verifySASTResults(ctx, r, sastIDs, stage)
	if err != nil {
		return err
	}
	tpFP := map[string]int{}
	for _, v := range verifiedResp.GetVerified() {
		key := v.GetVerdict().String()
		tpFP[key]++
	}
	summary["verified_by_verdict"] = tpFP

	// 4b 漏报搜索（source_tool=ai_agent 新增发现；实体由 dsh-runtime 直写 result-service）
	missed, err := o.searchMissedVulns(ctx, r, stage)
	if err != nil {
		return err
	}
	missedIDs := make([]string, 0, len(missed.GetMissedFindings()))
	for _, f := range missed.GetMissedFindings() {
		missedIDs = append(missedIDs, f.GetFindingId())
	}
	summary["missed_findings"] = len(missedIDs)
	summary["ai_finding_ids"] = missedIDs
	r.collect(missedIDs...)
	stage("done:ai", fmt.Sprintf("4a+4b settled: verified=%d missed=%d", len(verifiedResp.GetVerified()), len(missedIDs)))

	// 4d 结论回写（幂等键隔离；失败不中断——07 §10）
	if uerr := o.batchUpdateVerdict(ctx, r, verifiedResp.GetVerified(), stage); uerr != nil {
		stage("S8", "BatchUpdateVerdict degraded: "+uerr.Error())
	} else {
		summary["verdict_updated"] = len(verifiedResp.GetVerified())
	}

	// 阶段5 融合: 过滤误报→合并→去重对齐→置信度融合（融合所需实体由服务端解析，
	// 本地未命中回退 result-service GetFinding — 见 sast-adapter fusion 服务端说明/ADR-120）
	fusionResp, ferr := o.fuseResults(ctx, r, sastIDs, missedIDs, stage)
	if ferr != nil {
		// 07 §10: 融合失败→原始结果保留，任务不失败
		stage("S7", "fusion degraded, raw results kept: "+ferr.Error())
	} else {
		fr := fusionResp.GetResult()
		summary["fusion_total"] = fr.GetTotalCount()
		summary["fusion_removed_fp"] = fr.GetRemovedFalsePositives()
		summary["fusion_added_ai"] = fr.GetAddedAiFindings()
		summary["fusion_merged"] = fr.GetMetrics().GetMergedCount()
	}

	return o.generateReport(ctx, r, "S9-report", stage)
}

// ---- 模式C/模式D（ADR-182）: SAST+AI 并行审计 → 按 C/D 选择输出形态 ----
// [SAST 工具组] ∥ [AI 语义审计] 各自独立完成、互不交叉（并行段内联于此，C/D 共用）：
//
//	模式C（SCAN_MODE_PARALLEL，默认推荐）→ FuseResults 跨源去重合并为单一清单；
//	模式D（SCAN_MODE_COMPARE）→ CompareResults 单SAST/单AI/SAST+AI 三分桶同维度对比。
//
// 两侧皆败 → 任务失败（部分结果语义同 04 §2 S5）；单侧降级如实标注继续。
// （ADR-182 补遗：原 runModeParallelFusion/runModeCompare/parallelAudit 三函数
//
//	函数体 90% 重复，合并为这一个——并行段无独立调用方，拆层无价值。）
func (o *Orchestrator) runModeParallel(ctx context.Context, r RunRequest, stage StageRecorder, summary map[string]interface{}) error {
	var (
		wg            sync.WaitGroup
		sastIDs       []string
		aiIDs         []string
		fixN          int
		aiCount       int32
		sastAllFailed bool
		aiErr         error
	)

	wg.Add(2)
	go func() { defer wg.Done(); sastIDs, sastAllFailed = o.runMultipleScans(ctx, r, stage) }()
	go func() {
		defer wg.Done()
		res, fx, ids, err := o.runAIAnalysis(ctx, r, nil, stage)
		if err != nil {
			aiErr = err
			return
		}
		aiIDs, fixN, aiCount = ids, fx, res.GetAiFindingsCount()
	}()
	wg.Wait()

	if sastAllFailed && aiErr != nil {
		return fmt.Errorf("parallel audit both sides failed: RunMultipleScans produced no results and RunAIAnalysis errored (04 §2 S5)")
	}
	if sastAllFailed {
		stage("S5", "partial results: SAST side failed (04 §2 S5)")
		summary["sast_degraded"] = true
	}
	if aiErr != nil {
		stage("S5", "partial results: AI side failed (04 §2 S5): "+aiErr.Error())
		summary["ai_degraded"] = true
	}
	summary["sast_finding_ids"] = sastIDs
	summary["ai_finding_ids"] = aiIDs
	summary["sast_findings"] = len(sastIDs)
	summary["ai_findings"] = aiCount
	summary["fix_suggestions"] = fixN
	r.collect(sastIDs...)
	r.collect(aiIDs...)

	if r.ScanMode == pb.ScanMode_SCAN_MODE_COMPARE {
		// 模式D：三分桶对比（互为参照指标；对比即产物，不合并清单）
		cmp, cerr := o.compareResults(ctx, r, sastIDs, aiIDs, stage)
		if cerr != nil {
			return cerr
		}
		summary["comparison"] = cmp
		return o.generateReport(ctx, r, "S9-report", stage)
	}

	// 模式C：融合去重（SAST+AI 跨源：过滤误报→合并→去重对齐→置信度融合）
	o.fuseAndSummarize(ctx, r, stage, summary, sastIDs, aiIDs)
	return o.generateReport(ctx, r, "S9-report", stage)
}

// ---- 模式D（ADR-186）：AI增强SAST ----
// 依据: 04 §3.4（ADR-186 修订后）。SAST 多工具扫描（模式A 的内容来源段）→
// VerifySASTResults 逐条沙箱验证（dsh-runtime 侧先做同文件同段去重：一组一轮沙箱，
// 判定广播回同段全部发现，ADR-186）→ BatchUpdateVerdict 回写 AI 判定 →
// FuseResults 汇总（误报过滤段此时已能吃到 ai_verdict）→ 报告。
// 边界：不调 AnalyzeCode、不做漏报搜索（与旧模式B 的差异）；SAST 全部失败=任务失败
// （唯一内容来源，模式A 口径）；沙箱不可用时 dsh-runtime 如实降级全批 NEEDS_MANUAL
// （07 §10），任务不失败。
func (o *Orchestrator) runModeDEnhanced(ctx context.Context, r RunRequest, stage StageRecorder, summary map[string]interface{}) error {
	sastIDs, allFailed := o.runMultipleScans(ctx, r, stage)
	if allFailed {
		return fmt.Errorf("stage RunMultipleScans produced no results (模式D AI增强SAST: SAST扫描为唯一内容来源, ADR-186)")
	}
	summary["sast_finding_ids"] = sastIDs
	summary["sast_findings"] = len(sastIDs)
	r.collect(sastIDs...)

	// 阶段3 逐条沙箱验证（同段去重在 dsh-runtime 侧执行，ADR-186）
	verifiedResp, err := o.verifySASTResults(ctx, r, sastIDs, stage)
	if err != nil {
		return err
	}
	tpFP := map[string]int{}
	for _, v := range verifiedResp.GetVerified() {
		key := v.GetVerdict().String()
		tpFP[key]++
	}
	summary["verified_by_verdict"] = tpFP
	stage("done:ai", fmt.Sprintf("verify settled: verified=%d", len(verifiedResp.GetVerified()))) // ADR-181: 实时完成

	// 阶段4 AI 判定回写（幂等键隔离；失败不中断——07 §10，同旧模式B 4d 口径）
	if uerr := o.batchUpdateVerdict(ctx, r, verifiedResp.GetVerified(), stage); uerr != nil {
		stage("S8", "BatchUpdateVerdict degraded: "+uerr.Error())
	} else {
		summary["verdict_updated"] = len(verifiedResp.GetVerified())
	}

	// 阶段5 融合汇总（AI 集为空——AI 的产出是判定而非新发现；过滤段消费已回写的 ai_verdict）
	o.fuseAndSummarize(ctx, r, stage, summary, sastIDs, nil)

	// 阶段6 报告
	return o.generateReport(ctx, r, "S9-report", stage)
}

// ---- 旧模式D（SAST_REVIEW，已弃用 ADR-182）: 04 §3.6 SAST→AI审核 ----
// 仅历史任务兼容（SCAN_MODE_SAST_REVIEW 不再由 UI 提供）；ReviewSASTResults 能力保留。
// ADR-186 起"模式D"指 AI增强SAST（runModeDEnhanced），本函数为旧模式D 历史兼容分支。
// 阶段2 RunMultipleScans → 阶段3 ReviewSASTResults（整体评估+逐条审核+汇总）→ 阶段4 报告。
// 边界：不调 SearchMissedVulns、不做融合。SAST 全部失败 → 任务失败（无审核对象）。
func (o *Orchestrator) runModeD(ctx context.Context, r RunRequest, stage StageRecorder, summary map[string]interface{}) error {
	sastIDs, sastAllFailed := o.runMultipleScans(ctx, r, stage)
	if sastAllFailed {
		return fmt.Errorf("stage2 RunMultipleScans produced no results (04 §3.4 阶段2为唯一内容来源)")
	}
	summary["sast_finding_ids"] = sastIDs
	summary["sast_findings"] = len(sastIDs)
	r.collect(sastIDs...)

	reviewResp, err := o.reviewSASTResults(ctx, r, sastIDs, stage)
	if err != nil {
		return err
	}
	if rep := reviewResp.GetReport(); rep != nil {
		summary["review_quality_score"] = rep.GetOverall().GetQualityScore()
		summary["review_conclusion"] = rep.GetOverall().GetConclusion().String()
		summary["review_count"] = len(rep.GetReviews())
		opinions := map[string]int32{}
		for k, v := range rep.GetStats().GetByOpinion() {
			opinions[k] = v
		}
		summary["review_by_opinion"] = opinions
	}

	return o.generateReport(ctx, r, "S4-report", stage)
}

// ================= 下游调用封装（09 §2 通信矩阵逐行对应） =================

// analyzeCode — task→dsh-runtime CodeAnalysisService.AnalyzeCode（04 §3.x 阶段2b/模式A阶段2）。
// 返回 (cpg路径, 是否降级)。04 §6 降级策略：CPG 失败→AST 降级语义，不阻断流程。
func (o *Orchestrator) analyzeCode(ctx context.Context, r RunRequest, stage StageRecorder) (string, bool) {
	stage("analyze", "submitting AnalyzeCode") // ADR-181: 阶段开始即 RUNNING（时间线中间态）
	conn, closeFn, err := dial(o.cfg.DSHRuntimeAddr)
	if err != nil {
		o.record(r.TaskID, "analyze", "dial dsh-runtime failed (degraded): "+err.Error())
		stage("done:analyze", "dial failed (degraded)")
		return "", true
	}
	defer closeFn()

	client := pb.NewCodeAnalysisServiceClient(conn)
	// 正式口径 07 §8 AnalyzeCode 10m；本地小项目毫秒级，取 60s 上界
	analyzeCtx, analyzeCancel := ctx, context.CancelFunc(func() {})
	defer analyzeCancel()
	resp, err := client.AnalyzeCode(analyzeCtx, &pb.AnalyzeCodeRequest{
		Metadata:    md(r.RequestID + "-analyze"),
		TaskId:      r.TaskID,
		ProjectPath: r.ProjectPath,
	})
	if err != nil {
		// 04 §6 降级策略：CPG 失败→AST 降级语义，不阻断流程
		stage("analyze", "AnalyzeCode degraded: "+err.Error())
		stage("done:analyze", "degraded to AST semantics")
		return "", true
	}
	stage("analyze", "AnalyzeCode ok files="+fmt.Sprint(len(resp.GetResult().GetFiles())))
	stage("done:analyze", "cpg ready")
	return resp.GetResult().GetCpgStoragePath(), false
}

// runMultipleScans — task→sast-adapter RunMultipleScans（04 §3.2 阶段2a/模式D阶段2）。
// 返回 (finding_ids, 是否全部失败)。实体由 sast-adapter 落盘 result-service（09 §2 行）。
// "全部失败"= RPC 级失败或全部工具结果 SCAN_STATUS_FAILED（ADR-131：区分"全失败"与"扫出0条"）。
func (o *Orchestrator) runMultipleScans(ctx context.Context, r RunRequest, stage StageRecorder) ([]string, bool) {
	stage("scans", fmt.Sprintf("submitting RunMultipleScans tools=%v", r.SastTools)) // ADR-181: 阶段开始即 RUNNING
	conn, closeFn, err := dial(o.cfg.SastAdapterAddr)
	if err != nil {
		o.record(r.TaskID, "scans", "dial sast-adapter failed: "+err.Error())
		return nil, true
	}
	defer closeFn()

	client := pb.NewSASTAdapterServiceClient(conn)
	// 正式口径 07 §8 RunMultipleScans 20m；本地样本取 120s 上界
	scanCtx, scanCancel := ctx, context.CancelFunc(func() {})
	defer scanCancel()
	resp, err := client.RunMultipleScans(scanCtx, &pb.RunMultipleScansRequest{
		Metadata:    md(r.RequestID + "-scans"),
		TaskId:      r.TaskID,
		ProjectPath: r.ProjectPath,
		ToolIds:     r.SastTools,
	})
	if err != nil {
		o.record(r.TaskID, "scans", "RunMultipleScans failed: "+err.Error())
		return nil, true
	}

	ids := []string{}
	failedTools, totalTools := 0, 0
	for tool, tsr := range resp.GetResult().GetResults() {
		totalTools++
		if tsr.GetStatus() == pb.ScanStatus_SCAN_STATUS_FAILED {
			failedTools++
		}
		stage("scans", fmt.Sprintf("tool=%s status=%s findings=%d",
			tool, tsr.GetStatus(), len(tsr.GetFindingIds())))
		ids = append(ids, tsr.GetFindingIds()...)
	}
	allFailed := totalTools == 0 || failedTools == totalTools
	if !allFailed {
		stage("done:sast", fmt.Sprintf("%d findings from %d tools", len(ids), totalTools)) // ADR-181: 实时完成（全失败留给 finalize 落 FAILED）
	}
	return ids, allFailed
}

// runAIAnalysis — task→dsh-runtime RunAIAnalysis（04 §3.1 阶段3 / 3.3 阶段2b）。
// 返回 (result, fix建议数, ai_finding_ids)。AI新增发现实体由 dsh-runtime 直写 result-service。
func (o *Orchestrator) runAIAnalysis(ctx context.Context, r RunRequest, extraIDs []string, stage StageRecorder) (*pb.AIInferenceResult, int, []string, error) {
	conn, closeFn, err := dial(o.cfg.DSHRuntimeAddr)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("dial dsh-runtime: %w", err)
	}
	defer closeFn()

	client := pb.NewDSHRuntimeServiceClient(conn)
	// ADR-191（人类指令"撤掉超时安全网"）：AI 审计回合不再设外层时限——深度分析
	// （批量行号核验/补丁撰写）实测 30m 仍健康流式（gw-9b602266 实证：模型已在
	// "assembling and submitting findings"一步被 30m 网掐死→fail→auto-retry 从头
	// 重跑，30 分钟分析全毁）。回合结束只认 DSH 沙箱流式显式信号
	// （session.status=idle / turn/end / runtime.exit）；断流自愈=SSE streamErr→
	// teardown→降级链（07 §10）。cancel 保留 no-op 以维持调用面不变。
	aiCtx, aiCancel := ctx, context.CancelFunc(func() {})
	defer aiCancel()
	stage("ai", "submitting RunAIAnalysis") // ADR-181: 阶段开始即 RUNNING
	resp, err := client.RunAIAnalysis(aiCtx, &pb.RunAIAnalysisRequest{
		Metadata:       md(r.RequestID + "-ai"),
		TaskId:         r.TaskID,
		ProjectPath:    r.ProjectPath,
		SastFindingIds: extraIDs,
		ScanMode:       r.ScanMode,
	})
	if err != nil {
		return nil, 0, nil, fmt.Errorf("RunAIAnalysis: %w", err)
	}
	res := resp.GetResult()
	stage("ai", fmt.Sprintf("ai_findings=%d verified=%d fp=%d sug=%d",
		res.GetAiFindingsCount(), res.GetVerifiedCount(), res.GetFalsePositiveCount(), len(resp.GetFixSuggestions())))
	stage("done:ai", "RunAIAnalysis settled") // ADR-181: 实时完成
	return res, len(resp.GetFixSuggestions()), res.GetAiFindingIds(), nil
}

// verifySASTResults — task→dsh-runtime VerifySASTResults（04 §3.2 阶段4a）
func (o *Orchestrator) verifySASTResults(ctx context.Context, r RunRequest, ids []string, stage StageRecorder) (*pb.VerifySASTResultsResponse, error) {
	if len(ids) == 0 {
		stage("verify", "no SAST findings to verify (degraded side), skipping 4a")
		return &pb.VerifySASTResultsResponse{}, nil
	}
	conn, closeFn, err := dial(o.cfg.DSHRuntimeAddr)
	if err != nil {
		return nil, fmt.Errorf("dial dsh-runtime: %w", err)
	}
	defer closeFn()
	client := pb.NewDSHRuntimeServiceClient(conn)
	vCtx, vCancel := ctx, context.CancelFunc(func() {})
	defer vCancel()
	stage("verify", fmt.Sprintf("submitting %d findings for sandbox review", len(ids))) // ADR-181: 4a 开始即 RUNNING
	resp, err := client.VerifySASTResults(vCtx, &pb.VerifySASTResultsRequest{
		Metadata:    md(r.RequestID + "-verify"),
		TaskId:      r.TaskID,
		FindingIds:  ids,
		ProjectPath: r.ProjectPath, // ADR-173: 沙箱内整项目审查
	})
	if err != nil {
		return nil, fmt.Errorf("VerifySASTResults: %w", err)
	}
	stage("verify", fmt.Sprintf("%d findings verified", len(resp.GetVerified())))
	return resp, nil
}

// searchMissedVulns — task→dsh-runtime SearchMissedVulns（04 §3.2 阶段4b）
func (o *Orchestrator) searchMissedVulns(ctx context.Context, r RunRequest, stage StageRecorder) (*pb.SearchMissedVulnsResponse, error) {
	conn, closeFn, err := dial(o.cfg.DSHRuntimeAddr)
	if err != nil {
		return nil, fmt.Errorf("dial dsh-runtime: %w", err)
	}
	defer closeFn()
	client := pb.NewDSHRuntimeServiceClient(conn)
	mCtx, mCancel := ctx, context.CancelFunc(func() {})
	defer mCancel()
	stage("missed", "submitting full-project sandbox audit") // ADR-181: 4b 开始即 RUNNING
	resp, err := client.SearchMissedVulns(mCtx, &pb.SearchMissedVulnsRequest{
		Metadata:    md(r.RequestID + "-missed"),
		TaskId:      r.TaskID,
		ProjectPath: r.ProjectPath, // ADR-165: 契约新增字段, 修复此前靠环境变量的断线
	})
	if err != nil {
		return nil, fmt.Errorf("SearchMissedVulns: %w", err)
	}
	stage("missed", fmt.Sprintf("%d missed findings (source_tool=ai_agent)", len(resp.GetMissedFindings())))
	return resp, nil
}

// reviewSASTResults — task→dsh-runtime ReviewSASTResults（04 §3.4 阶段3）
func (o *Orchestrator) reviewSASTResults(ctx context.Context, r RunRequest, ids []string, stage StageRecorder) (*pb.ReviewSASTResultsResponse, error) {
	conn, closeFn, err := dial(o.cfg.DSHRuntimeAddr)
	if err != nil {
		return nil, fmt.Errorf("dial dsh-runtime: %w", err)
	}
	defer closeFn()
	client := pb.NewDSHRuntimeServiceClient(conn)
	rCtx, rCancel := ctx, context.CancelFunc(func() {})
	defer rCancel()
	stage("review", fmt.Sprintf("submitting %d findings for review", len(ids))) // ADR-181: 审核开始即 RUNNING
	resp, err := client.ReviewSASTResults(rCtx, &pb.ReviewSASTResultsRequest{
		Metadata:       md(r.RequestID + "-review"),
		TaskId:         r.TaskID,
		ProjectPath:    r.ProjectPath,
		SastFindingIds: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("ReviewSASTResults: %w", err)
	}
	stage("review", fmt.Sprintf("reviews=%d", len(resp.GetReport().GetReviews())))
	stage("done:review", "review settled") // ADR-181: 实时完成
	return resp, nil
}

// fuseResults — task→sast-adapter FuseResults（04 §3.2 阶段5；幂等）
func (o *Orchestrator) fuseResults(ctx context.Context, r RunRequest, sastIDs, aiIDs []string, stage StageRecorder) (*pb.FuseResultsResponse, error) {
	conn, closeFn, err := dial(o.cfg.SastAdapterAddr)
	if err != nil {
		return nil, fmt.Errorf("dial sast-adapter: %w", err)
	}
	defer closeFn()
	client := pb.NewSASTFusionServiceClient(conn)
	fCtx, fCancel := ctx, context.CancelFunc(func() {})
	defer fCancel()
	stage("fusion", "submitting FuseResults") // ADR-181: 融合开始即 RUNNING（此前成功路径无任何阶段事件）
	resp, err := client.FuseResults(fCtx, &pb.FuseResultsRequest{
		Metadata:       md(r.RequestID + "-fuse"),
		TaskId:         r.TaskID,
		SastFindingIds: sastIDs,
		AiFindingIds:   aiIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("FuseResults: %w", err)
	}
	stage("done:fusion", "fusion settled") // ADR-181: 实时完成
	return resp, nil
}

// compareResults — task→sast-adapter CompareResults（04 §3.3 阶段3；proto L1056）
func (o *Orchestrator) compareResults(ctx context.Context, r RunRequest, sastIDs, aiIDs []string, stage StageRecorder) (map[string]int32, error) {
	conn, closeFn, err := dial(o.cfg.SastAdapterAddr)
	if err != nil {
		return nil, fmt.Errorf("dial sast-adapter: %w", err)
	}
	defer closeFn()
	client := pb.NewSASTFusionServiceClient(conn)
	cCtx, cCancel := ctx, context.CancelFunc(func() {})
	defer cCancel()
	stage("compare", "submitting CompareResults") // ADR-181: 对比开始即 RUNNING
	resp, err := client.CompareResults(cCtx, &pb.CompareResultsRequest{
		TaskId:         r.TaskID,
		SastFindingIds: sastIDs,
		AiFindingIds:   aiIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("CompareResults: %w", err)
	}
	sum := resp.GetSummary()
	out := map[string]int32{
		"sast_total":   sum.GetSastTotal(),
		"ai_total":     sum.GetAiTotal(),
		"both_found":   sum.GetBothFound(),
		"sast_only":    sum.GetSastOnly(),
		"ai_only":      sum.GetAiOnly(),
		"disagreement": sum.GetDisagreement(),
	}
	stage("compare", fmt.Sprintf("both=%d sast_only=%d ai_only=%d disagreement=%d",
		out["both_found"], out["sast_only"], out["ai_only"], out["disagreement"]))
	stage("done:fusion", "comparison settled") // ADR-181: 对比完成（fusion 看板位）
	return out, nil
}

// batchUpdateVerdict — task→result BatchUpdateVerdict（04 §3.2 阶段4d）。
// 按真实 (verdict, confidence) 分组回写（ADR-131：废除写死 0.8，采信 VerifiedFinding.confidence）。
func (o *Orchestrator) batchUpdateVerdict(ctx context.Context, r RunRequest, verified []*pb.VerifiedFinding, stage StageRecorder) error {
	if len(verified) == 0 {
		return nil
	}
	conn, closeFn, err := dial(o.cfg.ResultAddr)
	if err != nil {
		return fmt.Errorf("dial result-service: %w", err)
	}
	defer closeFn()
	client := pb.NewResultServiceClient(conn)

	// proto BatchUpdateVerdictRequest 单值 verdict/confidence 语义 → 按 (verdict, confidence) 分组
	type vkey struct {
		v pb.AIVerdict
		c float32
	}
	groups := map[vkey][]string{}
	order := []vkey{}
	for _, v := range verified {
		k := vkey{v.GetVerdict(), v.GetConfidence()}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], v.GetOriginalFindingId())
	}
	for _, k := range order {
		ids := groups[k]
		vvCtx, vvCancel := ctx, context.CancelFunc(func() {})
		defer vvCancel()
		_, err := client.BatchUpdateVerdict(vvCtx, &pb.BatchUpdateVerdictRequest{
			Metadata:   md(fmt.Sprintf("%s-verdict-%s-%v", r.RequestID, k.v, k.c)),
			FindingIds: ids,
			Verdict:    k.v,
			Confidence: k.c,
		})
		if err != nil {
			return err
		}
	}
	stage("verdict", fmt.Sprintf("updated in %d groups", len(groups)))
	return nil
}

// generateReport — S9 Kafka 主路径不可达时的 gRPC 降级直调（ADR-006；R4 幂等防重复）
func (o *Orchestrator) generateReport(ctx context.Context, r RunRequest, stageName string, stage StageRecorder) error {
	conn, closeFn, err := dial(o.cfg.ResultAddr)
	if err != nil {
		return fmt.Errorf("dial result-service: %w", err)
	}
	defer closeFn()
	client := pb.NewReportServiceClient(conn)
	gCtx, gCancel := ctx, context.CancelFunc(func() {})
	defer gCancel()
	stage(stageName, "submitting GenerateReport") // ADR-181: 报告开始即 RUNNING
	resp, err := client.GenerateReport(gCtx, &pb.GenerateReportRequest{
		Metadata: md(r.RequestID + "-report"),
		TaskId:   r.TaskID,
		Format:   pb.ReportFormat_REPORT_FORMAT_JSON,
	})
	if err != nil {
		// 04 §6: Kafka/gRPC 双路径均失败→记录降级，任务保持部分结果语义
		stage(stageName, "GenerateReport degraded: "+err.Error())
		return nil
	}
	stage(stageName, "report generated: "+resp.GetResult().GetReportId()+" url="+resp.GetResult().GetReportUrl())
	stage("done:report", "report settled") // ADR-181: 实时完成
	return nil
}

// resultStats — 编排收尾从 result-service 反查权威统计（E2E 断言锚点）
func (o *Orchestrator) resultStats(taskID string) (*pb.ResultStats, error) {
	conn, closeFn, err := dial(o.cfg.ResultAddr)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	client := pb.NewResultServiceClient(conn)
	sCtx, sCancel := context.Background(), context.CancelFunc(func() {})
	defer sCancel()
	return client.GetTaskResultStats(sCtx, &pb.GetTaskResultStatsRequest{TaskId: taskID})
}

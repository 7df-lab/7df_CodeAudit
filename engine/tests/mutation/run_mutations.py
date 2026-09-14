#!/usr/bin/env python3
"""变异自检 — engine 反回归机制的执行器（REGRESSIONS.md 第三层）。

原理：把历史 bug 等价地"再引入"源文件的临时修改（改前字节级备份，finally 恢复），
跑对应的锁定测试，断言测试必须变红。测试套件若被弱化/删除/改名，或源码重构后
锚点失配，本脚本立刻非零退出——「门禁绿」因此自证牙齿还在。

维护规则（与 REGRESSIONS.md 台账联动）：
  1. 修一个 bug = 台账登记一行 + 回归测试 + 在 MUTANTS 追加一条变异（可杀它的 pattern）；
  2. 源码重构使 find 锚点失配 = 锚点恰配检查红，必须同步更新变异条目再谈门禁；
  3. 变异必须保持可编译——"编译失败杀掉测试"不是牙齿（kill 判据排除 build failed）。

用法：
  python3 tests/mutation/run_mutations.py            # 全量（门禁入口）
  python3 tests/mutation/run_mutations.py --check    # 仅锚点恰配检查（无 go 也能跑）
  python3 tests/mutation/run_mutations.py M9 M13     # 只跑指定变异（调试用）

安全：目标文件若有未提交改动（git）则拒绝执行（防覆盖在途工作）；
      每条变异改前备份、finally 恢复并复核字节一致。
"""

import os
import shutil
import subprocess
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))

# ---------------------------------------------------------------------------
# 变异注册表：每条 = 历史 bug 的等价再引入。
#   file     仓库根相对路径；edits [(find, replace)...]（find 必须在文件中恰出现 1 次）
#   module   go module 目录（相对仓库根）；run 为锁定测试的 -run pattern
# ---------------------------------------------------------------------------
MUTANTS = [
    {
        # R47 重构（2026-09-11）后锚点同步：解析链 RPC 移锁外，任务级/项目级守卫收敛进
        # needProjectLookup（r.Prepare==nil 语义保留），repo 兜底分支守卫是最后防线。
        "id": "M1", "bug": "R1/ADR-209: repo clone 覆盖任务级/项目级 upload_file_id（兜底守卫缺 r.Prepare==nil）",
        "file": "services/task-service/internal/service/task_service.go",
        "edits": [(
            'if needProjectLookup && r.Prepare == nil && repoURL != "" {',
            'if needProjectLookup && repoURL != "" {',
        )],
        "expect": 1,  # R47 后防御收敛单点（needProjectLookup 内含第二道）
        "module": "services/task-service",
        "run": "TestStartTask_(Task|Project)UploadWinsOverRepoURL",
    },
    {
        "id": "M2", "bug": "R2/ADR-212①: 阶段注册/查找/插入全不带 Metadata map → output_refs 写 nil map panic",
        "file": "services/task-service/internal/service/task_service.go",
        "edits": [
            (
                'stages = append(stages, &pb.TaskStage{StageId: id, Type: typ, Status: pb.StageStatus_STAGE_STATUS_PENDING,\n\t\t\tMetadata: map[string]string{}})',
                'stages = append(stages, &pb.TaskStage{StageId: id, Type: typ, Status: pb.StageStatus_STAGE_STATUS_PENDING})',
            ),
            (
                '\t\t\t// ADR-212: 旧代码路径注册的阶段可能无 Metadata，防御式补齐\n\t\t\tif st.Metadata == nil {\n\t\t\t\tst.Metadata = map[string]string{}\n\t\t\t}',
                '',
            ),
            (
                'st := &pb.TaskStage{\n\t\tStageId:  stageID,\n\t\tStatus:   pb.StageStatus_STAGE_STATUS_PENDING,\n\t\tMetadata: map[string]string{},\n\t}',
                'st := &pb.TaskStage{\n\t\tStageId: stageID,\n\t\tStatus:  pb.StageStatus_STAGE_STATUS_PENDING,\n\t}',
            ),
        ],
        "module": "services/task-service",
        "run": "TestReportStageComplete_ThreeState|TestRegisterStages_AIEnhancedSast",
    },
    {
        "id": "M3", "bug": "R4/ADR-212③: fusion 失败回退对 nil ctx 二次 panic（删 nil 兜底）",
        "file": "services/sast-adapter-service/internal/fusion/pipeline.go",
        "edits": [(
            'if fusionCtx == nil {\n\t\t\t\tfusionCtx = &FusionContext{\n\t\t\t\t\tTaskID:       req.GetTaskId(),\n\t\t\t\t\tSASTFindings: sastFindings,\n\t\t\t\t\tAIFindings:   aiFindings,\n\t\t\t\t}\n\t\t\t}\n\t\t\treturn p.buildFallbackResult(fusionCtx, startTime, err), nil',
            'return p.buildFallbackResult(fusionCtx, startTime, err), nil',
        )],
        "module": "services/sast-adapter-service",
        "run": "TestExecute_FirstStagePanic_FallbackNoPanic",
    },
    {
        "id": "M4", "bug": "R5/ADR-212④: findingsOf 无锁读 → 并发 map 读写（-race 判杀）",
        "file": "services/sast-adapter-service/internal/handler/sast_adapter_handler.go",
        "edits": [(
            'h.mu.RLock() // ADR-212: 与 scanOneTool 的加锁写并发，无锁读=进程级 fatal\n\tdefer h.mu.RUnlock()\n',
            '',
        )],
        "race": True,  # 并发缺陷需 -race 判杀（生产形态为 runtime fatal，测试以 race 检测器等价捕获）
        "module": "services/sast-adapter-service",
        "run": "TestFindingsOf_ConcurrentWithStoreWrite",
    },
    {
        "id": "M5", "bug": "R6/ADR-212⑤: task 事件不带 event_type 头 → 消费端全丢（删 Headers）",
        "file": "services/task-service/internal/service/event_publisher.go",
        "edits": [(
            'Headers: []kafka.Header{{Key: "event_type", Value: []byte(topic)}},',
            '',
        )],
        "module": "services/task-service",
        "run": "TestBuildTaskEvent_HeaderAndPayloadAligned",
    },
    {
        "id": "M6", "bug": "R7/ADR-212⑥: FAILED 报告重试不删旧行 → 同键必冲突（删 DeleteReport）",
        "file": "services/result-service/internal/service/report_service.go",
        "edits": [(
            'if existing != nil && existing.Status == "FAILED" {\n\t\tif err := s.repo.DeleteReport(existing.ID); err != nil {\n\t\t\treturn nil, status.Errorf(codes.Internal, "failed to clear FAILED report %s: %v", existing.ID, err)\n\t\t}\n\t}',
            '_ = existing',
        )],
        "module": "services/result-service",
        "run": "TestGenerateReport_FailedReportRetry_SameID|TestHandleTaskCompleted_Redelivery_Idempotent",
    },
    {
        "id": "M7", "bug": "R9/ADR-212⑧: WS token 查询参数整条落日志（脱敏失效）",
        "file": "services/gateway-service/internal/middleware/logging.go",
        "edits": [(
            'if q.Get("token") != "" {\n\t\tq.Set("token", "REDACTED")\n\t\tu.RawQuery = q.Encode()\n\t}',
            '_ = q',
        )],
        "module": "services/gateway-service",
        "run": "TestRedactToken",
    },
    {
        "id": "M8", "bug": "R10/ADR-212⑨: 限流键无视 JWT sub 回落（键策略回退）",
        "file": "services/gateway-service/internal/middleware/ratelimit.go",
        "edits": [(
            'key := ""\n\t\tif sub, ok := r.Context().Value(UserIDKey).(string); ok && sub != "" {\n\t\t\tkey = "user:" + sub\n\t\t}',
            'key := ""',
        )],
        "module": "services/gateway-service",
        "run": "TestRateLimit_KeyedByJWTSub",
    },
    {
        "id": "M9", "bug": "R11/ADR-212⑩: 通知 user_id 改回取 query（列表+归属核验双点 IDOR 回归）",
        "file": "services/gateway-service/internal/handler/transcode.go",
        "edits": [(
            'userID, _ := r.Context().Value(middleware.UserIDKey).(string)',
            'userID := r.URL.Query().Get("user_id")',
        )],
        "expect": 3,  # 列表分支、read 归属核验分支、read-all 组合分支同文案（原缺陷即多处同雷；ADR-222 增第三处）
        "module": "services/gateway-service",
        "run": "TestNotifications_UserIdFromJWTNotQuery|TestNotifications_ReadAll_UserFromJWTNotQuery",
    },
    {
        "id": "M10", "bug": "R12/ADR-212⑪: 沙箱创建失败不注销注册表（泄漏+屏蔽对账）",
        "file": "services/dsh-runtime-service/internal/sandbox/session.go",
        "edits": [(
            'activeSandboxes.Delete(name)\n\t\tr.event("error", "沙箱创建失败: %v", err)',
            'r.event("error", "沙箱创建失败: %v", err)',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestRun_CreateFailure_DeregistersActiveEntry",
    },
    {
        "id": "M11", "bug": "R13/ADR-212⑫: reconciler 标签键值混用（VALUE 当 KEY 查）",
        "file": "services/dsh-runtime-service/internal/sandbox/reconciler.go",
        "edits": [(
            'ref.Labels[managedByLabelKey] == managedByLabelValue',
            'ref.Labels[managedByLabelValue] == managedByLabelKey',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestOrphanNames",
    },
    {
        "id": "M12", "bug": "R16/ADR-213①: 流式路退出不撤上游流（泵 goroutine 泄漏）",
        "file": "services/gateway-service/internal/handler/taskwatch.go",
        "edits": [(
            'streamCtx, cancelStream := context.WithCancel(ctx)\n\tdefer cancelStream()',
            'streamCtx, cancelStream := context.WithCancel(ctx)\n\t_ = cancelStream',
        )],
        "module": "services/gateway-service",
        "run": "TestStreamWatch_FallbackCancelsUpstreamStream",
    },
    {
        "id": "M13", "bug": "R17/ADR-213②: 断流回退前不冲刷待推增量（丢最后一窗）",
        "file": "services/gateway-service/internal/handler/taskwatch.go",
        "edits": [(
            'if dirty {\n\t\t\t\t\tif !push() {\n\t\t\t\t\t\treturn true\n\t\t\t\t\t}\n\t\t\t\t\tdirty = false\n\t\t\t\t}\n\t\t\t\treturn false // 断流未收束（含 AI 流断而未 complete）→ 轮询兜底续跑',
            'return false // 断流未收束（含 AI 流断而未 complete）→ 轮询兜底续跑',
        )],
        "module": "services/gateway-service",
        "run": "TestStreamWatch_FallbackFlushesPendingLogs",
    },
    {
        "id": "M14", "bug": "R18/ADR-213③: 轮询路瞬时错误立即拆线（容忍拍数归零）",
        "file": "services/gateway-service/internal/handler/taskwatch.go",
        "edits": [(
            'if transientTicks > pollMaxTransientTicks {',
            'if transientTicks > 0 {',
        )],
        "module": "services/gateway-service",
        "run": "TestPollWatch_TransientErrorTolerated",
    },
    {
        "id": "M15", "bug": "R19/ADR-213 死路径: 无 DSH 连接时 AI 断流判定恒真（删 ais!=nil 守卫）",
        "file": "services/gateway-service/internal/handler/taskwatch.go",
        "edits": [(
            'if ais != nil && aiEnded && !sawAIComplete {',
            'if aiEnded && !sawAIComplete {',
        )],
        "module": "services/gateway-service",
        "run": "TestStreamWatch_NoDSH_StreamsStayOnStreamPath",
    },
    {
        "id": "M16", "bug": "R20/ADR-214: conflict 组员索引回建自 dedup 后输出（AI 成员恒缺）",
        "file": "services/sast-adapter-service/internal/fusion/stage_conflict.go",
        "edits": [(
            'byID := make(map[string]*pb.UnifiedFinding)\n\tfor _, f := range input.FilteredSAST {\n\t\tbyID[f.GetFindingId()] = f\n\t}\n\tfor _, f := range input.FilteredAI {\n\t\tbyID[f.GetFindingId()] = f\n\t}',
            'byID := make(map[string]*pb.UnifiedFinding)\n\tfor _, f := range input.FusedFindings {\n\t\tbyID[f.GetFindingId()] = f\n\t}',
        )],
        "module": "services/sast-adapter-service",
        "run": "TestConflictResolve_SeesAIMember_AndWritesBackVerdict|TestPipeline_ConflictAndConfidenceLive",
    },
    {
        "id": "M17", "bug": "R20/ADR-214: confidence 组员索引回建自 dedup 后输出（boost 恒 1.0）",
        "file": "services/sast-adapter-service/internal/fusion/stage_confidence.go",
        "edits": [(
            'members := make(map[string]*pb.UnifiedFinding, len(input.FilteredSAST)+len(input.FilteredAI))\n\t\tfor _, f := range input.FilteredSAST {\n\t\t\tmembers[f.GetFindingId()] = f\n\t\t}\n\t\tfor _, f := range input.FilteredAI {\n\t\t\tmembers[f.GetFindingId()] = f\n\t\t}',
            'members := make(map[string]*pb.UnifiedFinding)\n\t\tfor _, f := range input.FusedFindings {\n\t\t\tmembers[f.GetFindingId()] = f\n\t\t}',
        )],
        "module": "services/sast-adapter-service",
        "run": "TestConfidenceFusion_MultiSourceBoost|TestPipeline_ConflictAndConfidenceLive",
    },
    {
        "id": "M18", "bug": "R21/ADR-215①: sharedAILogs LRU 淘汰失效（over 恒 false）",
        "file": "services/dsh-runtime-service/internal/service/ai_interaction_log.go",
        "edits": [(
            'over := func() bool {\n\t\tif len(s.logs) <= aiLogMaxEntries && s.totalBytesLocked() <= aiLogMaxTotalBytes {\n\t\t\treturn false\n\t\t}\n\t\treturn len(s.logs) > 0\n\t}',
            'over := func() bool {\n\t\treturn false\n\t}',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestAILogStore_LRUEviction|TestAILogStore_IncompleteEntriesProtected",
    },
    {
        "id": "M19", "bug": "R22/ADR-215 回归: AI 日志回调不接进 cfg（接线序回归的等价形态）",
        "file": "services/dsh-runtime-service/internal/service/sandbox_verify.go",
        "edits": [(
            'cfg.OnHumanLog = func(s string) { e.write([]byte(s)) }\n\tcfg.OnRawLog = e.writeRaw',
            '_ = e',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestWireAILog_WiresCallbacksBeforeRunnerCopy",
    },
    {
        "id": "M20", "bug": "R23/gw-f6a3523①: source-file 根解析缺①b uploads-unpacked 流（404 回归）",
        "file": "services/gateway-service/internal/handler/sourcefile.go",
        "edits": [(
            'if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {\n\t\t\treturn resolveProjectRoot(candidate), "uploads_unpacked", nil\n\t\t}',
            '',
        )],
        "module": "services/gateway-service",
        "run": "TestSourceFile_UploadsUnpackedFlow",
    },
    {
        "id": "M21", "bug": "R24/gw-f6a3523②: 剥壳降入失效（唯一子目录不降入，unpacked/<壳> 根错位）",
        "file": "services/task-service/internal/service/archive.go",
        "edits": [(
            'for i := 0; i < resolveRootDescentCap; i++ {',
            'for i := 0; i < 0; i++ {',
        )],
        "module": "services/task-service",
        "run": "TestResolveProjectRoot",
    },
    {
        "id": "M22", "bug": "R25/gw-f6a3523③: WS 连接寿命回缩 30min（长审计中途断流）",
        "file": "services/gateway-service/internal/handler/taskwatch.go",
        "edits": [(
            'wsMaxLifetime = 6 * time.Hour',
            'wsMaxLifetime = 30 * time.Minute',
        )],
        "module": "services/gateway-service",
        "run": "TestTaskWatch_LifetimeCoversLongAudit",
    },
    {
        "id": "M23", "bug": "R26/TP12-T3: 幂等键注入先于解码被 protojson 重置（恒空）",
        "file": "services/gateway-service/internal/handler/transcode.go",
        "edits": [(
            'req := &pb.CreateProjectRequest{}\n\t\tif err := decodeBody(r, req); err != nil {\n\t\t\twriteError(w, http.StatusBadRequest, err.Error())\n\t\t\treturn\n\t\t}\n\t\treq.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}',
            'req := &pb.CreateProjectRequest{}\n\t\treq.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}\n\t\tif err := decodeBody(r, req); err != nil {\n\t\t\twriteError(w, http.StatusBadRequest, err.Error())\n\t\t\treturn\n\t\t}',
        )],
        "module": "services/gateway-service",
        "run": "TestCreateProject_IdempotencyInjectedAfterDecode",
    },
    {
        # R-29：变异面是 Dockerfile 文本（非 Go 源）——runner 的编辑/恢复机制与文件类型
        # 无关，锁定测试为 Go 侧镜像契约（读 Dockerfile 断言），删除 git 后必红。
        "id": "M24", "bug": "R29: task 运行时镜像删 git → 仓库拉取模式部署形态恒 DEAD",
        "file": "services/task-service/Dockerfile",
        "edits": [(
            'RUN apk --no-cache add ca-certificates tzdata git',
            'RUN apk --no-cache add ca-certificates tzdata',
        )],
        "module": "services/task-service",
        "run": "TestTaskImageContainsGit",
    },
    {
        # R-30：三条 SQL 路径漏 reasoning 列的等价再引入（文本面契约锁定，M25 锚点带
        # GetByID 独有 WHERE 尾巴保证唯一）。memory 仓整结构体拷贝，行为面测不出。
        # ADR-225：SELECT/INSERT 列清单追加 inherited_from_task_id，锚点同步。
        "id": "M25", "bug": "R30: 行投影 SELECT 删 reasoning → AI 结论/裁决理由读回恒空（ADR-195 链路点选永不渲染）",
        "file": "services/result-service/internal/repository/finding_repository.go",
        "edits": [(
            "SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, COALESCE(reasoning, '') AS reasoning, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id, COALESCE(inherited_from_task_id, '') AS inherited_from_task_id\n\t\tFROM findings WHERE id = $1",
            "SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id, COALESCE(inherited_from_task_id, '') AS inherited_from_task_id\n\t\tFROM findings WHERE id = $1",
        )],
        "module": "services/result-service",
        "run": "TestFindingRepoReasoningWired",
    },
    {
        "id": "M26", "bug": "R30: INSERT 删 reasoning → 创建期 AI 结论原文（[DSH-sandbox]/[LLM:]）不落库",
        "file": "services/result-service/internal/repository/finding_repository.go",
        "edits": [(
            'INSERT INTO findings (id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, reasoning, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id, inherited_from_task_id)',
            'INSERT INTO findings (id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id, inherited_from_task_id)',
        )],
        "module": "services/result-service",
        "run": "TestFindingRepoReasoningWired",
    },
    {
        "id": "M27", "bug": "R30: UPDATE 删 reasoning → 人工裁决理由静默丢弃（verdict 落库、理由丢）",
        "file": "services/result-service/internal/repository/finding_repository.go",
        "edits": [(
            'file_path = $7, line_number = $8, source_raw = $9, verdict = $10, reasoning = $18,',
            'file_path = $7, line_number = $8, source_raw = $9, verdict = $10,',
        )],
        "module": "services/result-service",
        "run": "TestFindingRepoReasoningWired",
    },
    {
        "id": "M28", "bug": "R32: CreateProject 包装键校验短路 → 缺 project 键静默创建全空项目（201 空壳）",
        "file": "services/project-service/internal/handler/project.go",
        "edits": [(
            'if req.GetProject() == nil || strings.TrimSpace(req.GetProject().GetName()) == "" {',
            'if false && (req.GetProject() == nil || strings.TrimSpace(req.GetProject().GetName()) == "") {',
        )],
        "module": "services/project-service",
        "run": "TestCreateProjectRejectsMissingProject",
    },
    {
        "id": "M29", "bug": "R33: archiveExt 丢 .tar.gz 双段后缀 → 落盘名 archive-<ts>.gz 不满足解包 switch，.tar.gz 上传 prepare 必挂（gw-e295b637）",
        "file": "services/task-service/internal/service/archive.go",
        "edits": [(
            'case strings.HasSuffix(name, ".tar.gz"):',
            'case strings.HasSuffix(name, ".tar.gz") && false:',
        )],
        "module": "services/task-service",
        "run": "TestFetchUploadArchive_TarGzFullChain",
    },
    {
        "id": "M30", "bug": "R34: Run 无视 PatchFixRound 套用 findings 解析 → 修复轮 submit_patches 合规提交被 no JSON 判废（gw-d331089f）",
        "file": "services/dsh-runtime-service/internal/sandbox/sandbox.go",
        "edits": [(
            'if t.PatchFixRound {',
            'if false && t.PatchFixRound {',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestRun_PatchFixRoundToolBatchesMerged",
    },
    {
        "id": "M31", "bug": "R35: resolveSectionPath 无沙箱挂载前缀容错 → 补丁段路径 project/… 全拒（gw-d331089f）",
        "file": "services/dsh-runtime-service/internal/service/fixpatch.go",
        "edits": [(
            'if !exists(path) && exists(trimmed) {',
            'if false && !exists(path) && exists(trimmed) {',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestNormalizeDiffPatch_SandboxMountPrefixRewrite",
    },
    {
        "id": "M32", "bug": "R36: interaction_dir 丢失部署覆盖口 → 探针指向本容器 CWD 相对路径恒 miss，长审计误判 TIMEOUT（gw-d331089f）",
        "file": "services/dsh-runtime-service/internal/service/sandbox_analysis.go",
        "edits": [(
            'v, err := cfg.Str("dsh_runtime.sandbox.interaction_dir", "CODEAUDIT_INTERACTION_DIR")',
            'v, err := cfg.Str("dsh_runtime.sandbox.interaction_dir")',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestInteractionDir_EnvOverrideWins",
    },
    {
        "id": "M33", "bug": "R37: @@ 定义行重新物化为上下文行 → 锚点双写/丢缩进形态整补丁被拒（gw-61200b8b/gw-5a7393ed）",
        "file": "services/dsh-runtime-service/internal/service/fixpatch.go",
        "edits": [(
            'hunk.defStr = strings.TrimPrefix(ln, "@@ ")',
            'hunk.defStr = strings.TrimPrefix(ln, "@@ "); hunk.lines = append(hunk.lines, patchLine{kind: lineCtx, text: strings.TrimPrefix(ln, "@@ ")})',
        )],
        "module": "services/dsh-runtime-service",
        "run": "TestNormalizeDiffPatch_AnchorDoubleWriteUnindented",
    },
    {
        "id": "M34", "bug": "R38: 融合合并按 file_path 精确字符串比对——SAST 绝对路径×AI 裸文件名永不命中，同文件同行漏洞双报（2026-09-09 用户报障）",
        "file": "services/sast-adapter-service/internal/fusion/stage_merge.go",
        "edits": [(
            'return s == a || strings.HasSuffix(s, "/"+a)',
            'return s == a',
        )],
        "expect": 1,  # 路径后缀对齐回退到精确匹配 = 变异再引入双报
        "module": "services/sast-adapter-service",
        "run": "TestMergeStage_PathSuffixAlignment_MergesSameVuln",
    },
    {
        # ADR-225 增量核心三颗牙齿（验收文档 F4/F7/F6 红线面）：
        # 继承排除过滤删除 = 已修复/已删除文件的旧 findings 复活（最高危：漏报被当有效历史）
        "id": "M35", "bug": "ADR-225: InheritFindings 删排除过滤 → 变更/删除文件的旧 findings 被继承（过期漏洞复活为新任务有效项）",
        "file": "services/result-service/internal/service/inherit_service.go",
        "edits": [(
            # 后缀对齐重构（94f46944）后锚点同步；恒假条件防 exclude 只写不读=编译失败
            'if excludedBy(normalizeInheritPath(f.FilePath), exclude) {',
            'if excludedBy(normalizeInheritPath(f.FilePath), exclude) && false {',
        )],
        "expect": 1,
        "module": "services/result-service",
        "run": "TestInheritFindings_MatrixAndCopy",
    },
    {
        # 基线候选的 COMPLETED 守卫删除 = RUNNING/FAILED 任务的半成品 findings 被当基线继承
        "id": "M36", "bug": "ADR-225: 基线选定删 COMPLETED 守卫 → 非终态任务混入基线候选（半成品 findings 被继承）",
        "file": "services/task-service/internal/service/incremental.go",
        "edits": [(
            'b.GetStatus() != pb.TaskStatus_TASK_STATUS_COMPLETED {',
            'false {',
        )],
        "expect": 1,  # 显式基线校验用 bt.GetStatus() 前缀不同，不与此锚点重叠
        "module": "services/task-service",
        "run": "TestSelectBaseline_Rules",
    },
    {
        # files_argv 占位守卫删除 = 文件清单模式静默退化成无目标参数调用
        # （编辑面取 `!hasFiles && false`：恒假禁用守卫但保持 hasFiles 被读取——
        #   直接换 `if false` 会令 hasFiles 只写不读=编译失败，不是牙齿）
        "id": "M37", "bug": "ADR-225: files_argv 缺 {files} 占位守卫删除 → 配置错误静默通过（增量扫描空跑）",
        "file": "services/sast-adapter-service/internal/handler/sast_incremental.go",
        "edits": [(
            'if !hasFiles {',
            'if !hasFiles && false {',
        )],
        "expect": 1,
        "module": "services/sast-adapter-service",
        "run": "TestBuildFilesArgv_MissingFilesPlaceholder",
    },
    {
        # ADR-225 S5 误删零容忍牙齿（验收全局底线③；F13.2/F13.6 双红线面）
        # 邻居保护删除 = R36 AI 交互日志/gateway 缓存被回收器清空
        "id": "M38", "bug": "ADR-225: 回收器删邻居目录保护（隐藏目录前缀）→ gateway 重物化缓存被清空（gcProtectedNames 精确名仍护 ai-interaction，隐藏前缀守卫单独判杀）",
        "file": "services/task-service/internal/service/repo_cache.go",
        "edits": [(
            'if strings.HasPrefix(name, ".") {\n\t\treturn true // .gateway-cache / .gc-* 中转态 / 其他隐藏目录\n\t}',
            'if strings.HasPrefix(name, ".") {\n\t\treturn false // .gateway-cache / .gc-* 中转态 / 其他隐藏目录\n\t}',
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestRepoCacheGC_HardProtections",
    },
    {
        # 终态判定吞并 FAILED = 自动重试在途任务卷树被并发删除（重试链正 re-prep 同一目录）
        "id": "M39", "bug": "ADR-225: isTerminalStatus 吞并 FAILED → 自动重试在途任务的卷树被驱逐（与重试 re-prepare 竞态）",
        "file": "services/task-service/internal/service/repo_cache.go",
        "edits": [(
            'pb.TaskStatus_TASK_STATUS_TIMEOUT, pb.TaskStatus_TASK_STATUS_DEAD:',
            'pb.TaskStatus_TASK_STATUS_TIMEOUT, pb.TaskStatus_TASK_STATUS_DEAD, pb.TaskStatus_TASK_STATUS_FAILED:',
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestRepoCacheGC_HardProtections",
    },
    {
        # 2026-09-11 修复批次（R43）：ListUsers 游标超界钳制禁用 = admin 面 cursor DoS panic
        "id": "M40", "bug": "R43: ListUsers offset 钳制移除 → cursor 超界负容量 panic（admin 面 DoS）",
        "file": "services/project-service/internal/handler/user.go",
        "edits": [(
            'if offset > len(recs) { // R43: 游标超界钳制（对齐 ListProjects）——否则负容量 panic',
            'if false { // MUTANT: 钳制禁用',
        )],
        "expect": 1,
        "module": "services/project-service",
        "run": "TestListUsers_CursorBeyondEnd_EmptyPage",
    },
    {
        # 2026-09-11 修复批次（R42）：snippet 转义移除 = 报告存储型 XSS
        "id": "M41", "bug": "R42: 报告代码片段列去 htmlEsc → 被扫源码注入 <script> 存储型 XSS",
        "file": "services/result-service/internal/service/report_service.go",
        "edits": [(
            'b.WriteString(fmt.Sprintf("<td><pre>%s</pre></td>", htmlEsc(snippet)))',
            'b.WriteString(fmt.Sprintf("<td><pre>%s</pre></td>", snippet))',
        )],
        "expect": 1,
        "module": "services/result-service",
        "run": "TestRenderHTMLReport_SnippetEscaped",
    },
    {
        # 2026-09-11 修复批次（R52/D5 复核点）：容量线记账改回删后量 = 超线全清
        "id": "M42", "bug": "R52: 容量线记账改回驱逐后 dirSize（恒 0）→ total 永不下降→超线逐出全部候选而非驱至水位",
        "file": "services/task-service/internal/service/repo_cache.go",
        "edits": [(
            'if freed, ok := g.evictIfTarInBucket(c.dir, c.taskID, c.tarID, storageAddr, "容量"); ok {\n\t\t\ttotal -= freed',
            'if _, ok := g.evictIfTarInBucket(c.dir, c.taskID, c.tarID, storageAddr, "容量"); ok {\n\t\t\ttotal -= dirSize(c.dir)',
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestRepoCacheGC_CapacityStopsAtWatermark",
    },
    {
        # R60：FailedCount 检查移除 = 缺继承行任务假成功
        "id": "M43", "bug": "R60: 继承部分失败检查移除 → 缺继承行的任务照常 COMPLETED（完整视图造假）",
        "file": "services/task-service/internal/orchestrator/incremental.go",
        "edits": [(
            "if n := resp.GetFailedCount(); n > 0 {",
            "if n := resp.GetFailedCount(); false {",
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestRunIncrementalInherit_PartialFailureFails",
    },
    {
        # R61：两段校验退化为裸后缀 = 嵌套同后缀误杀（漏报）
        "id": "M44", "bug": "R61: 后缀命中的前缀段判定移除 → vendor/pkg/util/keys.py 被 pkg/util/keys.py 误杀（漏洞消失）",
        "file": "services/result-service/internal/service/inherit_service.go",
        "edits": [(
            'prefix := strings.TrimSuffix(findingPath, p) // 形如 /data/repos/uploads-x/unpacked/ 或 /x/vendor/\n\t\tif prefix == "" || prefix == "/" {\n\t\t\treturn true // 相对形态（已归一）——精确后缀即命中\n\t\t}\n\t\tif strings.HasSuffix(prefix, "/unpacked/") {\n\t\t\treturn true // 上传树根形态——真绝对路径\n\t\t}',
            'prefix := strings.TrimSuffix(findingPath, p)\n\t\t_ = prefix\n\t\tif true {\n\t\t\treturn true\n\t\t}',
        )],
        "expect": 1,
        "module": "services/result-service",
        "run": "TestExcludedBy_TwoSegmentCheck",
    },
    {
        # 2026-09-12（R71/ADR-227）：畸形帧瞬态分类移除 = 一帧坏 JSON 判回合死刑拆沙箱
        "id": "M45", "bug": "R71: malformed SSE payload 从瞬态关键字表移除 → 流中途数据级损坏不再续跑重试，整回合作废沙箱回收",
        "file": "services/dsh-runtime-service/internal/sandbox/session.go",
        "edits": [(
            '\n\t\t"malformed SSE payload",',
            '',
        )],
        "expect": 1,
        "module": "services/dsh-runtime-service",
        "run": "TestRun_MainTurnMalformedPayloadRetry",
    },
    {
        # 2026-09-13 修复批次A（R72）：种子门禁失效 = 生产默认凭据 admin/admin 回潮
        "id": "M46", "bug": "R72: NewMemoryStore 种子门禁改恒真 → seedAdmin=false 仍预置 admin/admin+ROLE_ADMIN（默认凭据登入全 admin 权）",
        "file": "services/project-service/internal/repo/memory.go",
        "edits": [(
            "\tif !seedAdmin {\n\t\treturn s\n\t}",
            "\tif false { // MUTANT: R72 种子门禁失效（无条件种子，默认凭据回潮）\n\t\treturn s\n\t}",
        )],
        "expect": 1,
        "module": "services/project-service",
        "run": "TestSeedAdmin_DisabledByDefault",
    },
    {
        # 2026-09-13 修复批次A（R73）：role 剥离移除 = 非管理员自助改 role 自封 admin
        "id": "M47", "bug": "R73: 网关非 admin self 更新的 role 剥离移除 → self 更新携带 ROLE_ADMIN 直达服务端生效（纵向提权）",
        "file": "services/gateway-service/internal/handler/transcode.go",
        "edits": [(
            'req.User.Role = pb.Role_ROLE_UNSPECIFIED',
            '_ = req.GetUser() // MUTANT: R73 role 剥离移除',
        )],
        "expect": 1,
        "module": "services/gateway-service",
        "run": "TestUpdateUser_NonAdminRoleEscalationBlocked",
    },
    {
        # 2026-09-13 修复批次A（R74）：clone 侧白名单移除 = git ext:: 传输 RCE 回潮
        "id": "M48", "bug": "R74: cloneRepo scheme 白名单移除 → ext::<command> 外置传输在 task-service 容器执行任意命令",
        "file": "services/task-service/internal/service/repo_fetch.go",
        "edits": [(
            'if u, err := url.Parse(repoURL); err == nil && u.Scheme != "" && !allowedCloneSchemes[u.Scheme] {',
            'if u, err := url.Parse(repoURL); err == nil && false {',
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestCloneRepo_RejectsUnsafeRepoTarget",
    },
    {
        # 2026-09-13 修复批次B（R75）：编排入口废功注销回潮 = R67 取消传播链断裂
        "id": "M49", "bug": "R75: runOrchestration 入口 delete(s.cancels) 回潮 → 注销 StartTask 刚注册的取消器，CancelScanTask 恒 miss、在途编排不可打断（ADR-191 撤超时后取消是唯一中断机制）",
        "file": "services/task-service/internal/service/task_service.go",
        "edits": [(
            "\tattempt := 0\n\t// R75:",
            "\tattempt := 0\n\ts.mu.Lock()\n\tdelete(s.cancels, r.TaskID) // MUTANT: R67 废功注销回潮\n\ts.mu.Unlock()\n\t// R75:",
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestCancelTask_PropagatesToOrchestration",
    },
    {
        # 2026-09-13 修复批次B（R76）：Run 超时接线移除 = 回合挂起不可打断沙箱永不回收
        "id": "M50", "bug": "R76: Run 的 Task.Timeout deadline 派生移除 → 07 §8 超时矩阵回到死字段，回合可无限挂起",
        "file": "services/dsh-runtime-service/internal/sandbox/sandbox.go",
        "edits": [(
            "\t// R76: Task.Timeout 施加 deadline（07 §8；<=0 不施加——模式 B/C ADR-191 不设外层时限，挂起由 HTTPClientTimeout 兜底）\n\tctx, cancel := withDeadline(ctx, t.Timeout)\n\tdefer cancel()",
            "\t_ = t.Timeout // MUTANT: R76 超时接线移除",
        )],
        "expect": 1,
        "module": "services/dsh-runtime-service",
        "run": "TestRun_TaskTimeoutStopsHangingLaunch",
    },
    {
        # 2026-09-13 修复批次C（R77）：锁内 PG 写无界回归 = PG 抖动冻结全任务面
        "id": "M51", "bug": "R77: upsert 超时 ctx 摘除（改 Background）→ 全局写锁内无界 PG Exec，PG 抖动冻结创建/列表/快照/流式全任务面",
        "file": "services/task-service/internal/service/task_store_pg.go",
        "edits": [(
            "_, err = st.db.ExecContext(ctx, `INSERT INTO tasks",
            "_, err = st.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO tasks",
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestPGUpsert_BoundedInLock",
    },
    {
        # 2026-09-13 修复批次C（R78）：编排内 Background ctx 取消旁路回潮
        "id": "M52", "bug": "R78: compensateFindings 补偿 ctx 改回 Background → 取消旁路回潮，result 挂起即编排协程永久悬挂",
        "file": "services/task-service/internal/orchestrator/orchestrator.go",
        "edits": [(
            "dCtx, dCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)",
            "dCtx, dCancel := context.WithTimeout(context.Background(), 30*time.Second)",
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestOrchestrator_NoBackgroundCtxBypass",
    },
    {
        # 2026-09-13 修复批次C（R79）：共享 .tmp 回潮 = 并发 miss 缓存投毒
        "id": "M53", "bug": "R79: 重物化 tmp 唯一后缀回退共享 <id>.tmp → 并发 miss 入口/收尾 RemoveAll 互拆，产出缺文件的投毒缓存树",
        "file": "services/gateway-service/internal/handler/sourcefile_rehydrate.go",
        "edits": [(
            'tmpDir := taskDir + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)',
            'tmpDir := taskDir + ".tmp" // MUTANT: R79 共享 tmp 回潮',
        )],
        "expect": 1,
        "module": "services/gateway-service",
        "run": "TestRehydrateTmpDir_UniqueSuffix",
    },
    {
        # 2026-09-13 修复批次C（R80）：model 白名单移除 = 沙箱内 bash -c 注入面回潮
        "id": "M54", "bug": "R80: SetInferenceRoute model 白名单移除 → 空格/元字符 model 名直拼沙箱 bash -c（断裂或注入）",
        "file": "services/dsh-runtime-service/internal/service/inference_admin.go",
        "edits": [(
            "if !modelSafePattern.MatchString(req.GetModel()) {",
            "if false { // MUTANT: R80 白名单移除",
        )],
        "expect": 1,
        "module": "services/dsh-runtime-service",
        "run": "TestSetInferenceRoute_RejectsUnsafeModel",
    },
    {
        # 2026-09-13 修复批次C（R81）：落盘失败仍缓存 = findings 永久丢失
        "id": "M55", "bug": "R81: RunSASTScan 落盘失败仍写幂等缓存 → 同 request_id 重试永远拿缓存，findings 对下游永久丢失",
        "file": "services/sast-adapter-service/internal/handler/sast_adapter_handler.go",
        "edits": [(
            "if persistErr == nil {\n\t\th.idempotency.Store(reqID, resp)\n\t\th.bumpIdemGuard()\n\t}",
            "\t_ = persistErr // MUTANT: R81 落盘失败仍缓存\n\th.idempotency.Store(reqID, resp)\n\th.bumpIdemGuard()",
        )],
        "expect": 1,
        "module": "services/sast-adapter-service",
        "run": "TestRunSASTScan_PersistFailureNotCached",
    },
    {
        # 2026-09-13 修复批次C（R81）：原子计数退化为裸 int（-race 判杀）
        "id": "M56", "bug": "R81: bumpIdemGuard 计数退回裸 int++ → 并行扫描下数据竞争+丢失更新（R69 护栏计数失真）",
        "file": "services/sast-adapter-service/internal/handler/sast_adapter_handler.go",
        "edits": [
            ('\t"sync"\n\t"sync/atomic"\n', '\t"sync"\n'),
            ("idemCount    atomic.Int64", "idemCount    int"),
            ("if h.idemCount.Add(1) > 10_000 {", "h.idemCount++\n\tif h.idemCount > 10_000 {"),
            ("h.idemCount.Store(0)", "h.idemCount = 0"),
        ],
        "expect": 1,
        "module": "services/sast-adapter-service",
        "run": "TestBumpIdemGuard_ConcurrentAtomic",
        "race": True,
    },
    {
        # 2026-09-13 修复批次C（R82）：feedback_type 丢弃回潮
        "id": "M57", "bug": "R82: feedback_type 赋值改常量 → 请求分类被丢弃，误报/漏报/误级统计与训练回流数据源失效",
        "file": "services/result-service/internal/service/result_service.go",
        "edits": [(
            "FeedbackType: req.GetFeedbackType().String(),",
            'FeedbackType: "FEEDBACK_UNSPECIFIED", // MUTANT: R82 分类丢弃',
        )],
        "expect": 1,
        "module": "services/result-service",
        "run": "TestSubmitFindingFeedback_PersistsFeedbackType",
    },
    {
        # 2026-09-13 修复批次C（R83）：producer Close 接线移除（R70② 漂移回潮）
        "id": "M58", "bug": "R83: main 的 defer producer.Close() 移除 → 停机不冲刷在途事件批（R70② 台账漂移回潮）",
        "file": "services/task-service/cmd/main.go",
        "edits": [(
            "\t\tdefer producer.Close()",
            "\t\t_ = producer // MUTANT: R83 Close 接线移除",
        )],
        "expect": 1,
        "module": "services/task-service",
        "run": "TestMainWiresProducerClose",
    },
    {
        # 2026-09-13 修复批次C（R84）：请求体上限移除 = 网关 OOM DoS 回潮
        "id": "M59", "bug": "R84: serveHTTP 的 MaxBytesReader 移除 → decodeBody 无界 ReadAll，认证用户大 body OOM 网关",
        "file": "services/gateway-service/internal/handler/transcode.go",
        "edits": [(
            "r.Body = http.MaxBytesReader(w, r.Body, 1<<20)",
            "_ = w // MUTANT: R84 请求体上限移除",
        )],
        "expect": 1,
        "module": "services/gateway-service",
        "run": "TestServeHTTP_BodyCappedAt1MiB",
    },
    {
        # 2026-09-13 复审修正批（R85）：RunSession 超时接线移除（R76 的 session 侧回潮面）
        "id": "M60", "bug": "R76: RunSession 的 SessionTask.Timeout deadline 派生移除 → 多轮会话回到死字段，30m 上限失效",
        "file": "services/dsh-runtime-service/internal/sandbox/session.go",
        "edits": [(
            "\t// R76: 整会话上限接线（07 §8:126；原 SessionTask.Timeout 死字段同 Run）\n\tctx, cancel := withDeadline(ctx, t.Timeout)\n\tdefer cancel()",
            "\t_ = t.Timeout // MUTANT: R76 RunSession 接线移除",
        )],
        "expect": 1,
        "module": "services/dsh-runtime-service",
        "run": "TestRunSession_TaskTimeoutStopsHangingLaunch",
    },    {
        # 2026-09-13 评审修正（R86）：孤儿清扫终态守卫移除 → 在途任务残留被清（正常业务受损）
        "id": "M61", "bug": "R86: sweepStaleRehydrateTmp 的任务终态守卫移除 → 在途任务的 .tmp 残留被时长阈值单独判死，正常业务被清",
        "file": "services/gateway-service/internal/handler/sourcefile_rehydrate.go",
        "edits": [(
            "if err != nil || !isTerminalTaskStatus(resp.GetStatus()) {\n\t\treturn // 查不到/未终态：保守不扫\n\t}",
            "_ = resp\n\t_ = err // MUTANT: R86 终态守卫移除（在途残留被清）",
        )],
        "expect": 1,
        "module": "services/gateway-service",
        "run": "TestSweepStaleRehydrateTmp_TerminalGuard",
    },    {
        # 2026-09-13 架构项批次（R87）：refresh 会话纪元守卫移除 → 登出/改密/重置/停用后
        # 被盗 refresh（7d）仍可持续换新 access 对
        "id": "M62", "bug": "R87: RefreshToken 会话纪元守卫移除 → 登出/改密/管理员重置/停用后旧 refresh 照常换新",
        "file": "services/project-service/internal/service/user.go",
        "edits": [(
            '\tif iatF, ok := claims["iat"].(float64); ok {\n\t\tif s.sessionInvalidated(sub, time.Unix(int64(iatF), 0)) {\n\t\t\treturn nil, fmt.Errorf("session has been invalidated, please login again")\n\t\t}\n\t}',
            '\t_ = claims // MUTANT: R87 会话纪元守卫移除',
        )],
        "expect": 1,
        "module": "services/project-service",
        "run": "TestRefreshToken_RejectsAfterLogout",
    },
    {
        # 2026-09-13 架构项批次（R88）：网关撤销集查询移除 → 登出后 access 在业务路由仍可用 30min
        "id": "M63", "bug": "R88: JWTMiddleware 的 AccessRevoked 查询移除 → 登出后 access 在全部业务路由仍可用至 TTL",
        "file": "services/gateway-service/internal/middleware/jwt.go",
        "edits": [(
            '\t\tif AccessRevoked(tokenString) {\n\t\t\thttp.Error(w, `{"error": "token has been revoked"}`, http.StatusUnauthorized)\n\t\t\treturn\n\t\t}',
            '\t\t_ = tokenString // MUTANT: R88 撤销集查询移除',
        )],
        "expect": 1,
        "module": "services/gateway-service",
        "run": "TestJWTMiddleware_RejectsRevokedToken",
    },
    {
        # 2026-09-13 架构项批次（R89）：任务归属比对移除 → 水平越权回潮
        "id": "M64", "bug": "R89: canAccessTask 归属比对移除 → 他人（非 owner 非 admin）对任务域 12 用例全放行，可读源码/日志并操作他人任务",
        "file": "services/gateway-service/internal/handler/authz.go",
        "edits": [(
            '\tif resp.GetCreatedBy() != uid {\n\t\tdenyAuthz(w, r, "task/"+taskID)\n\t\treturn false\n\t}',
            '\t_ = resp // MUTANT: R89 归属比对移除',
        )],
        "expect": 1,
        "module": "services/gateway-service",
        "run": "TestAuthz_TaskOwnershipMatrix",
    },
    {
        # 2026-09-13 架构项批次（R90）：admin 短路恒真 = 派生域/列表/批量门禁整体旁路
        "id": "M65", "bug": "R90: authz 全部 isAdmin 短路改恒真 → finding/report 按 ID、裸列表、批量 verdict 门禁整体放行",
        "file": "services/gateway-service/internal/handler/authz.go",
        "edits": [(
            'if isAdmin(r) {',
            'if true { // MUTANT: R90 门禁短路恒真',
        )],
        "expect": 6,
        "module": "services/gateway-service",
        "run": "TestAuthz_FindingReportMatrix",
    },
    {
        # 2026-09-13 架构项批次（R91）：项目写面门禁移除 → 非 admin 可改/删任意项目
        "id": "M66", "bug": "R91: canAccessProject 写面分支移除 → 成员/任意认证用户可改/删任意项目（一期写面=管理面口径失效）",
        "file": "services/gateway-service/internal/handler/authz.go",
        "edits": [(
            '\tif write {\n\t\t// 一期写面收紧为管理面（此前任意认证用户可改/删任意项目）\n\t\tlog.Printf("[authz] deny project write user=%q project=%s path=%s", uid, projectID, r.URL.Path)\n\t\twriteError(w, http.StatusForbidden, "project write requires admin")\n\t\treturn false\n\t}',
            '\t_ = write // MUTANT: R91 项目写面门禁移除',
        )],
        "expect": 1,
        "module": "services/gateway-service",
        "run": "TestAuthz_ProjectMembershipMatrix",
    },
]


def find_go():
    for cand in [
        shutil.which("go"),
        os.path.join(ROOT, ".toolchain", "go", "bin", "go"),
        os.path.join(ROOT, ".toolchain", "bin", "go"),
    ]:
        if cand and os.path.exists(cand):
            return cand
    return None


def anchor_check(mutant, src_cache):
    """每个 find 必须在目标文件中出现 expect 次（默认 1；0=源码漂移/删改，不符=锚点失去区分度）。"""
    path = os.path.join(ROOT, mutant["file"])
    if mutant["file"] not in src_cache:
        src_cache[mutant["file"]] = open(path, encoding="utf-8").read()
    src = src_cache[mutant["file"]]
    expect = mutant.get("expect", 1)
    problems = []
    for find, _ in mutant["edits"]:
        n = src.count(find)
        if n != expect:
            problems.append(f"锚点出现 {n} 次（应恰 {expect} 次）: {find[:60]!r}...")
    return problems


def run_mutant(mutant, go_bin):
    path = os.path.join(ROOT, mutant["file"])
    original = open(path, "rb").read()

    # 安全闸：目标文件有未提交改动则拒绝（防覆盖在途工作）
    st = subprocess.run(
        ["git", "-C", ROOT, "status", "--porcelain", "--", mutant["file"]],
        capture_output=True, text=True)
    if st.stdout.strip():
        return False, f"目标文件有未提交改动，拒绝变异: {st.stdout.strip()}"

    mutated = original.decode("utf-8")
    for find, replace in mutant["edits"]:
        mutated = mutated.replace(find, replace)
    args = [go_bin, "test", "./...", "-count=1"]
    if mutant.get("race"):
        args.append("-race")
    args += ["-run", mutant["run"]]
    try:
        with open(path, "w", encoding="utf-8") as f:
            f.write(mutated)
        r = subprocess.run(
            args,
            cwd=os.path.join(ROOT, mutant["module"]),
            capture_output=True, text=True, timeout=300,
            env={**os.environ, "GOPROXY": "https://goproxy.cn,direct"},
        )
        out = r.stdout + r.stderr
        build_failed = "build failed" in out or "[build failed]" in out
        test_failed = "--- FAIL:" in out or "panic:" in out
        if r.returncode == 0:
            return False, f"变异存活：带 bug 的代码跑 {mutant['run']} 竟然绿了（锁定测试失去牙齿）"
        if build_failed and not test_failed:
            return False, f"变异以编译失败收场（不是牙齿）：\n{out[:600]}"
        if not test_failed:
            return False, f"非零退出但未见 --- FAIL/panic（不可判杀）：\n{out[:600]}"
        return True, out.strip().splitlines()[-1] if out.strip() else "killed"
    finally:
        with open(path, "wb") as f:
            f.write(original)
        # 恢复核验：字节必须与改前一致
        restored = open(path, "rb").read()
        if restored != original:
            print(f"FATAL: {mutant['file']} 恢失败败（字节不一致），请 git diff 核对", file=sys.stderr)
            sys.exit(3)


def main():
    args = sys.argv[1:]
    check_only = "--check" in args
    ids = [a for a in args if not a.startswith("--")]
    go_bin = find_go()

    selected = [m for m in MUTANTS if not ids or m["id"] in ids]
    if ids and len(selected) != len(ids):
        print(f"未知变异 id: {set(ids) - {m['id'] for m in selected}}", file=sys.stderr)
        return 2

    src_cache = {}
    fail = 0
    print(f"=== 变异自检（{len(selected)}/{len(MUTANTS)} 条，模式={'仅锚点检查' if check_only else '全量'}） ===")
    for m in selected:
        problems = anchor_check(m, src_cache)
        if problems:
            print(f"  ✗ {m['id']} 锚点失配（源码漂移，先更新 MUTANTS 再谈门禁）:")
            for p in problems:
                print(f"      {p}")
            fail += 1
    if fail or check_only:
        print(f"RESULT: {'PASS' if fail == 0 else 'FAIL'} (锚点检查)")
        return 1 if fail else 0

    if go_bin is None:
        print("  ! go 工具链不可用，变异自检未执行（诚实降级；锚点检查已过）")
        return 0

    for m in selected:
        ok, detail = run_mutant(m, go_bin)
        mark = "✓" if ok else "✗"
        print(f"  {mark} {m['id']} 被杀死：{m['bug']}")
        if not ok:
            print(f"      {detail}")
            fail += 1
    print(f"RESULT: {'PASS' if fail == 0 else 'FAIL'} ({fail} failed / {len(selected) - fail} killed)")
    return 1 if fail else 0


if __name__ == "__main__":
    sys.exit(main())

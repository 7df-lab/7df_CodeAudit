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
        "id": "M1", "bug": "R1/ADR-209: repo clone 覆盖任务级/项目级 upload_file_id（守卫缺 r.Prepare==nil，两处同雷）",
        "file": "services/task-service/internal/service/task_service.go",
        "edits": [(
            'if r.ProjectPath == "" && r.Prepare == nil && task.GetProjectId() != "" {',
            'if r.ProjectPath == "" && task.GetProjectId() != "" {',
        )],
        "expect": 2,  # 任务级与项目级两处守卫同文案（ADR-209 双修）
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
        "expect": 2,  # 列表分支与 read 归属核验分支同文案（原缺陷即两处同雷）
        "module": "services/gateway-service",
        "run": "TestNotifications_UserIdFromJWTNotQuery",
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
        "id": "M25", "bug": "R30: 行投影 SELECT 删 reasoning → AI 结论/裁决理由读回恒空（ADR-195 链路点选永不渲染）",
        "file": "services/result-service/internal/repository/finding_repository.go",
        "edits": [(
            "SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, COALESCE(reasoning, '') AS reasoning, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id\n\t\tFROM findings WHERE id = $1",
            "SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id\n\t\tFROM findings WHERE id = $1",
        )],
        "module": "services/result-service",
        "run": "TestFindingRepoReasoningWired",
    },
    {
        "id": "M26", "bug": "R30: INSERT 删 reasoning → 创建期 AI 结论原文（[DSH-sandbox]/[LLM:]）不落库",
        "file": "services/result-service/internal/repository/finding_repository.go",
        "edits": [(
            'INSERT INTO findings (id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, reasoning, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id)',
            'INSERT INTO findings (id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id)',
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

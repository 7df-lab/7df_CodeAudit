# -*- coding: utf-8 -*-
"""
E2E 五模式垂直贯通测试（tests/e2e/test_e2e_four_modes.py；文件名保留 four_modes 兼容既有引用）

设计依据:
  - ADR-186 五模式矩阵（2026-09-03 人类决策；ADR-182 四模式矩阵的扩展）:
      A=SCAN_MODE_SAST_ONLY 纯SAST多工具并行→去重合并
      B=SCAN_MODE_AI_ONLY 纯AI
      C=SCAN_MODE_PARALLEL SAST+AI并行→融合去重（默认推荐）
      D=SCAN_MODE_AI_ENHANCED_SAST AI增强SAST：扫描→同段去重→逐条沙箱验证→SAST+AI判定汇总
      E=SCAN_MODE_COMPARE SAST+AI并行→三分桶同维度对比（ADR-186 前称"模式D"）
  - 旧值 TRADITIONAL_FIRST/SAST_REVIEW 已弃用（兼容分支保留，不在本套件覆盖）
  - test-gates.md §5 SMK 级别定义（本套件为 SMK-6 的全模式扩展）
  - 07 §1 F1≥0.85 目标在 F1 评估文件中单独承接

贯通路径（每个模式均为真实 gRPC 跨服务调用链）:
  gateway(可选) → task-service(编排) → sast-adapter(RunMultipleScans→真bandit) 
    → dsh-runtime(AnalyzeCode/RunAIAnalysis/Verify/Missed/Review→沙箱DSH；降级内置RuleScan ADR-175)
    → result-service(BatchCreateFindings/BatchUpdateVerdict/GenerateReport, memory store)
"""
import os
import subprocess
import time
import uuid

import grpc
import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
sys_path = os.path.join(ROOT, "libs", "proto-gen", "python")

import sys  # noqa: E402
sys.path.insert(0, sys_path)

import codeaudit_common_pb2 as pb  # noqa: E402
import codeaudit_common_pb2_grpc as pb_grpc  # noqa: E402

SAMPLE_PROJECT = os.path.join(ROOT, "tests", "samples", "python_flask")

ADDRS = {
    "task": "127.0.0.1:50054",      # ADR-113
    "sast": "127.0.0.1:50051",
    "dsh": "127.0.0.1:50057",       # ADR-114
    # ADR-175: ai-inference(50056) 已删除
    "result": "127.0.0.1:50058",    # ADR-117
}


def _channel(addr):
    return grpc.insecure_channel(addr)


@pytest.fixture(scope="session")
def services_up():
    """五服务真实进程就绪检查（由 scripts/e2e_up.py up 预先拉起）。"""
    for name, addr in ADDRS.items():
        ch = _channel(addr)
        try:
            grpc.channel_ready_future(ch).result(timeout=15)
        finally:
            ch.close()
    return True


def create_and_run(mode, tools, timeout=240):  # ADR-168 预算沿革见各用例；真沙箱 AI 用例显式给 600（ADR-183 补遗：补丁撰写回合 4~6min+）
    """完整走一遍: CreateScanTask→StartTask→轮询终态（ADR-171: 审批流废除）。"""
    tag = pb.ScanMode.Name(mode).lower().replace('scan_mode_', '')
    with _channel(ADDRS["task"]) as ch:
        stub = pb_grpc.TaskServiceStub(ch)
        created = stub.CreateScanTask(pb.CreateScanTaskRequest(
            metadata=pb.RequestMetadata(
                request_id=f"e2e-{tag}-{uuid.uuid4().hex[:8]}"),  # 幂等键即 taskID(03§2 服务端约定)
            project_id="e2e-project",
            scan_mode=mode,
            sast_tools=tools,
            config={"project_path": SAMPLE_PROJECT},
        ))
        task_id = created.task_id
        stub.StartTask(pb.StartTaskRequest(task_id=task_id))

        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            task = stub.GetScanTask(pb.GetScanTaskRequest(task_id=task_id))
            last = task.status
            if task.status in (
                pb.TASK_STATUS_COMPLETED, pb.TASK_STATUS_FAILED,
                pb.TASK_STATUS_DEAD, pb.TASK_STATUS_TIMEOUT,
                pb.TASK_STATUS_CANCELLED,
            ):
                break
            time.sleep(0.5)
        return task_id, last


def list_findings(task_id):
    with _channel(ADDRS["result"]) as ch:
        stub = pb_grpc.ResultServiceStub(ch)
        resp = stub.ListFindings(pb.ListFindingsRequest(
            task_id=task_id,
            pagination=pb.PaginationRequest(page_size=100),
        ))
        return list(resp.findings)


# ============================================================
# 模式A（ADR-182 重排）— 纯SAST：多工具并行 → 去重合并产出
# ============================================================
def test_mode_a_sast_only_end_to_end(services_up):
    # ADR-182: 多工具并行审计（sast-adapter RunMultipleScans 并行执行）
    task_id, status = create_and_run(pb.SCAN_MODE_SAST_ONLY, ["bandit", "opengrep"], timeout=300)
    assert status == pb.TASK_STATUS_COMPLETED, f"mode A ended {pb.TaskStatus.Name(status)}"

    findings = list_findings(task_id)
    assert len(findings) > 0, "mode A must persist SAST findings"
    tools = {f.source_tool for f in findings}
    assert tools <= {"bandit", "opengrep"}, f"pure SAST mode must not emit AI findings: {tools}"

    # bandit 在样本上必须命中（真实工具执行断言，沿用稳定锚点）
    bandit_hits = [f for f in findings if f.source_tool == "bandit"]
    assert len(bandit_hits) >= 3, f"bandit should hit >=3 on sample, got {len(bandit_hits)}"
    # SQL 注入样本点必须有工具覆盖（CWE-89）
    assert any("CWE-89" in f.cwe_id.upper() or "B608" in f.source_rule_id
               for f in bandit_hits), "SQLi sample point not detected by bandit"

    # 阶段矩阵（ADR-182）: sast → fusion(去重合并) → report，无 ai 阶段
    with _channel(ADDRS["task"]) as ch:
        task = pb_grpc.TaskServiceStub(ch).GetScanTask(pb.GetScanTaskRequest(task_id=task_id))
        stage_ids = {st.stage_id for st in task.stages}
    assert stage_ids == {"sast", "fusion", "report"}, f"mode A stages: {stage_ids}"


# ============================================================
# 模式B（ADR-182 重排）— 纯AI：沙箱 DSH 语义审计（原"模式A"流程不变）
# ============================================================
def test_mode_b_ai_only_end_to_end(services_up):
    # ADR-183 补遗: 补丁撰写让真实 LLM 回合达 4~6min+（产品侧已废本地预算，收敛只认
    # 流式 idle 信号，07 §8 30m 矩阵兜底）；测试预算 600s 覆盖慢回合+一次编排重试
    task_id, status = create_and_run(pb.SCAN_MODE_AI_ONLY, [], timeout=600)
    assert status == pb.TASK_STATUS_COMPLETED, f"mode B ended {pb.TaskStatus.Name(status)}"

    findings = list_findings(task_id)
    assert len(findings) > 0, "mode B must persist AI findings"
    for f in findings:
        assert f.source_tool == "ai_agent", f"unexpected tool {f.source_tool}"

    # 报告阶段
    with _channel(ADDRS["result"]) as ch:
        reports = pb_grpc.ReportServiceStub(ch).ListReports(
            pb.ListReportsRequest(task_id=task_id))
        assert len(reports.reports) >= 1, "mode B report missing"


# ============================================================
# 模式C（ADR-182 重排，默认推荐）— SAST+AI 并行 → 融合去重输出
# ============================================================
def test_mode_c_parallel_fusion_end_to_end(services_up):
    # ADR-182: SAST 工具组 ∥ AI 沙箱审计（~2min），双侧产出 → FuseResults 融合去重
    task_id, status = create_and_run(pb.SCAN_MODE_PARALLEL, ["bandit"], timeout=600)  # ADR-183 补遗: AI 回合含补丁撰写变长
    assert status == pb.TASK_STATUS_COMPLETED, f"mode C ended {pb.TaskStatus.Name(status)}"

    findings = list_findings(task_id)
    tools = {f.source_tool for f in findings}
    # 双源并存（SAST 侧 + AI 侧各自独立完成）
    assert "bandit" in tools, f"SAST findings missing: {tools}"
    assert "ai_agent" in tools, f"AI findings missing: {tools}"

    # 阶段矩阵: sast → ai → fusion → report（并行段 sast/ai 交叠）
    with _channel(ADDRS["task"]) as ch:
        task = pb_grpc.TaskServiceStub(ch).GetScanTask(pb.GetScanTaskRequest(task_id=task_id))
        stage_ids = {st.stage_id for st in task.stages}
    assert stage_ids == {"sast", "ai", "fusion", "report"}, f"mode C stages: {stage_ids}"


# ============================================================
# 模式D（ADR-186）— AI增强SAST：扫描→同段去重→逐条沙箱验证→融合汇总报告
# ============================================================
def test_mode_d_ai_enhanced_end_to_end(services_up):
    # ADR-186: SAST 全工具扫描（模式A 内容来源段）→ VerifySASTResults 逐条沙箱验证
    #（dsh-runtime 侧同文件同段去重：一组一轮沙箱）→ BatchUpdateVerdict 回写 → 融合 → 报告。
    # 验证轮次=去重后组数 × 真实 DSH 回合时长，预算对齐模式B（600s）再加验证轮余量。
    task_id, status = create_and_run(pb.SCAN_MODE_AI_ENHANCED_SAST, ["bandit", "opengrep"], timeout=900)
    assert status == pb.TASK_STATUS_COMPLETED, f"mode D ended {pb.TaskStatus.Name(status)}"

    findings = list_findings(task_id)
    assert len(findings) > 0, "mode D must persist SAST findings"
    tools = {f.source_tool for f in findings}
    assert tools <= {"bandit", "opengrep"}, f"AI-enhanced mode verifies SAST findings only: {tools}"
    # 每条 SAST 发现都应有 AI 判定回写（沙箱真判 or 降级 NEEDS_MANUAL，均非 UNSPECIFIED）
    for f in findings:
        assert f.ai_verdict != pb.AI_VERDICT_UNSPECIFIED, \
            f"finding {f.finding_id} lacks ai_verdict (verify verdict not written back)"

    # 阶段矩阵（ADR-186）: sast → ai(验证) → fusion → report
    with _channel(ADDRS["task"]) as ch:
        task = pb_grpc.TaskServiceStub(ch).GetScanTask(pb.GetScanTaskRequest(task_id=task_id))
        stage_ids = {st.stage_id for st in task.stages}
    assert stage_ids == {"sast", "ai", "fusion", "report"}, f"mode D stages: {stage_ids}"

    # 报告阶段（SAST+AI 判定汇总）
    with _channel(ADDRS["result"]) as ch:
        reports = pb_grpc.ReportServiceStub(ch).ListReports(
            pb.ListReportsRequest(task_id=task_id))
        assert len(reports.reports) >= 1, "mode D report missing"


# ============================================================
# 模式E（ADR-186 前称"模式D"）— SAST+AI 并行单独完成 → 三分桶同维度对比
# ============================================================
def test_mode_d_compare_end_to_end(services_up):
    task_id, status = create_and_run(pb.SCAN_MODE_COMPARE, ["bandit"], timeout=600)  # ADR-183 补遗: AI 回合含补丁撰写变长
    assert status == pb.TASK_STATUS_COMPLETED, f"mode E ended {pb.TaskStatus.Name(status)}"

    # 用返回的 ID 直接调 CompareResults 校验三分桶（编排器已调用过一次；此处独立复核）
    with _channel(ADDRS["sast"]) as ch:
        fusion = pb_grpc.SASTFusionServiceStub(ch)
        sast_ids = [f.finding_id for f in list_findings(task_id) if f.source_tool == "bandit"]
        ai_ids = [f.finding_id for f in list_findings(task_id) if f.source_tool == "ai_agent"]
        cmp = fusion.CompareResults(pb.CompareResultsRequest(
            task_id=task_id, sast_finding_ids=sast_ids, ai_finding_ids=ai_ids))
        s = cmp.summary
        # 三分桶不变量（ADR-182: 单SAST/单AI/SAST+AI；同位置去重后分桶）
        assert s.sast_total + s.ai_total > 0, "compare inputs empty"
        assert s.both_found + s.sast_only <= s.sast_total, "sast bucket overcounted"
        assert s.both_found + s.ai_only <= s.ai_total, "ai bucket overcounted"
        assert s.disagreement <= s.both_found, "disagreement is a subset of both_found"
        assert s.metrics.total_unique == s.sast_total + s.ai_total - s.both_found, "unique math broken"


# ============================================================
# 幂等与状态机反向抽查（test-gates.md §3 范围内关键项的 E2E 复核）
# ============================================================
def test_reverse_submit_approve_removed(services_up):
    """ADR-171: 审批流废除——SubmitTask/ApproveTask RPC 已从契约删除（UNIMPLEMENTED）。"""
    tag = f"e2e-rev-{uuid.uuid4().hex[:8]}"
    with _channel(ADDRS["task"]) as ch:
        stub = pb_grpc.TaskServiceStub(ch)
        # 存根层：方法已随契约删除
        assert not hasattr(stub, "SubmitTask")
        assert not hasattr(stub, "ApproveTask")
        assert not hasattr(stub, "RejectTask")
        created = stub.CreateScanTask(pb.CreateScanTaskRequest(
            metadata=pb.RequestMetadata(request_id=tag),
            project_id="e2e-project",
            scan_mode=pb.SCAN_MODE_AI_ONLY,
            config={"project_path": SAMPLE_PROJECT},
        ))
        task_id = created.task_id
        # 线上层：旧方法路径打回 UNIMPLEMENTED（不依赖已删除的生成存根，手构造调用）
        for method in ("SubmitTask", "ApproveTask", "RejectTask"):
            legacy = ch.unary_unary(
                f"/codeaudit.common.v1.TaskService/{method}",
                request_serializer=bytes,
                response_deserializer=bytes,
            )
            with pytest.raises(grpc.RpcError) as ei:
                legacy(b"")
            assert ei.value.code() == grpc.StatusCode.UNIMPLEMENTED
        # CREATED 直接 StartTask 合法（有创建权限即有启动权限）
        task = stub.StartTask(pb.StartTaskRequest(task_id=task_id))
        assert task.status == pb.TASK_STATUS_RUNNING


def test_reverse_create_without_idempotency_key(services_up):
    with _channel(ADDRS["task"]) as ch:
        stub = pb_grpc.TaskServiceStub(ch)
        with pytest.raises(grpc.RpcError) as ei:
            stub.CreateScanTask(pb.CreateScanTaskRequest(project_id="x"))
        assert ei.value.code() == grpc.StatusCode.INVALID_ARGUMENT  # R4


def test_result_stats_queryable_after_modes(services_up):
    """收尾统计锚点: 编排器的 stored_total 口径可被外部复核。"""
    # ADR-183 补遗: 本用例只验证 stats 口径可查，不重复烧 LLM 审计（真实 AI 回合含补丁
    # 撰写 3~10min 高方差，四模式 AI 覆盖已在各自专属用例）——切 SAST_ONLY 秒级完成，
    # 断言语义不变（完成态任务 + 落盘发现 + stats 可查）。
    task_id, status = create_and_run(pb.SCAN_MODE_SAST_ONLY, ["bandit"], timeout=300)
    assert status == pb.TASK_STATUS_COMPLETED
    with _channel(ADDRS["result"]) as ch:
        stats = pb_grpc.ResultServiceStub(ch).GetTaskResultStats(
            pb.GetTaskResultStatsRequest(task_id=task_id))
        total = sum(stats.by_verdict.values())
        assert total >= 1, "stats must reflect persisted findings"

# -*- coding: utf-8 -*-
"""
E2E F1 评估（TP11-T2 机制在本地样本上的真实执行；目标口径 07 §1 F1≥0.85）。

数据集状态: DiverseVul 缺失（TP09-T2 blocked 如实在案），本评估使用
tests/fixtures/diversevul_sample/ground_truth.json + 本仓库自建样本的标注，
评估对象为真实链路产出（bandit 扫描 + RuleScan 引擎），无 mock。

评估口径:
  - TP=工具与真值同文件同行(±2行)同 CWE 族；FP=工具报了真值没有的；FN=真值有工具没报的
  - precision=TP/(TP+FP), recall=TP/(TP+FN), F1=2PR/(P+R)
"""
import json
import os
import sys
import uuid

import grpc
import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
sys.path.insert(0, os.path.join(ROOT, "libs", "proto-gen", "python"))

import codeaudit_common_pb2 as pb  # noqa: E402
import codeaudit_common_pb2_grpc as pb_grpc  # noqa: E402

ADDRS = {"sast": "127.0.0.1:50051", "result": "127.0.0.1:50058"}  # ADR-175: ai(50056) 已删除
SAMPLE = os.path.join(ROOT, "tests", "samples", "python_flask")

# 真值标注（自建样本，来源=tests/samples/python_flask；CWE 族口径见文件头注释）
GROUND_TRUTH = [
    {"file": "app.py", "line": 9, "cwe": "CWE-798"},   # hardcoded token
    {"file": "app.py", "line": 16, "cwe": "CWE-89"},   # SQLi concat
    {"file": "app.py", "line": 23, "cwe": "CWE-78"},   # os.system concat
    {"file": "app.py", "line": 29, "cwe": "CWE-502"},  # yaml.load
    {"file": "util.py", "line": 7, "cwe": "CWE-78"},   # subprocess concat
]

# 非安全类规则（代码质量提示，非漏洞）：DiverseVul 口径下不计入评估（前置过滤）。
# B404=import 检查提示；severity=INFO 同理不计。
QUALITY_ONLY_RULES = {"B404"}


def eval_scope(findings):
    """评估前置过滤: 剔除代码质量提示与 INFO 级（非安全缺陷语义）。"""
    return [f for f in findings
            if f.source_rule_id not in QUALITY_ONLY_RULES
            and f.severity != pb.SEVERITY_INFO]


def run_bandit():
    """sast-adapter 真实执行 bandit；实体按 09 §2 已落盘 result-service，回读取全。"""
    task_id = f"f1-eval-{uuid.uuid4().hex[:6]}"
    with grpc.insecure_channel(ADDRS["sast"]) as ch:
        stub = pb_grpc.SASTAdapterServiceStub(ch)
        stub.RunSASTScan(pb.RunSASTScanRequest(
            metadata=pb.RequestMetadata(request_id=f"f1-bandit-{uuid.uuid4().hex[:6]}"),
            task_id=task_id,
            project_path=SAMPLE,
            tool_id="bandit",
        ))
    with grpc.insecure_channel(ADDRS["result"]) as ch:
        stub = pb_grpc.ResultServiceStub(ch)
        resp = stub.ListFindings(pb.ListFindingsRequest(
            task_id=task_id, pagination=pb.PaginationRequest(page_size=100)))
        return list(resp.findings)


# ADR-175: rulescan-only 基线入口随 ai-inference 删除（RuleScan 已内嵌 dsh-runtime 仅作降级兜底）
def match(findings):
    """计算 TP/FP/FN（±2 行窗口 + CWE 语义匹配；先过评估前置过滤）。"""
    findings = eval_scope(findings)

    # bandit 规则→CWE 族语义对齐（B105/B106/B107=硬编码凭据族, B6xx=注入族, B506=CWE-502）
    def tool_cwes(f):
        cwes = {(f.cwe_id or "").upper()}
        rid = f.source_rule_id or ""
        if rid in ("B105", "B106", "B107"):
            cwes.add("CWE-259")
        if rid.startswith("B60"):
            cwes.add("CWE-78")
        if rid == "B506":
            cwes.add("CWE-502")
        if rid == "B608":
            cwes.add("CWE-89")
        return cwes

    def families(cwes):
        out = set()
        for c in cwes:
            if c.startswith("CWE-") and "-" in c:
                out.add(c.split("-")[1])
        return out

    tp = 0
    for gt in GROUND_TRUTH:
        gt_fam = gt["cwe"].split("-")[1]
        gt_alt = {"798": {"798", "259"}, "78": {"78"}, "89": {"89"}, "502": {"502"}}[gt_fam]
        matched = False
        for f in findings:
            same_file = f.location.file_path.endswith(gt["file"])
            near = abs(f.location.start_line - gt["line"]) <= 2
            if same_file and near and families(tool_cwes(f)) & gt_alt:
                matched = True
                break
        if matched:
            tp += 1
    fp = len(findings) - tp
    fn = len(GROUND_TRUTH) - tp
    return tp, max(fp, 0), fn


def score(tp, fp, fn):
    p = tp / (tp + fp) if tp + fp else 0.0
    r = tp / (tp + fn) if tp + fn else 0.0
    f1 = 2 * p * r / (p + r) if p + r else 0.0
    return round(p, 3), round(r, 3), round(f1, 3)


@pytest.mark.order_last
def test_f1_evaluation_report(services_up=None):
    findings = run_bandit()
    tp, fp, fn = match(findings)
    p, r, f1 = score(tp, fp, fn)
    print(f"\n[F1] bandit-only:    TP={tp} FP={fp} FN={fn} P={p} R={r} F1={f1}")
    assert f1 >= 0.85, f"bandit-only F1={f1} < 0.85 (07 §1)"

    # ADR-175: rulescan-only 基线段已随 ai-inference 删除（RuleScan 仅作 dsh-runtime 降级兜底，无独立入口）
    # 归档证据
    ev = {
        "evaluation": "TP11-T2 F1 mechanism (local sample; DiverseVul blocked as recorded; ADR-175 rulescan baseline removed)",
        "target": "F1>=0.85 (07 §1)",
        "bandit": {"tp": tp, "fp": fp, "fn": fn, "precision": p, "recall": r, "f1": f1},
        "ground_truth_size": len(GROUND_TRUTH),
    }
    os.makedirs(".agent/evidence", exist_ok=True)
    with open(".agent/evidence/F1_e2e_report.json", "w") as f:
        json.dump(ev, f, indent=2)

#!/bin/bash
# ============================================================
# TP11-T2: F1 评估流程脚本
# 依据: 07 §1（M6目标 F1≥0.85）+ test-gates.md §5 SMK-6
# 
# 说明: DiverseVul 数据集缺失（TP09-T2 blocked），本脚本实现
#       评估流程与机制，使用替代标注集或内置样例验证流程正确性
# ============================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# ============================================================
# 配置
# ============================================================
EVIDENCE_DIR=".agent/evidence"
REPORT_FILE="${EVIDENCE_DIR}/F1_evaluation_report.json"
CSV_FILE="${EVIDENCE_DIR}/F1_evaluation_results.csv"

# 目标阈值（依据: 07 §1）
TARGET_F1="0.85"
TARGET_PRECISION="0.85"
TARGET_RECALL="0.90"

# ============================================================
# 数据集状态检查
# ============================================================
check_dataset() {
    log_info "检查数据集状态..."
    
    local dataset_found=false
    
    # 检查 DiverseVul 数据集
    local diversevul_paths=(
        "/data/diversevul"
        "/opt/diversevul"
        "/home/diversevul"
        "tests/fixtures/diversevul"
        "tests/fixtures/DiverseVul"
    )
    
    for path in "${diversevul_paths[@]}"; do
        if [ -d "$path" ] || [ -f "$path" ]; then
            log_info "找到数据集: $path"
            dataset_found=true
            DIVERSEVUL_PATH="$path"
            break
        fi
    done
    
    if [ "$dataset_found" = false ]; then
        log_warn "DiverseVul 数据集未找到（TP09-T2 已 blocked）"
        log_warn "使用内置样例数据集验证评估流程正确性"
        DIVERSEVUL_PATH="tests/fixtures/diversevul_sample"
        create_sample_dataset
    fi
    
    return 0
}

# ============================================================
# 创建样例数据集（用于流程验证）
# ============================================================
create_sample_dataset() {
    log_info "创建样例数据集..."
    
    mkdir -p tests/fixtures/diversevul_sample
    
    # 创建标注文件（模拟 DiverseVul 格式）
    cat > tests/fixtures/diversevul_sample/ground_truth.json <<'EOF'
{
  "version": "1.0",
  "description": "SMK-6 样例标注集（流程验证用）",
  "source": "内置样例（非真实 DiverseVul）",
  "samples": [
    {
      "id": "sample-001",
      "file": "tests/samples/python/app.py",
      "line": 7,
      "vulnerability_type": "SQL Injection",
      "cwe": "CWE-89",
      "severity": "HIGH",
      "is_vulnerable": true,
      "evidence": "cursor.execute(f\"SELECT * FROM users WHERE id = {user_id}\")"
    },
    {
      "id": "sample-002",
      "file": "tests/samples/python/app.py",
      "line": 3,
      "vulnerability_type": "None",
      "cwe": "None",
      "severity": "NONE",
      "is_vulnerable": false,
      "evidence": "import sqlite3"
    },
    {
      "id": "sample-003",
      "file": "tests/samples/python/app.py",
      "line": 4,
      "vulnerability_type": "None",
      "cwe": "None",
      "severity": "NONE",
      "is_vulnerable": false,
      "evidence": "def get_user(user_id):"
    }
  ]
}
EOF
    
    log_info "样例数据集创建完成: tests/fixtures/diversevul_sample/"
}

# ============================================================
# 运行扫描任务（模式B）
# ============================================================
run_scan() {
    log_info "运行扫描任务..."
    
    # 设置环境变量
    export GATEWAY_URL="${GATEWAY_URL:-http://localhost:18080}"
    export PROJECT_SERVICE_URL="${PROJECT_SERVICE_URL:-http://localhost:50052}"
    export TASK_SERVICE_URL="${TASK_SERVICE_URL:-http://localhost:50054}"
    export RESULT_SERVICE_URL="${RESULT_SERVICE_URL:-http://localhost:50055}"
    
    # 运行端到端测试（收集结果用于 F1 计算）
    python3 -c "
import json
import os
import sys
import time
import uuid
import requests
from datetime import datetime

GATEWAY_URL = os.getenv('GATEWAY_URL')
JWT_SECRET = os.getenv('JWT_SECRET', 'ci-test-secret')

def get_auth_headers():
    try:
        import jwt
        token = jwt.encode({'sub': 'eval-user', 'exp': int(time.time()) + 3600}, JWT_SECRET, algorithm='HS256')
    except ImportError:
        token = 'test-token-eval'
    return {'Authorization': f'Bearer {token}', 'Content-Type': 'application/json'}

# 创建项目
project_name = f'f1-eval-{uuid.uuid4().hex[:8]}'
project_resp = requests.post(f'{os.getenv(\"PROJECT_SERVICE_URL\")}/api/v1/projects', json={
    'name': project_name,
    'description': 'F1 evaluation',
    'language': 'python',
    'repository_url': 'https://github.com/example/test-repo.git',
    'config': {'sast_tools': ['semgrep', 'bandit'], 'scan_mode': 'TRADITIONAL_FIRST'}
}, headers=get_auth_headers(), timeout=10)
project_resp.raise_for_status()
project_id = project_resp.json().get('project_id', project_resp.json().get('id'))
print(f'Project created: {project_id}')

# 创建任务
task_id = str(uuid.uuid4())
task_resp = requests.post(f'{os.getenv(\"TASK_SERVICE_URL\")}/api/v1/tasks', json={
    'task_id': task_id,
    'project_id': project_id,
    'scan_mode': 'TRADITIONAL_FIRST',
    'sast_tools': ['semgrep', 'bandit'],
    'request_metadata': {'request_id': f'f1-eval-{task_id[:8]}', 'timestamp': datetime.utcnow().isoformat() + 'Z'}
}, headers=get_auth_headers(), timeout=10)
task_resp.raise_for_status()
print(f'Task created: {task_id}')

# 等待完成
timeout = 300
start = time.time()
while time.time() - start < timeout:
    resp = requests.get(f'{os.getenv(\"TASK_SERVICE_URL\")}/api/v1/tasks/{task_id}', headers=get_auth_headers(), timeout=10)
    resp.raise_for_status()
    status = resp.json().get('status')
    print(f'Status: {status}')
    if status == 'COMPLETED':
        break
    elif status in ['FAILED', 'DEAD', 'TIMEOUT']:
        print(f'Task failed: {status}')
        sys.exit(1)
    time.sleep(5)

# 获取结果
resp = requests.get(f'{os.getenv(\"RESULT_SERVICE_URL\")}/api/v1/findings', params={'task_id': task_id, 'page_size': 100}, headers=get_auth_headers(), timeout=10)
resp.raise_for_status()
findings = resp.json().get('findings', [])
print(f'Findings: {len(findings)}')

# 保存扫描结果
with open('${EVIDENCE_DIR}/scan_results.json', 'w') as f:
    json.dump({'task_id': task_id, 'findings': findings, 'timestamp': datetime.utcnow().isoformat() + 'Z'}, f, indent=2)
print('Scan results saved')
" 2>&1 | tee "${EVIDENCE_DIR}/scan_output.txt"
    
    return $?
}

# ============================================================
# 计算 F1 分数
# ============================================================
calculate_f1() {
    log_info "计算 F1 分数..."
    
    python3 <<'PYTHON_SCRIPT'
import json
import os
import sys
from datetime import datetime
from typing import Dict, List, Tuple

# 加载扫描结果
scan_results_file = ".agent/evidence/scan_results.json"
if not os.path.exists(scan_results_file):
    print("ERROR: 扫描结果文件不存在")
    sys.exit(1)

with open(scan_results_file, "r") as f:
    scan_results = json.load(f)

findings = scan_results.get("findings", [])

# 加载标注数据
ground_truth_file = "tests/fixtures/diversevul_sample/ground_truth.json"
if not os.path.exists(ground_truth_file):
    print("ERROR: 标注数据文件不存在")
    sys.exit(1)

with open(ground_truth_file, "r") as f:
    ground_truth = json.load(f)

samples = ground_truth.get("samples", [])

# ============================================================
# 计算指标（依据: 07 §1）
# ============================================================

def match_finding(finding: Dict, sample: Dict) -> bool:
    """检查扫描结果是否匹配标注"""
    # 简单匹配逻辑：检查文件路径和行号
    finding_file = finding.get("file_path", finding.get("path", ""))
    finding_line = finding.get("line_number", finding.get("start", {}).get("line", 0))
    
    sample_file = sample.get("file", "")
    sample_line = sample.get("line", 0)
    
    # 标准化路径
    if finding_file.startswith("./"):
        finding_file = finding_file[2:]
    if sample_file.startswith("./"):
        sample_file = sample_file[2:]
    
    return finding_file == sample_file and finding_line == sample_line

def calculate_metrics(findings: List[Dict], samples: List[Dict]) -> Tuple[float, float, float, Dict]:
    """
    计算精确率、召回率、F1 分数
    依据: 07 §1 质量指标基线
    """
    # 统计
    true_positives = 0
    false_positives = 0
    false_negatives = 0
    
    # 标记已匹配的样本
    matched_samples = set()
    
    # 检查每个扫描结果
    for finding in findings:
        matched = False
        for i, sample in enumerate(samples):
            if i not in matched_samples and match_finding(finding, sample):
                if sample.get("is_vulnerable", False):
                    true_positives += 1
                    matched_samples.add(i)
                matched = True
                break
        
        if not matched:
            # 检查是否与任何漏洞样本匹配
            is_false_positive = True
            for i, sample in enumerate(samples):
                if sample.get("is_vulnerable", False) and match_finding(finding, sample):
                    true_positives += 1
                    matched_samples.add(i)
                    is_false_positive = False
                    break
            
            if is_false_positive:
                false_positives += 1
    
    # 统计漏报
    for i, sample in enumerate(samples):
        if sample.get("is_vulnerable", False) and i not in matched_samples:
            false_negatives += 1
    
    # 计算指标
    precision = true_positives / (true_positives + false_positives) if (true_positives + false_positives) > 0 else 0.0
    recall = true_positives / (true_positives + false_negatives) if (true_positives + false_negatives) > 0 else 0.0
    f1 = 2 * precision * recall / (precision + recall) if (precision + recall) > 0 else 0.0
    
    details = {
        "true_positives": true_positives,
        "false_positives": false_positives,
        "false_negatives": false_negatives,
        "total_findings": len(findings),
        "total_samples": len(samples),
        "vulnerable_samples": sum(1 for s in samples if s.get("is_vulnerable", False))
    }
    
    return precision, recall, f1, details

# 计算指标
precision, recall, f1, details = calculate_metrics(findings, samples)

# ============================================================
# 生成报告（依据: 07 §1）
# ============================================================

report = {
    "evaluation": "F1 Evaluation",
    "timestamp": datetime.utcnow().isoformat() + "Z",
    "dataset": {
        "name": "diversevul_sample",
        "description": "内置样例数据集（流程验证用，非真实 DiverseVul）",
        "total_samples": details["total_samples"],
        "vulnerable_samples": details["vulnerable_samples"]
    },
    "scan_results": {
        "total_findings": details["total_findings"]
    },
    "metrics": {
        "precision": round(precision, 4),
        "recall": round(recall, 4),
        "f1": round(f1, 4)
    },
    "details": details,
    "targets": {
        "f1": 0.85,
        "precision": 0.85,
        "recall": 0.90
    },
    "status": {
        "f1": "PASS" if f1 >= 0.85 else "FAIL",
        "precision": "PASS" if precision >= 0.85 else "FAIL",
        "recall": "PASS" if recall >= 0.90 else "FAIL"
    },
    "blocked_reason": "DiverseVul 数据集缺失（TP09-T2 blocked），使用样例数据集验证流程",
    "note": "此为流程验证结果，非真实 DiverseVul F1 评估。真实评估需待数据集就绪后执行。"
}

# 保存报告
os.makedirs(".agent/evidence", exist_ok=True)
with open(".agent/evidence/F1_evaluation_report.json", "w") as f:
    json.dump(report, f, indent=2)

# 输出结果
print("=" * 60)
print("F1 评估结果（样例数据集）")
print("=" * 60)
print(f"精确率 (Precision): {precision:.4f} (目标: ≥0.85) {'✓' if precision >= 0.85 else '✗'}")
print(f"召回率 (Recall):    {recall:.4f} (目标: ≥0.90) {'✓' if recall >= 0.90 else '✗'}")
print(f"F1 分数:            {f1:.4f} (目标: ≥0.85) {'✓' if f1 >= 0.85 else '✗'}")
print("=" * 60)
print(f"总体状态: {'PASS' if f1 >= 0.85 else 'FAIL'}")
print(f"报告已保存: .agent/evidence/F1_evaluation_report.json")
print("=" * 60)

# 退出码
sys.exit(0 if f1 >= 0.85 else 1)
PYTHON_SCRIPT
    
    return $?
}

# ============================================================
# 生成 CSV 报告
# ============================================================
generate_csv_report() {
    log_info "生成 CSV 报告..."
    
    python3 <<'PYTHON_SCRIPT'
import json
import csv

# 加载报告
with open(".agent/evidence/F1_evaluation_report.json", "r") as f:
    report = json.load(f)

# 生成 CSV
csv_file = ".agent/evidence/F1_evaluation_results.csv"
with open(csv_file, "w", newline="") as f:
    writer = csv.writer(f)
    
    # 写入标题
    writer.writerow(["指标", "值", "目标", "状态"])
    
    # 写入数据
    writer.writerow(["精确率 (Precision)", report["metrics"]["precision"], "≥0.85", report["status"]["precision"]])
    writer.writerow(["召回率 (Recall)", report["metrics"]["recall"], "≥0.90", report["status"]["recall"]])
    writer.writerow(["F1 分数", report["metrics"]["f1"], "≥0.85", report["status"]["f1"]])
    
    # 写入详细信息
    writer.writerow([])
    writer.writerow(["统计项", "值"])
    writer.writerow(["总扫描结果数", report["details"]["total_findings"]])
    writer.writerow(["总样本数", report["details"]["total_samples"]])
    writer.writerow(["漏洞样本数", report["details"]["vulnerable_samples"]])
    writer.writerow(["真阳性 (TP)", report["details"]["true_positives"]])
    writer.writerow(["假阳性 (FP)", report["details"]["false_positives"]])
    writer.writerow(["假阴性 (FN)", report["details"]["false_negatives"]])
    
    # 写入阻塞原因
    writer.writerow([])
    writer.writerow(["阻塞原因", report.get("blocked_reason", "")])
    writer.writerow(["说明", report.get("note", "")])

print(f"CSV 报告已生成: {csv_file}")
PYTHON_SCRIPT
    
    return $?
}

# ============================================================
# 主流程
# ============================================================
main() {
    local start_time=$(date +%s)
    
    echo "============================================================"
    echo "TP11-T2: F1 评估流程"
    echo "依据: 07 §1（M6目标 F1≥0.85）"
    echo "============================================================"
    echo "开始时间: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    
    # 创建证据目录
    mkdir -p "${EVIDENCE_DIR}"
    
    # 检查数据集
    check_dataset
    
    # 运行扫描
    if ! run_scan; then
        log_error "扫描任务失败"
        return 1
    fi
    
    # 计算 F1
    if ! calculate_f1; then
        log_warn "F1 计算完成（可能未达到目标）"
    fi
    
    # 生成 CSV 报告
    generate_csv_report
    
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    
    echo ""
    echo "============================================================"
    echo "F1 评估完成"
    echo "耗时: ${duration}s"
    echo "结束时间: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    echo "============================================================"
    
    # 读取最终状态
    local f1_status=$(python3 -c "import json; print(json.load(open('.agent/evidence/F1_evaluation_report.json'))['status']['f1'])")
    
    if [ "$f1_status" = "PASS" ]; then
        log_info "F1 评估通过（样例数据集）"
        return 0
    else
        log_warn "F1 评估未通过（样例数据集）"
        return 1
    fi
}

# 运行主流程
main "$@"

#!/usr/bin/env bash
# openshell-manager 交付门禁（防回归机制第三层）。
#
#   bash .agent/verify.sh
#
# 内容 = 契约测试 + 守门测试（pytest 全量）+ 直跑模式（README 承诺的
# 第二种调用形态）+ deploy.sh 行为关卡（PATH 桩，C2-6）。任何红 = 不得
# 交付；修复缺陷须先加失败测试再修（流程见 REGRESSIONS.md 头部）。
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== [1/3] pytest：契约 + 守门 =="
python3 -m pytest tests/ -q

echo "== [2/3] 直跑模式（tests/test_contract.py 自身可执行口径）=="
python3 tests/test_contract.py > /dev/null
echo "direct-run mode: OK"

echo "== [3/3] deploy.sh 行为关卡（tests/deploy_cases.sh，PATH 桩）=="
bash tests/deploy_cases.sh

echo "== manager verify: ALL GREEN =="

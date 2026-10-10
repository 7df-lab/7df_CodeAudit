#!/usr/bin/env bash
# tests/smoke/run.sh — G5 冒烟套件入口（test-gates.md §5；.agent/verify.sh --milestone 调用）
# SMK-6 为 pytest 形态（test_smk6_mode_b_e2e.py）：前置 = docker 全栈 + Gateway REST 入口
# （默认 http://localhost:18080，可用 GATEWAY_URL 覆盖）。无栈/缺 requests 时用例自身
# 显式 SKIP（ADR-136：绝不 collected 0 假绿），-rA 让 SKIP 原因可见——
# 里程碑验收时应确认是真跑通过而非 SKIP。
# 用法: bash tests/smoke/run.sh [pytest 额外参数]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
exec python3 -m pytest tests/smoke -q --tb=short -rA -p no:cacheprovider "$@"

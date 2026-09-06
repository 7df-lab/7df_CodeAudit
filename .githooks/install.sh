#!/usr/bin/env bash
# 安装/更新本仓库的 git 钩子（新克隆后跑一次即可）
set -euo pipefail
cd "$(dirname "$0")/.."
git config core.hooksPath .githooks
echo "已启用 .githooks/pre-commit（提交前自动敏感信息清除与门禁）"

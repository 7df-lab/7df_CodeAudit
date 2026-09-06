#!/bin/bash
# Proto 代码生成脚本
# 用途：从 codeaudit_common.proto 生成 Go 和 Python 代码
# 遵循 R1：proto 是唯一数据契约，生成代码必须由 proto 派生
#
# 2026-08-22 (ADR-112): 由 buf BSR 远程插件改为本地工具链直生成。
# 原因: buf.gen.yaml 的 plugin: buf.build/* 版本随远端漂移, 且 CI 新容器需联网拉取——
# 网络抖动导致 generate 失败→fallback 到不同工具→diff 误报"生成物与源不一致"(gate 非确定性失败)。
# 本地路径版本锁定(镜像内): protoc 25.1 / protoc-gen-go v1.36.12 / protoc-gen-go-grpc 1.6.2 / grpcio-tools。

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
PROTO_DIR="$PROJECT_ROOT/proto"
OUTPUT_DIR="$PROJECT_ROOT/libs/proto-gen"
PROTO_FILE="$PROTO_DIR/codeaudit_common.proto"

echo "=== Proto 代码生成 ==="
echo "源目录: $PROTO_DIR"

# 工具链预检(AGENTS.md §2c): 必须在清理动作之前——工具缺失时不得破坏已入库生成物
for t in protoc protoc-gen-go protoc-gen-go-grpc; do
    command -v "$t" > /dev/null || { echo "错误: $t 未安装"; exit 1; }
done
python3 -c 'import grpc_tools' 2>/dev/null || { echo "错误: grpcio-tools 未安装"; exit 1; }

# 清理旧的生成代码
rm -rf "$OUTPUT_DIR/go" "$OUTPUT_DIR/python"
mkdir -p "$OUTPUT_DIR/go" "$OUTPUT_DIR/python"

# Go 代码: protoc + 本地插件(paths=source_relative 与原 buf 配置一致)
echo "生成 Go 代码 (protoc $(protoc --version | grep -oE '[0-9.]+$'), $(protoc-gen-go --version 2>/dev/null))..."
protoc \
    --plugin="protoc-gen-go=$(command -v protoc-gen-go)" \
    --plugin="protoc-gen-go-grpc=$(command -v protoc-gen-go-grpc)" \
    --go_out="$OUTPUT_DIR/go" --go_opt=paths=source_relative \
    --go-grpc_out="$OUTPUT_DIR/go" --go-grpc_opt=paths=source_relative \
    -I "$PROTO_DIR" "$PROTO_FILE"

# Python 代码: grpc_tools.protoc(import 形态与既有生成物一致: 平铺模块名)
echo "生成 Python 代码..."
PYTHONPATH="$PROTO_DIR" python3 -m grpc_tools.protoc \
    --python_out="$OUTPUT_DIR/python" --grpc_python_out="$OUTPUT_DIR/python" \
    -I "$PROTO_DIR" "$PROTO_FILE"

# 验证生成结果
echo ""
echo "=== 生成结果 ==="
echo "Go 代码:"
find "$OUTPUT_DIR/go" -name "*.go" | head -10
echo ""
echo "Python 代码:"
find "$OUTPUT_DIR/python" -name "*.py" | head -10
echo ""
echo "=== 生成完成 ==="

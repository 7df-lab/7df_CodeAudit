#!/bin/bash
# Proto 工具安装脚本
# 用途：安装 buf 和 protoc，用于 proto 代码生成

set -e

echo "=== 安装 Proto 工具 ==="

# 安装 buf
install_buf() {
    echo "安装 buf..."
    if command -v buf &> /dev/null; then
        echo "buf 已安装: $(buf --version)"
        return 0
    fi

    # 使用官方安装方法
    BIN_DIR="/usr/local/bin"
    curl -sSL "https://github.com/bufbuild/buf/releases/latest/download/buf-$(uname -s)-$(uname -m)" -o "${BIN_DIR}/buf"
    chmod +x "${BIN_DIR}/buf"
    echo "buf 安装完成: $(buf --version)"
}

# 安装 protoc
install_protoc() {
    echo "安装 protoc..."
    if command -v protoc &> /dev/null; then
        echo "protoc 已安装: $(protoc --version)"
        return 0
    fi

    PROTOC_VERSION="25.1"
    ARCH=$(uname -m)
    OS=$(uname -s)

    if [ "$OS" = "Linux" ]; then
        if [ "$ARCH" = "x86_64" ]; then
            PROTOC_ZIP="protoc-${PROTOC_VERSION}-linux-x86_64.zip"
        elif [ "$ARCH" = "aarch64" ]; then
            PROTOC_ZIP="protoc-${PROTOC_VERSION}-linux-aarch_64.zip"
        fi
    elif [ "$OS" = "Darwin" ]; then
        if [ "$ARCH" = "x86_64" ]; then
            PROTOC_ZIP="protoc-${PROTOC_VERSION}-osx-x86_64.zip"
        elif [ "$ARCH" = "arm64" ]; then
            PROTOC_ZIP="protoc-${PROTOC_VERSION}-osx-aarch_64.zip"
        fi
    fi

    if [ -z "$PROTOC_ZIP" ]; then
        echo "错误: 不支持的平台 $OS $ARCH"
        return 1
    fi

    curl -OL "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/${PROTOC_ZIP}"
    sudo unzip -o "${PROTOC_ZIP}" -d /usr/local bin/protoc 'include/*'
    rm -f "${PROTOC_ZIP}"
    echo "protoc 安装完成: $(protoc --version)"
}

# 安装 Go protoc 插件
install_go_protoc_plugins() {
    echo "安装 Go protoc 插件..."
    go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
    echo "Go protoc 插件安装完成"
}

# 安装 Python grpc 工具
install_python_grpc_tools() {
    echo "安装 Python gRPC 工具..."
    pip install grpcio grpcio-tools
    echo "Python gRPC 工具安装完成"
}

# 主流程
main() {
    install_buf
    install_protoc
    install_go_protoc_plugins
    install_python_grpc_tools
    echo ""
    echo "=== 所有 Proto 工具安装完成 ==="
}

main "$@"

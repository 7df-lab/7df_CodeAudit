# sast-adapter-service 镜像 —— ADR-198: SAST 工具链烘焙进镜像（不再依赖宿主机）。
#
# 运行时 = python:3.12-slim（debian/glibc）：vendor 的 opengrep 二进制动态链接
# glibc（见 vendor/PROVENANCE.md），alpine/musl 不可用；python 提供 bandit。
# 工具清单与调用口径（configs/codeaudit.yaml sast_tools 段）：
#   opengrep 1.29.0  → /usr/local/bin/opengrep（argv 直达；规则 {rules}=services/sast-adapter-service/rules）
#   bandit  1.9.4    → pip 安装，console script `bandit` 在 PATH（argv 直达，
#                      python_module/pythonpath 兜底仅在裸机 .toolchain 场景生效）
# 仓库根布局标记：findRulesDir 从 CWD 向上找 configs/codeaudit.yaml + rules/
# （此副本仅作路径标记；运行时配置值经 compose 挂载 /etc/codeaudit/codeaudit.yaml）。
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
WORKDIR /app/services/sast-adapter-service
ENV GOPROXY=https://goproxy.cn,direct
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /service ./cmd/main.go

FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates tzdata git && \
    rm -rf /var/lib/apt/lists/* && \
    pip install --no-cache-dir -i https://pypi.tuna.tsinghua.edu.cn/simple \
      bandit==1.9.4

COPY services/sast-adapter-service/tools/opengrep /usr/local/bin/opengrep
RUN chmod 0755 /usr/local/bin/opengrep && opengrep --version && bandit --version

COPY configs/codeaudit.yaml /app/configs/codeaudit.yaml
COPY services/sast-adapter-service/rules/ /app/services/sast-adapter-service/rules/
COPY --from=builder /service /usr/local/bin/service

WORKDIR /app
EXPOSE 50051
ENTRYPOINT ["service"]

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
#
# builder 段为 ADR-233 统一模板（上下文=仓库根由 .dockerignore 收窄；依赖层先行+
# BuildKit 缓存挂载；GOPROXY 经 ARG 可覆盖）；运行时二进制名与各服务一致为服务名
# （原泛名 `service` 退役）。
FROM golang:1.22-alpine AS builder
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src

COPY libs/proto-gen/go/go.mod libs/proto-gen/go/go.sum libs/proto-gen/go/
COPY libs/common-go/go.mod libs/common-go/go.sum libs/common-go/
COPY libs/go-config/go.mod libs/go-config/go.sum libs/go-config/
COPY services/sast-adapter-service/go.mod services/sast-adapter-service/go.sum services/sast-adapter-service/

WORKDIR /src/services/sast-adapter-service
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/sast-adapter-service ./cmd

FROM python:3.12-slim
# pip 源经 ARG 可覆盖（发布工程旋钮，release-sanitize-map 旋钮 6）
ARG PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates tzdata git && \
    rm -rf /var/lib/apt/lists/* && \
    pip install --no-cache-dir -i ${PIP_INDEX_URL} \
      bandit==1.9.4

COPY services/sast-adapter-service/tools/opengrep /usr/local/bin/opengrep
RUN chmod 0755 /usr/local/bin/opengrep && opengrep --version && bandit --version

COPY configs/codeaudit.yaml /app/configs/codeaudit.yaml
COPY services/sast-adapter-service/rules/ /app/services/sast-adapter-service/rules/
COPY --from=builder /out/sast-adapter-service /usr/local/bin/sast-adapter-service

WORKDIR /app
EXPOSE 50051
ENTRYPOINT ["sast-adapter-service"]

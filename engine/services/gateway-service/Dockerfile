# Multi-stage build: golang:1.22-alpine → alpine:3.19 (CGO_ENABLED=0 static build)
# 依据: 09 §1 gateway-service 资源规格 / ADR-113 port 8080
# 构建上下文=仓库根（compose: context: .，go.mod replace ../../libs 相对路径依赖整仓），
# 由根 .dockerignore 收窄；依赖层先行+BuildKit 缓存挂载（ADR-233 统一模板）。
FROM golang:1.22-alpine AS builder

# GOPROXY 经 ARG 可覆盖（发布工程旋钮，release-sanitize-map 旋钮 6）；缺省=CI 直连惯例
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# 依赖层先行：仅 go.mod/go.sum 变更触发模块重新解析下载；缓存挂载跨构建/跨服务复用
COPY libs/proto-gen/go/go.mod libs/proto-gen/go/go.sum libs/proto-gen/go/
COPY libs/common-go/go.mod libs/common-go/go.sum libs/common-go/
COPY libs/go-config/go.mod libs/go-config/go.sum libs/go-config/
COPY services/gateway-service/go.mod services/gateway-service/go.sum services/gateway-service/

WORKDIR /src/services/gateway-service
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 源码层（COPY 目的地显式 /src，不受当前 WORKDIR 影响）
COPY . /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/gateway-service ./cmd

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /out/gateway-service /usr/local/bin/gateway-service

EXPOSE 8080

ENTRYPOINT ["gateway-service"]

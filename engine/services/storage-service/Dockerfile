# Multi-stage build: golang:1.22-alpine → alpine:3.19 (CGO_ENABLED=0 static build)
# 依据: 09 §1 storage-service 资源规格
# 构建上下文=仓库根（compose: context: .，go.mod replace ../../libs 相对路径依赖整仓），
# 由根 .dockerignore 收窄；依赖层先行+BuildKit 缓存挂载（ADR-233 统一模板）。
FROM golang:1.22-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

COPY libs/proto-gen/go/go.mod libs/proto-gen/go/go.sum libs/proto-gen/go/
COPY libs/common-go/go.mod libs/common-go/go.sum libs/common-go/
COPY libs/go-config/go.mod libs/go-config/go.sum libs/go-config/
COPY services/storage-service/go.mod services/storage-service/go.sum services/storage-service/

WORKDIR /src/services/storage-service
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/storage-service ./cmd

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /out/storage-service /usr/local/bin/storage-service

EXPOSE 50055

ENTRYPOINT ["storage-service"]

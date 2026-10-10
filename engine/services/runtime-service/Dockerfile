# build context = 仓库根(compose: context: .); go.mod replace ../../libs 相对路径依赖整仓
# 由根 .dockerignore 收窄；依赖层先行+BuildKit 缓存挂载（ADR-233 统一模板）。
# 入口=ENTRYPOINT（原 CMD 为历史不一致残留，本仓无任何 compose/脚本以 command 覆写
# 本服务，统一为与其余 6 服务同款 ENTRYPOINT）。
FROM golang:1.22-alpine AS builder
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src

COPY libs/proto-gen/go/go.mod libs/proto-gen/go/go.sum libs/proto-gen/go/
COPY libs/common-go/go.mod libs/common-go/go.sum libs/common-go/
COPY libs/go-config/go.mod libs/go-config/go.sum libs/go-config/
COPY services/runtime-service/go.mod services/runtime-service/go.sum services/runtime-service/

WORKDIR /src/services/runtime-service
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/runtime-service ./cmd

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /out/runtime-service /usr/local/bin/runtime-service
EXPOSE 50057
ENTRYPOINT ["runtime-service"]

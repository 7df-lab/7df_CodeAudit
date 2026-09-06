# build context = 仓库根(compose: context: .); 与gateway同款: 整仓拷贝+服务目录内构建
# (go.mod replace ../../libs/proto-gen/go 依赖相对路径, 整仓拷贝天然满足)
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
WORKDIR /app/services/dsh-runtime-service
ENV GOPROXY=https://goproxy.cn,direct
RUN CGO_ENABLED=0 go build -o /dsh-runtime-service ./cmd/main.go

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /dsh-runtime-service /usr/local/bin/
EXPOSE 50057
CMD ["dsh-runtime-service"]

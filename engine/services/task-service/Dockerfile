# Multi-stage build: golang:1.22-alpine → alpine (CGO_ENABLED=0 static build)
# 依据: 09 §1 task-service 资源规格 / ADR-113 port 50054
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy the entire repo for module resolution (replace directives need ../../libs)
COPY . .

# Build from the service directory
WORKDIR /app/services/task-service
ENV GOPROXY=https://goproxy.cn,direct
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /task-service ./cmd/main.go

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /task-service /usr/local/bin/task-service

EXPOSE 50054

ENTRYPOINT ["task-service"]

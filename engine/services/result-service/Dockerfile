# Multi-stage build: golang:1.22-alpine → alpine (CGO_ENABLED=0 static build)
# 依据: 09 §1 result-service 资源规格 / ADR-114 port 50058
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy the entire repo for module resolution
COPY . .

# Build from the service directory
WORKDIR /app/services/result-service
ENV GOPROXY=https://goproxy.cn,direct
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /result-service ./cmd/main.go

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /result-service /usr/local/bin/result-service

EXPOSE 50058

ENTRYPOINT ["result-service"]

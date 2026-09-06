.PHONY: all build build-go test test-go test-contract lint lint-go proto verify clean help

# 7 服务全部为 Go（dsh-runtime 唯一入口 = cmd/main.go；requirements.txt 为 FastAPI
# 时代遗留文件，不再是构建输入）。历史 build-python/test-python/lint-python 段
# 随 dsh-runtime 的 Python 实现退役而删除（2026-09-05）。
GO_SERVICES := gateway-service project-service task-service result-service storage-service sast-adapter-service dsh-runtime-service

# 默认目标
all: verify build test

# 构建所有服务
build: build-go

build-go:
	@echo "Building Go services..."
	@for svc in $(GO_SERVICES); do \
		echo "  Building $$svc..."; \
		( cd services/$$svc && GOPROXY=https://goproxy.cn,direct go build ./... ) || exit 1; \
	done

# 运行测试
test: test-go test-contract

test-go:
	@echo "Running Go tests..."
	@for svc in $(GO_SERVICES); do \
		echo "  Testing $$svc..."; \
		( cd services/$$svc && GOPROXY=https://goproxy.cn,direct go test ./... ) || exit 1; \
	done

# 契约测试：fixture_server 假 gRPC 服务口径
test-contract:
	@echo "Running contract tests..."
	@CODEAUDIT_FIXTURE_PORT=50071 python3 -m pytest tests/contract -q --tb=short -p no:cacheprovider

# 代码检查
lint: lint-go

lint-go:
	@echo "Linting Go code..."
	@for svc in $(GO_SERVICES); do \
		( cd services/$$svc && golangci-lint run ./... 2>/dev/null || true ); \
	done

# Proto 生成（SSOT=根目录 codeaudit_common.proto；现行主路径见 scripts/generate-proto.sh, ADR-112）
proto:
	@echo "Generating proto code..."
	@cd proto && buf generate

# 验证（红线检查）
verify:
	@bash scripts/check-proto-sync.sh

# 清理
clean:
	@echo "Cleaning build artifacts..."
	@find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	@find . -type f -name "*.pyc" -delete 2>/dev/null || true
	@echo "Done."

# 帮助
help:
	@echo "CodeAudit Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  all           - Run verify, build, and test"
	@echo "  build         - Build all services (Go)"
	@echo "  build-go      - Build Go services (7 services, module per service)"
	@echo "  test          - Run all tests (Go + contract)"
	@echo "  test-go       - Run Go tests"
	@echo "  test-contract - Run contract tests (fixture_server, port 50071)"
	@echo "  lint          - Run linters (Go)"
	@echo "  proto         - Generate proto code (buf; SSOT=codeaudit_common.proto)"
	@echo "  verify        - Run SSOT red-line checks (proto sync)"
	@echo "  clean         - Clean build artifacts"
	@echo "  help          - Show this help"

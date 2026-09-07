.PHONY: all build build-go test test-go test-contract test-guardrails test-mutation lint lint-go proto verify clean help

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
test: test-go test-contract test-guardrails

test-go:
	@echo "Running Go tests..."
	@for svc in $(GO_SERVICES); do \
		echo "  Testing $$svc..."; \
		( cd services/$$svc && GOPROXY=https://goproxy.cn,direct go test ./... ) || exit 1; \
	done

# 契约测试：与 .agent/verify.sh G4 同一口径（fixture_server 假 gRPC 服务）
test-contract:
	@echo "Running contract tests..."
	@CODEAUDIT_FIXTURE_PORT=50071 python3 -m pytest tests/contract -q --tb=short -p no:cacheprovider

# 代码检查
# 守门测试（结构级：路由表/proto覆盖/台账/端口 ↔ docs 契约三件套，REGRESSIONS.md 第二层）
test-guardrails:
	@echo "Running guardrail tests..."
	@python3 -m pytest tests/test_guardrails.py -q --tb=short -p no:cacheprovider

# 变异自检（反回归牙齿证明：历史 bug 等价再引入→锁定测试必须红，REGRESSIONS.md 第三层）
test-mutation:
	@echo "Running mutation self-check..."
	@python3 tests/mutation/run_mutations.py

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
	@bash .agent/verify.sh

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
	@echo "  test           - Run all tests (Go + contract + guardrails)"
	@echo "  test-guardrails - Run guardrail tests (docs/code structural locks)"
	@echo "  test-mutation  - Run mutation self-check (regression teeth proof)"
	@echo "  test-go       - Run Go tests"
	@echo "  test-contract - Run contract tests (fixture_server, port 50071)"
	@echo "  lint          - Run linters (Go)"
	@echo "  proto         - Generate proto code (buf; SSOT=codeaudit_common.proto)"
	@echo "  verify        - Run SSOT red-line checks (.agent/verify.sh)"
	@echo "  clean         - Clean build artifacts"
	@echo "  help          - Show this help"

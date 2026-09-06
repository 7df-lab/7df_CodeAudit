# CodeAudit Umbrella — 并行开发工作区
# 模式：每个子目录 = 完整独立仓库（跟踪各自 origin 主干），日常开发直接在子目录进行；
#       伞仓跟随各仓主干持续更新（make pull），pin 仅作为可选的"部署/对账锚点"。
# 开发提交发生在子仓；伞仓默认不需要提交。

SUBLEVELS := engine web vscode-plugin dsh-runtime manager openshell-gateway dsh-pentest-sse
GIT_TLS := -c http.sslVerify=false

.PHONY: help status pull pin update test-web build-web test-engine deploy-sim test-sim down-sim destroy-sim logs-sim sanitize sanitize-check hooks

help:
	@echo "make status          -- 各子仓 一览（分支/领先落后/未提交/未推送）"
	@echo "make pull            -- 跟随各仓主干：全部子仓 pull --ff-only（日常同步入口）"
	@echo "make update          -- 新克隆引导：init + 按各仓主干检出（含递归）"
	@echo "make pin MSG=\"...\"  -- 可选：把当前各子仓 HEAD 记录进伞仓（部署/对账锚点）"
	@echo "make sanitize        -- 敏感信息清除（真实内网地址/域名 → 占位符）"
	@echo "make sanitize-check  -- 提交前门禁：硬违例阻断 + 人工确认清单"
	@echo "make hooks           -- 安装 pre-commit 钩子（新克隆跑一次）"
	@echo "make test-web       -- 前端 vitest 全量"
	@echo "make build-web      -- 前端 tsc+vite 生产构建"
	@echo "make test-engine    -- engine（平台后端）make verify 门禁"

# 敏感信息清除与门禁（push 前标准流程: make sanitize && make sanitize-check）
sanitize:
	@bash sanitize.sh fix

sanitize-check:
	@bash sanitize.sh check

hooks:
	@bash .githooks/install.sh

# 对账一览：分支 × 领先/落后 upstream × 未提交/未推送
status:
	@echo "== 各子仓工作树（跟随状态）=="
	@for s in $(SUBLEVELS); do \
		branch=$$(git -C $$s branch --show-current); \
		dirty=$$(git -C $$s status --porcelain 2>/dev/null | wc -l | tr -d ' '); \
		ahead=$$(git -C $$s rev-list --count origin/$$branch..$$branch 2>/dev/null || echo '?'); \
		behind=$$(git -C $$s rev-list --count $$branch..origin/$$branch 2>/dev/null || echo '?'); \
		echo "$$s [$$branch] 落后=$$behind 领先=$$ahead 未提交=$$dirty"; \
	done
	@echo ""
	@echo "== 伞仓记录的 pin（仅作部署/对账锚点，非开发态）=="
	@git submodule status

# 并行会话互斥一览（认领协议见 AGENTS.md §3）

# 日常同步入口：全部子仓快进到各自 origin 主干（本地领先时拒绝——先推子仓）
pull:
	@for s in $(SUBLEVELS); do \
		echo "[pull] $$s"; git $(GIT_TLS) -C $$s pull --ff-only || exit 1; \
	done
	@git submodule status

# 新克隆引导（本仓 clone 后第一步）
update:
	git $(GIT_TLS) submodule update --init --recursive
	@$(MAKE) pull

# 可选锚点：记录当前各子仓 HEAD 到伞仓历史（部署版本集/审计用）
pin:
ifdef MSG
	@git add engine web vscode-plugin dsh-runtime manager openshell-gateway dsh-pentest-sse
	@git -c user.name="RoyTse" -c user.email="roytse@codeaudit.local" commit -m "pin: $(MSG)"
	@echo "已记录 pin——push 需手动执行（见 README）"
else
	@echo '用法: make pin MSG="部署/对账说明"'
endif

test-web:
	cd web && npm test

build-web:
	cd web && npm run build

# 注意：Go 工具链在 engine/.toolchain（gitignored 本地目录，自定位 env.sh）；
# 配方显式 bash——make 默认 /bin/sh(dash) 不支持 env.sh 的 BASH_SOURCE 自定位
test-engine:
	cd engine && bash -c '. .toolchain/env.sh && make verify'

# ---- 生产模拟环境（deploy/，宿主机裸栈直跑退役）----
.PHONY: deploy-sim test-sim down-sim destroy-sim logs-sim

deploy-sim:
	bash deploy/sim.sh up

test-sim:
	bash deploy/tests/run.sh

down-sim:
	bash deploy/sim.sh down

destroy-sim:
	bash deploy/sim.sh destroy

logs-sim:
	bash deploy/sim.sh logs

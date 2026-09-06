# 开发 / 测试 / 生产全景图(dev-prod-map)

> 2026-09-05 立档:主仓 + 7 个子仓 + deploy 的开发、测试、生产**流程、配置与文件**一站式速查。
> AI 会话的入口与红线在 [AGENTS.md](../AGENTS.md),步骤级操作手册在 [docs/playbooks/](playbooks/)——
> 本文是它们引用的**事实地图**(命令/端口/SSOT 的单一事实源)。
> 改部署、动配置、跑迁移前先读本文;可复发问题档案另见 [LESSONS.md](../LESSONS.md),
> 部署纪律与自检清单见 [deploy/README.md](../deploy/README.md)。
> 本文描述的是 2026-09-05 当日实测状态;子仓演进后随改(改动入口命令/配置文件/端口口径时必须同步本文)。

---

## 1. 总体模式

**伞仓 = 并行开发工作区**:7 个子目录各自是完整独立 Git 仓(GitLab `gitlab.local/codeaudit` 组),
日常开发/提交/推送直接在子目录内进行(可并行开多个会话);伞仓只做总览与同步
(`make status` / `make pull`),`make pin MSG="..."` 仅在记录部署/对账版本集时使用。

**三层环境口径**(deploy/README.md;人类指令 2026-09-05:不允许宿主机裸栈开发测试,一律容器):

| 层级 | 入口 | 用途 | 栈 |
|------|------|------|----|
| ① 单元/仓库内测试 | 各仓自带命令(下文逐仓列) | 逻辑回归,无栈依赖 | 无 |
| ② 生产模拟栈 | 伞仓 `make deploy-sim` + `make test-sim` | 集成/功能/部署验收 | project=codeaudit-sim,网段 10.10.210.0/24,发布端口 18080/18088 |
| ③ 生产 | `bash deploy/sandbox-deploy.sh all`(目标 LXC 107) | 现役环境 | 网段 10.10.110.0/24,gateway 发布 8090 |

②③ 共用同一服务定义事实源 `engine/docker-compose.yml`,差异全部收敛在 overlay + env——
**"在模拟里通过"才对生产有证明力**。

**运行时拓扑**(消费链):

```
浏览器 ── web(nginx SPA + /v1 反代,18088/80) ── engine gateway(:8080 REST+WS,对外唯一入口)
                                                    ├─ 6 个 gRPC 微服务 + PG/Redis/MinIO/Kafka
                                                    └─ dsh-runtime(:50057)
                                                          ↓ HTTP/JSON + Bearer(唯一南向通道,ADR-174)
                                                    manager(:18800,FastAPI 纯管道)
                                                          ↓ gRPC
                                                    openshell-gateway(:8080,DooD 按需拉起)
                                                          ↓
                                                    沙箱容器(镜像 dsh-pentest-sse:latest,
                                                    内跑 dsh-runtime + bridge.mjs JSON-RPC⇄SSE)

vscode-plugin ──(codeaudit.serverUrl 配置)──> engine gateway REST/WS 直连
```

---

## 2. 逐仓速查

### 2.1 engine(平台引擎,Go 1.22,7 微服务)

| 维度 | 内容 |
|------|------|
| **开发** | 先 `source .toolchain/env.sh`(Go 工具链在 gitignored 本地目录);`make build`(7 服务逐 module,根目录无 go.mod,go 命令必须逐服务目录执行);proto SSOT=根目录 `codeaudit_common.proto`,生成走 `scripts/generate-proto.sh`(ADR-112),一致性 `scripts/check-proto-sync.sh` |
| **测试** | 交付门禁 `bash .agent/verify.sh`(G1 SSOT 红线 7 检查 + G2 Go 逐 module 单测 + pytest + G4 契约测试经 fixture_server:50071);`--milestone` 加 G5 冒烟(`tests/smoke/run.sh`,2026-09-05 补齐:无栈时用例如实 SKIP,ADR-136 口径);真 gRPC 全链 e2e `tests/e2e/`(15~30min,手动);Makefile `test-contract` 与 verify.sh G4 同口径 |
| **生产** | 每服务一份 `services/<svc>/Dockerfile`(构建上下文=仓库根,proto-gen 走相对 replace);sast-adapter 特殊:python:3.12-slim + vendored opengrep(见 §4 SSOT)+ bandit;配置唯一承载 `configs/codeaudit.yaml`(ADR-137,缺键启动 panic,env `CODEAUDIT_*` 覆盖);API 面 `/health` + `/v1/auth|uploads|projects|tasks|findings|reports|tools|notifications|users`(日志→限流 50/min→JWT) |
| **关键文件** | `configs/codeaudit.yaml`、`codeaudit_common.proto`、`proto/buf.gen.yaml`、`docker-compose.yml`(本地编排=三层共用事实源)、`Makefile`、`.agent/verify.sh`、`.agent/test-gates.md`、`.toolchain/env.sh`、`services/*/Dockerfile`(×7)、`scripts/init-db.sql`(仅模拟栈 seed 用,生产只建空库不建表) |

注:dsh-runtime 唯一入口 = `cmd/main.go`(Go);`requirements.txt` 为 FastAPI 时代遗留,不再是构建输入。
2026-09-05 已删 Makefile 过时的 build-python/test-python/lint-python 段,并把 dsh-runtime-service 补进 Go 构建/测试循环。

### 2.2 web(前端控制台,React 18 + antd 5 + Vite)

| 维度 | 内容 |
|------|------|
| **开发** | `npm run dev`(Vite 5173;`/v1` 代理到 `CODEAUDIT_GATEWAY_URL`,缺省 `http://localhost:8080`,WS 透传) |
| **测试** | `npm test`(Vitest + jsdom,19 文件约 86 用例;`src/testsupport/fakeGateway.ts` 在 axios adapter 层造假网关);类型门禁在 `npm run build` 内(`tsc -b`);无 eslint。伞仓入口 `make test-web` / `make build-web` |
| **生产** | `npm run build` → `dist/`;Dockerfile 多阶段(node:20-alpine → nginx:1.27-alpine);`nginx/default.conf.template`:SPA 托管 + `/v1` 反代(WS 升级、300s 超时、`proxy_buffering off`、上传 30m 上限);模拟栈内 console 是栈内服务(18088);**生产 console 经伞仓部署链发布**(`web/deploy.sh`,8088→80,2026-09-05 前为 CD 单独发布/宿主机 vite preview 裸跑) |
| **关键文件** | `package.json`、`vite.config.ts`、`tsconfig.json`、`Dockerfile`、`nginx/default.conf.template`、`docker-compose.yml`(单仓自起用)、`src/test-setup.ts` |

### 2.3 vscode-plugin(VS Code 插件)

| 维度 | 内容 |
|------|------|
| **开发** | `npm run compile / watch`(tsc → `out/`,CommonJS);无 dev server,连接靠 VS Code 配置项 `codeaudit.serverUrl`(缺省 `http://localhost:8080`)直连网关 REST+WS;需 Node ≥ 20 |
| **测试** | `npm test`(Mocha 11 + vscode 模块桩,约 125 用例,大头 applyPatch 44 / diffParse 26);真实链路脚本 `test/smoke.js`、`test/fixflow.e2e.js`、`test/mockGateway.js`(需网关在线) |
| **生产/打包** | `npm run package`(vsce + `scripts/verify-vsix.js` 关卡:强制校验 VSIX 内含 node_modules/adm-zip;勿用 `--no-dependencies`);产物 `codeaudit-vscode-0.1.0.vsix` |
| **关键文件** | `package.json`(19 命令/8 配置项)、`tsconfig.json`、`tsconfig.test.json`、`.vscodeignore`、`scripts/verify-vsix.js`、`test/mocks/vscode.js` |

### 2.4 manager(OpenShell 沙箱管理,FastAPI)

| 维度 | 内容 |
|------|------|
| **开发** | `./run.sh` = `python3 -m openshell_manager`(默认 127.0.0.1:18800,环回免 token);无 requirements.txt,依赖清单体现在 `deploy/Dockerfile.manager` pip 行(fastapi/uvicorn/grpcio/httpx 等);`config.json` 与引擎共享(gitignored,见 §4) |
| **测试** | `python3 -m pytest tests/ -q`——26 条离线契约测试(假 SDK 经 client_factory 缝注入 + 真 uvicorn HTTP 层,无需网关) |
| **生产** | `deploy/deploy.sh`(统一契约 deploy/check/status/start/stop/restart/logs)同步到 LXC 107 `/root/os-deploy/deploy/openshell-manager` 并 compose 构建;镜像两条路径:根 `Dockerfile`(离线 wheels 叠加 = 现役 2.0.0 实际产出)/`deploy/Dockerfile.manager`(自包含配方 python:3.12-slim);非环回绑定强制 token;**不挂 docker.sock**(纯管道,不持业务状态) |
| **关键文件** | `config.json`(SSOT)、`.token`、`run.sh`、`Dockerfile`、`deploy/{Dockerfile.manager,docker-compose.yml,deploy.sh,env,env.template,README.md}`、`tests/test_contract.py`、`libs/OpenShell/python`(vendored SDK,镜像仅取此子树) |

### 2.5 openshell-gateway(网关部署事实源,纯配置仓)

| 维度 | 内容 |
|------|------|
| **形态** | 无源码无测试:网关本体用上游预构建镜像 `ghcr.io/nvidia/openshell/gateway:latest`;正确性靠 `deploy.sh --check`(md5 漂移)+ `gateway_lifecycle.sh verify`;两个 Dockerfile 属上游源码重建路径,非日常 |
| **关键配置** | `gateway.toml`(bind 8080/8081、DooD compute_drivers、server_sans 路由域、auth 信任 LXC 内网)+ `docker-compose.yml`(`command: []` 清 CLI flags 让 TOML 接管、挂 docker.sock、`/var/lib/openshell` 同路径 bind) |
| **部署** | `./deploy.sh`(差量下发 + ensure)/ `./gateway_lifecycle.sh {ensure|verify|status|restart|logs}`;纪律:别随手 `recreate` |
| **实测口径** | 8080 gRPC 可达;**8081 health 发布但不可达**(仅绑 loopback),存活探测用 TCP 8080 或 manager `GET /api/v1/gateway/health` |

### 2.6 dsh-runtime(DSH 运行时源码,上游 fork)

| 维度 | 内容 |
|------|------|
| **开发** | pnpm monorepo(53 包组,Node ≥22);`pnpm install` 后 `pnpm dsh` 可源码直跑;相对上游 github deepseek-harness **仅 1 个本地补丁** `feb3854610`(SSE 断流归因) |
| **测试** | 分层 849 spec:`pnpm test` / `test:e2e`(无 DEEPSEEK_API_KEY 自动跳)/ `test:snapshot`(录制回放)/ `test:coverage`(逐文件 100% 门)/ typecheck/lint/duplication 静态门 |
| **生效纪律** | **未提交改动永远进不了镜像**——dsh-pentest-sse 的 deploy.sh 用 `git archive HEAD` 取源;改动必须先 commit 再重建镜像;commit hash 经 `DSH_CLIENT_COMMIT_HASH` build-arg 注入 |
| **关键文件** | `package.json`、`pnpm-workspace.yaml`、`AGENTS.md`(=CLAUDE.md 符号链接)、`tsconfig.{base,host,client}.json`、`docs/{architecture,testing,development}.md` |

### 2.7 dsh-pentest-sse(沙箱镜像配方仓)

| 维度 | 内容 |
|------|------|
| **构建即全部** | 两阶段 `Dockerfile`(digest-pin base;stage1 `sha256sum -c` 强校验 agent-tools → jq/sqlmap/testssl/playwright;主镜像 Node 22 + Chromium + CJK 字体 + dsh-runtime 源码 `pnpm install --frozen-lockfile && pnpm run build` + pdtools/nuclei-templates + codeaudit-submit 插件 + settings.yaml(ADR-193 窗口 150000) + ENTRYPOINT `bridge.mjs`);**仓内无自动化测试,靠构建期断言 + deploy.sh smoke** |
| **可复现** | `sandbox-artifacts/fetch.sh`(权威=引擎侧 four-direction-pentest-engine/deploy/tool-sbom.json)/ `fetch-agent-tools.sh`(权威=本目录 `agent-tools-sbom.json`);`--verify` 离线复核;vendored 大件 gitignore,**新检出首次构建前必须先跑 fetch** |
| **部署** | `./deploy.sh` = staging 组装 → `pct exec 107 docker build -t dsh-pentest-sse:latest` → 经 manager API 冒烟(`SKIP_SMOKE=1` 跳过);镜像无常驻进程,网关按需拉起 |
| **改 tag 同步三处** | `deploy.sh:16`(IMAGE 默认)、`sandbox-artifacts/agent-tools-sbom.json` 的 image 字段、伞仓 `deploy/sandbox-deploy.toml` |
| **关键文件** | `Dockerfile`、`deploy.sh`、`bridge.mjs`(JSON-RPC⇄SSE 桥,320 行,零依赖)、`dsh-settings/settings.yaml`、`plugins/codeaudit-submit/`、`sandbox-artifacts/{fetch.sh,fetch-agent-tools.sh,agent-tools-sbom.json,agent-tools.sha256}` |

---

## 3. 生产部署编排(伞仓统一入口)

**两套入口,同一拓扑**(清单 `deploy/sandbox-deploy.toml` 5 项目,拓扑序=依赖序):

| 入口 | 形态 | 目标 | 适用 |
|------|------|------|------|
| `bash deploy/sandbox-deploy.sh all` | 开发测试态:tar/pct 下发,远端 compose 构建 | LXC 107(gateway.internal) | 本工作区日常部署/回归 |
| `bash deploy/production-deploy.sh` | **生产态一键**:clone 后在本机 docker daemon 直接构建 | 任意 docker 服务器 | 第三方用户自部署 |

- 只读:`bash deploy/sandbox-deploy.sh plan`(清单)/ `check`(全项目漂移;`check <name>` 单查)/ `pull`(全项目上游镜像预拉,多源兜底)。
- 模拟栈供给:`bash deploy/sim-sync.sh push|rebuild|test [VMID]`——把本地 git 已提交树(engine+deploy)同步到 107 侧检出台(/root/codeaudit-sim-check, git archive 只取已提交内容, overlay 解包保远端 env.sim)并调远端 sim.sh up/tests;rebuild 构建久, 勿套短超时(2026-09-06 人类指令"用脚本走安装流程"固化)。
- 部署:`all` 或 `<name> [action]`;**deploy 前自动跑 pull-images.sh**(daemon registry-mirrors 不可信:死源+白名单拒 bitnami/*,实测 daocloud 拒/1panel 通)。
- codeaudit 单独入口 `deploy/prod/deploy.sh {deploy|check|status|logs|start|stop|restart}`。
- codeaudit deploy 流程:tar 同步 engine 源码树 + prod overlay → 远端 compose 双文件构建 7 Go 服务 + 4 中间件 → `.env` 首次随机 JWT、`OPENSHELL_MANAGER_TOKEN` 取 `manager/deploy/env` → 只建 3 空库不建表(服务自迁移)。
- web deploy 流程(2026-09-05 接管 CD 退役后的 console 发布职责):收敛式同步 web 源码树 → `web/docker-compose.yml` 单文件构建(`codeaudit-console:latest`,npm 构建在镜像内) → 8088→80,`CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:8090` 反代宿主发布网关 → 健康等待 + `/v1` 反代 401 透传断言。
- **生产态一键**(`deploy/production-deploy.sh` + `deploy/production.env`,后者 gitignored,首跑按 template 自动生成:密钥随机+宿主 IP 探测):预检 → opengrep 自动拉取(官方 release manylinux_x86,sha256 复核,见 PROVENANCE.md) → 镜像预拉 → gateway(ensure 自足:JWT 密钥 generate-certs+supervisor 镜像 `:latest` retag `:local`) → manager(deploy/.manager-stage 装配) → engine → 沙箱镜像(DOCKER_CMD=docker) → web;`status/stop/down` 全套。密钥接线单文件:`deploy/production.env`(JWT/manager token/Kafka advertised/端口/网段全部 env 化)。
- 容器化接线要点(2026-09-05 GUI 实测沉淀,均在 engine base compose):①服务间地址 env 全覆盖(task→project/storage,gateway→sast/dsh,dsh-runtime→result/task,缺省回落 yaml 的 localhost 即拨自身);②任务源共享卷 `agent_repos` 四方同卷(gateway/task/dsh-runtime 挂 /data/repos,sast-adapter 挂 /app/data/repos——运行时 CWD 各异);③dsh-runtime 沙箱路由 `CODEAUDIT_GATEWAY_DIAL_ADDR`(prod/sim overlay 缺省 host.docker.internal:8080);④prod overlay 补 `CODEAUDIT_STORE=s3`(storage 生产档,缺省 memory=通知空+文件不落 MinIO)。
- e2e 套件 `bash deploy/tests/run.sh`(前置模拟栈已 up;`run.sh 04` 跑单用例):
  01 健康 → 02 认证/refresh → 03 项目 CRUD → 04 上传→SAST 全链(任务自带上传件) → 05 console SPA/反代 → 06 通知(Kafka→Redis) → 07 AI 链路(manager/LLM 不可达时断言**诚实失败**) → **08 项目级上传→自动任务全链(GUI 用户路径,2026-09-05 增,回归服务间地址+共享卷接线)** → **09 可观测面(快照/AI 日志/通知,回归 AppendTaskLog+存储档位)**。
- 部署接线静态审计 `python3 deploy/check-wiring.py`(服务间地址 env 全覆盖/agent_repos 共享卷/生产档位/代码侧 env 出口/YAML 重复键;LESSONS #10 防线,sandbox-deploy check 与 production-deploy 预检已自动带)。
- GUI 人工全流程指引:[docs/manual-test-guide.md](manual-test-guide.md)。

---

## 4. gitignored 单一事实源清单(SSOT 不跟仓走,迁移/新检出必查)

**"文件存在"不等于"链路能跑"**——以下文件 gitignore,新机器/新检出必须交接或重建:

| 文件 | 用途 | 重建/校验方式 | 本机状态(2026-09-05) |
|------|------|---------------|----------------------|
| `manager/deploy/env`(600) | manager token 单一事实源,生产 .env 与 dsh-runtime 凭它生成 | `pct exec 107 -- cat /root/os-deploy/deploy/openshell-manager/.env > manager/deploy/env && chmod 600 manager/deploy/env` | ✅ 在位 |
| `manager/config.json` | 引擎与 manager 共享全局 SSOT(url/bind/port/tokenFile/gatewayEndpoint) | 从现役实例抄回或按 `config.py` 默认值手写 | ✅ 在位 |
| `manager/.token`(600) | Bearer token 文件 | 同上,与 config.json tokenFile 一致 | ✅ 在位 |
| ~~`manager/libs/OpenShell/python`~~ → **已纳管**(2026-09-05) | vendored SDK,镜像 COPY 输入 | 原为嵌套上游仓整树 gitignored——fresh clone 无此目录 manager 构建必败;现转为真实追踪文件(920K,23 源文件),7.2G 上游 clone 仍是开发机本地物(manager 1e02c20) | ✅ 入库 |
| `engine/services/sast-adapter-service/tools/opengrep` | sast-adapter 镜像烘焙的 SAST 二进制(缺它镜像必败,ADR-198) | 版本/sha256 见同目录 `PROVENANCE.md`(入库);宿主机下载 vendor 进树后 **`cd engine && sha256sum -c services/sast-adapter-service/tools/opengrep.sha256`**(清单是仓库根相对路径) | ✅ 在位,sha256 校验通过 |
| `deploy/env.sim` | 模拟栈真实 env | 按 `deploy/env.sim.example` 填 | —(可选,存在则自动加载) |
| `dsh-pentest-sse/sandbox-artifacts/` vendored 大件 | 沙箱镜像构建输入(pdtools/nuclei-templates/agent-tools) | `fetch.sh` + `fetch-agent-tools.sh`(按 sbom 拉取,`--verify` 复核);新检出首次构建前必跑。**tool-sbom.json 副本已入库为缺省事实源**(2026-09-05,原权威在伞仓外兄弟检出,FDPE_SBOM 可覆盖);GitHub 直连为缺省,PW_TOOLS_PROXY 显式才走代理 | —(fetch 产物) |
| `engine/.toolchain/` | 本地 Go 工具链(gitignored,ADR-122) | 见 engine README/ADR;source `env.sh` | ✅ 在位 |
| 各前端仓 node_modules | 依赖 | `npm install` / `pnpm install` | — |

迁移/重构后四步自检(详版见 deploy/README.md):① grep 旧标识(`../platform`、`../console`、`CD/` 等)→ ② `sandbox-deploy.sh plan` + `check` 只读全链 → ③ 密钥交接/重建(上表) → ④ compose 接管冲突处置(曾被手工绕过的容器先 `docker rm -f` 再 up)。
2026-09-05 执行记录:①发现并修复 `deploy/sim.sh` 两处旧表述(注释"默认 ../platform"、报错"platform/console 子模块");②check 结果 gateway/manager in sync、codeaudit 漂移(本地源码树演进,下次 deploy 收敛)、沙箱镜像 1.2.3 就位;③上表全部在位;④无冲突。

---

## 5. 端口口径总表(谁占哪个口,冲突前先查这里)

| 端口 | 归属 | 说明 |
|------|------|------|
| **8080** | openshell-gateway(gRPC 控制面) | **LXC 107 上被它占用**——CodeAudit 生产网关因此改发 **8090**(`CODEAUDIT_HOST_GATEWAY`,见 deploy/prod/env.template) |
| 8081 | openshell-gateway health | 发布但不可达(仅绑 loopback),探测用 TCP 8080 |
| 8080(容器内) | engine gateway / 沙箱 bridge | 容器内监听,不与上面冲突;发布时经映射 |
| **18080 / 18088** | 模拟栈 gateway / console | 1xxxx 段与生产隔离(codeaudit-sim) |
| **8088** | 生产 console(web) | 107 宿主发布,nginx 内 80;80/443 被宿主 wechat-nginx 占用故取 8088 |
| 50051/50052/50054/50055/50057/50058 | sast-adapter / project / task / storage / dsh-runtime / result(gRPC) | 服务名互访,不发布宿主 |
| 5432/6379/9000/9092 | PG / Redis / MinIO / Kafka | 中间件,栈内 |
| 18800 | manager | LXC 107 发布,引擎 dsh-runtime 南向唯一通道 |
| 5173 | web dev server(Vite) | 本地开发 |
| 网段 | 模拟 10.10.210.0/24(codeaudit-sim_default);生产 10.10.110.0/24(`codeaudit-engine-net`);manager 10.10.109.0/24(`codeaudit-sandbox-gateway-manager-net`);gateway `codeaudit-sandbox-gateway-net`;console `codeaudit-web-net` | 刻意避开 daemon 默认池 192.168.0.0/16(物理网段);伞仓部署链网络统一 codeaudit- 前缀 |

---

## 6. 刻意保护项(不是坑,别"修"它们)

- **伞仓 `make pull` 用 `--ff-only`**:子仓有未推提交时拒绝快进——先推子仓再同步。
- **dsh-pentest-sse deploy.sh 取 `git archive HEAD`**:dsh-runtime 未提交改动进不了镜像——改完必须先 commit(见 §2.6)。
- **openshell-gateway compose 的 `command: []`**:清掉镜像默认 CMD 让 gateway.toml 接管;随手 recreate 会破坏该口径。
- **engine `make pull`/伞仓同步的 GIT_TLS(`http.sslVerify=false`)**:内部 GitLab 自签证书,仅限 gitlab.local。
- **vscode-plugin `src/*.ts` 注释中的 `web/console/...` 路径**:历史血缘标注(前端已迁至 `web/src/...`),非运行引用,保留作演进线索。

# 发布清源与一键部署配置化地图(release-sanitize-map)

> 2026-09-07 立档:AI 会话对全部 7 子仓 + 伞仓 deploy 链的**硬编码与环境变量全量盘点**。
> 两个用途:① 未来对外发布时"清空内部信息"的施工清单;② 一键部署脚本
> `deploy/production-deploy.sh`(+`production.env.template`)**增设配置旋钮的依据**(§5)。
> 所有 file:line 均为当日实测(通读+grep+抽验);子仓演进后随改(同 U7 纪律)。
> 事实地图见 [dev-prod-map.md](dev-prod-map.md),本文是它的发布面镜像。

---

## 1. TL;DR——四类泄漏面

| 类 | 内容 | 处置原则 |
|----|------|----------|
| **A. git 元数据** | 全部 8 仓 remote=`https://gitlab.local/codeaudit/*`(伞仓 `.gitmodules` 同);提交作者 `RoyTse <roytse@auditmind.local>` / `<user@example.com>` 遍布历史;engine/.agent/ 账本含 LXC 107/SSH 探测路径/内网拓扑全量叙述(2026-09-08 已按人类指令退订 git+ignore,增量断供;**历史提交仍含全量叙述**) | **文件层面怎么清都没用**——必须 fresh orphan 分支或 `git filter-repo` + 重写 `.gitmodules` 指向公开托管 + 中性化作者身份 |
| **B. 运行时配置内网地址** | engine yaml/compose 的 `gateway.internal`×3;manager 代码级缺省 `gateway.internal:8080`;openshell-gateway `server_sans *.openshell.internal` + 5 条 xwpt extra_hosts;dsh-pentest-sse deploy.sh `MANAGER_BASE` 缺省内网 IP;伞仓 sim overlay/env.sim.example 同 | 全部 env 化或改中性缺省(§3 逐仓、§5 旋钮) |
| **C. 国内镜像源假设** | `goproxy.cn`(engine 9 文件)、`pypi.tuna.tsinghua.edu.cn`(engine 2 + sse 1)、`registry.npmmirror.com`(sse Dockerfile ENV + fetch-agent-tools.sh) | ARG/ENV 化 + 公共源缺省,镜像源退化为可选加速项 |
| **D. 弱密钥/缺省凭据** | `CODEAUDIT_JWT_SECRET:-changeme`(engine compose×2);project-service **代码级** JWT fallback `codeaudit-dev-secret-change-in-production`(user.go:47-53);postgres/postgres、minioadmin/minioadmin、admin/admin 种子;manager 现役 token 值在本机 `.token`/`deploy/env`(gitignored,但 tar 型发布会带出) | 堵缺省改必填;manager token 出网即视为泄漏须轮换 |

**发布前全区 grep 收口字典**(任一命中即未清完):
`gitlab.local / gateway.internal / proxy.internal / pve.internal / 10.10.110 / 10.10.109 / 10.10.210 / 10.10.210.1 / internal / RoyTse / auditmind.local / 100000000 / pct exec / VMID.*107 / /root/os-deploy / openshell-manager:1.0.0 / 127.0.0.1:5000 / codeaudit-test:latest / goproxy.cn / npmmirror / tuna.tsinghua / changeme / ci-test-secret`(注意排除测试夹具中的 RFC1918 样例与上游 dsh-runtime 的公共 API 域名)。

---

## 2. 逐仓硬编码清单(发布必清项;行号=2026-09-07 实测)

### 2.1 engine(最大泄漏源)

| 位置 | 值 | 生效性 | 处置 |
|------|----|--------|------|
| `configs/codeaudit.yaml:104` | `manager_url: http://gateway.internal:18800` | **运行时缺省**(env 未设时生效) | 缺省改 `http://127.0.0.1:18800`(与代码缺省一致),env `OPENSHELL_MANAGER_URL` 已有 |
| `configs/codeaudit.yaml:111` | `gateway_dial_addr: "gateway.internal:8080"` | **运行时缺省** | 缺省改空串(代码已支持"空=由 manager host 推导")或 `host.docker.internal:8080` |
| `docker-compose.yml:76` | `KAFKA_CFG_ADVERTISED_LISTENERS` 缺省 `gateway.internal` | **运行时缺省** | 缺省改 `kafka`(production.env 已设 `kafka`) |
| `docker-compose.yml:105,145` | `CODEAUDIT_JWT_SECRET:-changeme` | **运行时缺省** | 去 `:-changeme` 改 `:?`(网关空值 fail-fast 已具备) |
| `docker-compose.yml:14-15,52-53,283` | postgres/postgres、minioadmin/minioadmin(×2 组)、DSN 内嵌凭据 | **运行时** | `${...:?}` 化(§5 旋钮 3/4) |
| `docker-compose.yml:177-179` | `CODEAUDIT_MINIO_ENDPOINT/ACCESS_KEY/SECRET_KEY` | **死键**(代码读 `CODEAUDIT_S3_*`,base 里这组从不生效) | 删除(生产档正确命名在 prod overlay 已有) |
| `docker-compose.yml:146-150` | `CODEAUDIT_DB_*` | **死键**(project-service 是内存存储,无代码读) | 删除,防误导 |
| `services/project-service/internal/service/user.go:47-53` | 代码级 JWT fallback `codeaudit-dev-secret-change-in-production` | **运行时**(env 未设时以此签名) | 删 fallback 改 fail-fast(与网关对齐) |
| `services/project-service/internal/repo/memory.go:80-96` | 种子 `admin/admin` ROLE_ADMIN | **运行时演示凭据** | 保留则文档醒目+首登强改密,或 `CODEAUDIT_SEED_DEMO_ADMIN` 门控 |
| `services/{7 服务}/Dockerfile`(各:7-15 行)+`Dockerfile.test`+`Makefile:18,28`+`.agent/verify.sh:123` 等 | `ENV GOPROXY=https://goproxy.cn,direct`;pip `pypi.tuna...` | **构建时** | 改 `ARG GOPROXY=https://proxy.golang.org,direct` / `ARG PIP_INDEX_URL=https://pypi.org/simple` |
| `.gitlab-ci.yml`(整文件) | 内网 GitLab API+`oauth2:$TOKEN@gitlab.local/admins/codeaudit.git`、runner 122、内部镜像 `codeaudit-test:latest`、`GIT_SSL_NO_VERIFY`×13 | CI 运行时 | **整文件不随发布**或重写为公共 CI |
| `.agent/{status,decisions}.md` 及 archives/roadmap/research 等 | LXC 107/`pct exec`/双 IP/`/root/os-deploy`/SSH 探测路径 | 文档(~~git 跟踪~~→2026-09-08 已退订为本机文件,各仓 `.agent/*` ignore 仅豁免 `*.sh`) | 增量已断供;历史提交仍随 A 类 git 重写一并剔除 |
| `docs/api-external.md:28`、`docs/data-flows.md:106,214`、`12_部署与运维方案.md:14`、`06_OpenShell集成设计.md:678` | `gateway.internal`、"LXC 107" | 文档 | 占位符化 |
| `scripts/evaluate_f1.sh:58` | `/home/diversevul` | 测试脚本 | env 化 |

其余:容器内端口 8080/50051-50058(公共契约,保留);`RoyTse`、`10.10.*` 确认不在本仓任何跟踪文件中。

### 2.2 web

| 位置 | 值 | 生效性 | 处置 |
|------|----|--------|------|
| `deploy.sh:17-24` | `pct exec 107`、`/root/os-deploy/deploy/web`、`http://gateway.internal:8088`、upstream 8090 | **运行时(内网 CD 链)** | 该文件整体不随发布(对外发布走伞仓 production-deploy 的 [5/5]);或中性 env 化 |
| `docker-compose.yml:6`(注释)、`README.md:134,184` | `gateway.internal:8080`、"107 模拟栈网关 :18080" | 文档 | 占位符化 |
| `src/findings/chainParser.ts:28` + `src/__tests__/chainParser.test.ts:80` | 注释/夹具含 `gateway.internal` | 注释/测试 | 改 RFC5737 `192.0.2.x`(保住 4 段 IP 形状) |
| `.gitignore` | 不含 `.env` | 风险 | 发布前补 |

产品运行时本身干净:同源 `baseURL:'/'`(`src/api/client.ts:15`)、WS 用 `window.location.host`,bundle 不含绝对地址;nginx 模板全 envsubst 化。

### 2.3 vscode-plugin

| 位置 | 值 | 生效性 | 处置 |
|------|----|--------|------|
| `package.json:11` | `"repository.url": https://gitlab.local/admins/codeaudit-vscode-plugin.git` | **随 VSIX 发布** | 改公开仓库地址或删除;现存 `codeaudit-vscode-0.1.0.vsix` 已内嵌该 URL——**旧产物作废重建** |
| `package.json:6-8` | `publisher: codeaudit`、`license: UNLICENSED`、`private: true` | 随 VSIX | 上架口径需产品决策 |
| `test/fixflow.e2e.js:55,60` | 缺省网关 `http://pve.internal:8080`(第 4 个内网主机)、`admin/admin` | 测试 | 改 `localhost:8080` 缺省+argv/env 覆盖 |
| `scripts/verify-vsix.js:6` | 硬编码 `codeaudit-vscode-0.1.0.vsix` 文件名 | 打包门禁 | 从 package.json version 推导或 glob |

运行时配置仅 `codeaudit.serverUrl`(缺省 `http://localhost:8080`,package.json:28 + extension.ts:184)——对外文档须说明:一键部署主机上 8080 归 openshell-gateway,插件应填**引擎网关发布口(缺省 8090)**。零 env 消费。

### 2.4 manager

| 位置 | 值 | 生效性 | 处置 |
|------|----|--------|------|
| `openshell_manager/config.py:29` | `DEFAULT_GATEWAY_ENDPOINT = "gateway.internal:8080"` | **代码级运行时缺省** | 改 `host.docker.internal:8080` 类中性缺省(env `OPENSHELL_GATEWAY_ENDPOINT` 已有,§5 旋钮 1) |
| `deploy/docker-compose.yml:22,31` | extra_hosts `gateway.internal:host-gateway`;env 缺省同域名 | **运行时** | 同上参数化;extra_hosts 块整体可删 |
| `deploy/docker-compose.yml:49`(注释) | "LXC=gateway.internal / PVE=pve.internal"、`192.168.0.0/16` 池 | 文档 | 删 |
| `deploy/docker-compose.yml:45,50` | 网络名 `codeaudit-sandbox-gateway-manager-net`、钉网段 `10.10.109.0/24` | **运行时** | 网段 env 化或去钉(§5 旋钮 9) |
| `deploy/deploy.sh:16-20,86` | `pct exec 107`、`/root/os-deploy/...`、`http://gateway.internal:18800` | **运行时(内网链)** | 内网链不随发布;对外链是伞仓 production-deploy 的 [2/5] staging 装配 |
| `deploy/Dockerfile.manager:15` | `COPY config.json ./`(兜底档,而 config.json 是 gitignored 本地物且含生产 IP) | **构建时** | 改为跟踪的 `config.json.example`;production-deploy.sh:187-196 生成的最小档里 `gateway.internal:8080` 同步中性化 |
| 根 `Dockerfile:7-9` | 基镜像 `openshell-manager:1.0.0`(注释自述"已带 config.json 与 .token 部署文件"——**烘过 token 的离线路径**) | 构建 | 不随发布 |
| `deploy/env.template:2` | `OPENSHELL_GATEWAY_ENDPOINT=gateway.internal:8080` | 模板 | 占位符化 |
| `.token` / `deploy/env`(本机 600,gitignored) | **现役 token 明文**(同一值两处) | 本地 | 任何 tar 型发布前剔除;出网即轮换 |
| `manager.log`、`config.json`(本机) | 生产 IP/内部 provider 名 | 本地 | 同上 |
| README/docs 10+ 行 | `gateway.internal`、`gateway.internal`、GitLab 组路径、`/root/om-build` | 文档 | 清洗 |

env 机制健全:`OPENSHELL_MANAGER_{CONFIG,BIND,PORT,TOKEN,MAX_UPLOAD_BYTES}`、`OPENSHELL_GATEWAY_ENDPOINT`、`OPENSHELL_LIB_PATH`,优先级 env > config.json > 代码缺省。

### 2.5 openshell-gateway(纯配置仓)

| 位置 | 值 | 生效性 | 处置 |
|------|----|--------|------|
| `gateway.toml:29` | `server_sans = ["*.openshell.internal"]` | **运行时**(SAN 路由域;lifecycle `ensure` 会远端钉此值) | 参数化(lifecycle 已有 `ROUTING_DOMAIN` env,缺省也是内网域;§5 旋钮 2) |
| `docker-compose.yml:83-89` | 5 条 xwpt 实验域 extra_hosts + "pentest lab" 注释 | **运行时** | 整块删除或移可选 overlay;沙箱服务路由真实依赖=网关发布口+Host 头,不依赖这 5 条(它们是开发机兜底) |
| `docker-compose.yml:149` | 网络名 `codeaudit-sandbox-gateway-net` | **运行时** | 中性化 |
| `docker-compose.yml:79` | `user: "0"`;`:56`(toml)`allow_unauthenticated_users = true` | **安全姿态** | 保留但对外文档必须写明信任边界(loopback/发布范围) |
| `deploy.sh:28-30` / `gateway_lifecycle.sh:46-49` | `REMOTE="${REMOTE-pct exec 107 --}"`、`VMID=107`、`/root/os-deploy/deploy/docker`、`ROUTING_DOMAIN=openshell.internal` | **运行时** | 对外链=伞仓 production-deploy 传 `REMOTE="" DEPLOY_DIR=本地`;缺省需中性化+补 ssh 传输路径(pct-only 是一键缺口) |
| `gateway.toml:43` | `supervisor_image = "ghcr.io/nvidia/openshell/supervisor:local"` | **运行时**(`:latest`→`:local` retag 自举约定) | 保留机制,文档化 |
| `tests/run.sh` 多处(97-98,184-190,215-217,441-449…) | **字面量断言**钉死 xwpt 域/`pct exec 107`/网络名 | 测试门禁 | **改配置必须同 commit 改此测试**,否则 CI 红 |
| README/docs | `gateway.internal=gateway.internal`、LXC 107、内网部署链叙述 | 文档 | 清洗 |

无密钥入库(JWT 密钥运行时 generate-certs 生成)。env:`IMAGE_TAG`、`OPENSHELL_PORT`(与容器 8080 **同号契约**)、`OPENSHELL_HEALTH_PORT`、`OPENSHELL_GATEWAY_CONFIG`、`OPENSHELL_DB_URL`、`XDG_DATA_HOME`、`HOME`。

### 2.6 dsh-runtime(上游 fork,跟踪树基本干净)

- 跟踪文件中**无任何内网标识**(双 IP/网段/RoyTse/LCX/相关端口全为 0 命中);命中者均为上游公共内容(`api.deepseek.com` 缺省、RFC1918 测试夹具、`sk-*` 假 key)。
- **泄漏全在 git 元数据**:remote 内网 GitLab;本地 4 个补丁提交(不止文档记载的 1 个:`feb3854610` SSE 归因 + `3dd5d42bbd` 接口文档 + `a90f372a86` 回归门禁 + `77631bd397` CLI 测试)作者含 `RoyTse <roytse@auditmind.local>`;`.git/config` 还有 `http.sslverify=false`。
- 未跟踪的 `vendor/*/node_modules/.bin` shim 内嵌本机绝对路径——**发布只能走 `git archive HEAD`**(现行镜像链已是如此),严禁工作树 tar。
- env 面(沙箱镜像消费):`DEEPSEEK_API_KEY`/`DEEPSEEK_BASE_URL`(bootstrap-only)、`DSH_*` 家族、代理/CA 变量;镜像侧接线在 dsh-pentest-sse bridge。

### 2.7 dsh-pentest-sse

| 位置 | 值 | 生效性 | 处置 |
|------|----|--------|------|
| `deploy.sh:20` | `MANAGER_BASE` 缺省 `http://gateway.internal:18800` | **运行时缺省**(伞仓链已显式传 `127.0.0.1:18800`) | 缺省改必填或 localhost |
| `deploy.sh:18-19` | `VMID=107`、`DOCKER_CMD=pct exec $VMID -- docker` | **运行时缺省** | 伞仓链已传 `DOCKER_CMD=docker`;缺省中性化。**注意** `test/static.consistency.test.mjs:127` 字面量钉住此行——同 commit 改 |
| `deploy.sh:128-129` | 冒烟断言 `openshell.internal:8080` 路由域 | **运行时** | 参数化(§5 旋钮 2 的 EXPOSE_DOMAIN) |
| `Dockerfile:64,70-73` | pip 走 tuna;`COREPACK_NPM_REGISTRY`/`npm_config_registry=registry.npmmirror.com`(ENV 烘死) | **构建时** | ARG/ENV 化+公共源缺省;"LXC 107 直连不可达"注释同删 |
| `sandbox-artifacts/fetch-agent-tools.sh:19` | `NPM_REGISTRY="https://registry.npmmirror.com"`(shell 变量,**不可 env 覆盖**) | **构建时** | 改 `${NPM_REGISTRY:-https://registry.npmjs.org}` |
| `sandbox-artifacts/tool-sbom.json:8,140-144` | 内网 registry `127.0.0.1:5000`、`gateway.internal` inspect 叙述、`CD/` 路径、签名 keyId | 文档(**本仓最密泄漏**) | 重写 provenance 注记或只留工具清单 |
| `sandbox-artifacts/agent-tools-sbom.json:7,97` | 内网代理 `proxy.internal:2080`、`gateway.internal` DNS 观察 | 文档 | 清洗(`sandbox-artifacts/README.md:37` 还把代理误写成缺省——顺手修文档漂移) |
| README/docs/REGRESSIONS 若干 | `gateway.internal`、`openshell.internal`、LXC 107 | 文档 | 清洗 |

bridge.mjs/settings.yaml/codeaudit-submit 插件干净:无密钥、无端点(插件 ack-only 走 SSE);digest pin(ghcr base/MCR playwright)是公共 registry,保留但建议 ARG 化。env:`BRIDGE_PORT/DSH_*/BRIDGE_*_TIMEOUT_MS`、`DSH_CLIENT_COMMIT_HASH`(build-arg 必填)。

### 2.8 伞仓层(发布裁剪的对象本身)

| 位置 | 值 | 处置 |
|------|----|------|
| `.gitmodules`(7 条) | 全部 `https://gitlab.local/codeaudit/*.git` | **发布第一刀**:`--recurse-submodules` clone 会直拨内网;必须重写指向公开托管 |
| `deploy/production-deploy.sh` | ~~:193 生成的最小 config.json 含 `gateway.internal:8080`~~ → 已中性化(2026-09-07 交互化联动改造一并修复,见 §4 旋钮 1) | ✅ |
| `deploy/prod/env.template:7` | `OPENSHELL_MANAGER_URL=http://gateway.internal:18800` | 内网链文件,不随发布 |
| `deploy/prod/deploy.sh:21-25,70,87` | pct/107/`gateway.internal`/`/root/os-deploy` | 内网链,不随发布 |
| `deploy/sandbox-deploy.toml`(vmid=107×5、/root/os-deploy×4)、`deploy/sim*.sh`、`deploy/docker-compose.sim.yml:92`(缺省内网 manager)、`deploy/env.sim.example:48`、`deploy/pull-images.sh:21`(`REMOTE-pct exec 107`)、`deploy/tests/*`(git_fixture 10.10.210.1:19418、ui_check 缺省 `gateway.internal:18088`) | 内网 QA 链全套 | **不随发布**(见 §4 发布树裁剪) |
| `docs/`(dev-prod-map 等)、`LESSONS.md`、`.agent/`、`Makefile` sim 目标、`README.md` | 内网拓扑全量叙述 | 不随发布或重写 |
| `deploy/prod/docker-compose.deploy.yml:18-19` | S3 凭据 `minioadmin` 硬编码 | §5 旋钮 3 |

---

## 3. 环境变量机制现状(各仓"已 env 化到什么程度")

| 仓 | 机制 | 缺口 |
|----|------|------|
| engine | `CODEAUDIT_*`/`OPENSHELL_*` env > `configs/codeaudit.yaml` > 代码缺省;compose 全部 `${VAR:-default}` 展开;宿主口 12 个 `CODEAUDIT_HOST_*` | yaml 内两处内网 IP 充当缺省;compose 死键两组;Dockerfile 零 ARG(镜像源烘死) |
| manager | `OPENSHELL_*` env > config.json > 代码缺省(config.py) | 代码缺省内网域;compose 网段硬钉 |
| web | 运行时零 env(同源 `/v1`);部署态 nginx envsubst `CODEAUDIT_GATEWAY_UPSTREAM`(缺省分歧:compose 8080 vs 内网 deploy.sh 8090——对外只走 production.env 的 8090 口径)+`CODEAUDIT_CONSOLE_PORT`;开发态 `CODEAUDIT_GATEWAY_URL` | 无(缺省分歧收敛即可) |
| vscode-plugin | 零 env;VS Code 配置项 `codeaudit.serverUrl` | 无(属用户侧配置) |
| openshell-gateway | compose `IMAGE_TAG`/`OPENSHELL_PORT`(同号契约)/`OPENSHELL_HEALTH_PORT`;TOML 不可 env 化,靠 lifecycle `ensure` 远端改 `server_sans`(`ROUTING_DOMAIN` env 驱动) | toml 其余键无 env 通道(设计如此,发布只动 server_sans) |
| dsh-pentest-sse | `DOCKER_CMD/VMID/MANAGER_BASE/IMAGE/CONTEXT/MANAGER_ENV/SKIP_SMOKE` + bridge `BRIDGE_*/DSH_*` + build-arg `DSH_CLIENT_COMMIT_HASH` | 镜像源 ENV 烘死;冒烟域断言硬编码 |
| dsh-runtime | `DEEPSEEK_API_KEY/BASE_URL`、`DSH_*`(bootstrap-only)、代理/CA 族 | 无(上游机制) |
| **伞仓一键** | `deploy/production.env` 单文件(gitignored,template 首跑生成:密钥随机+宿主 IP 探测),已覆盖 16 键 | 见 §5 |

---

## 4. 一键部署配置机会清单(production.env 增设施工表)

`production-deploy.sh` 现读 16 键(密钥 2 + 地址 2 + 端口 9 + Kafka 1 + 网段 1 + 追加的 console upstream)。以下为**链路上仍硬编码、脚本给不出旋钮**的缺口,按优先级。
**2026-09-07 交互化落地**(deploy/production-deploy.sh 重构):①deploy/configure 终端运行时交互确认访问 IP/端口冲突(给空闲建议,避开本栈计划口)/网段重叠(给跳位建议),汇总确认才开工,`--yes`/非 TTY 跳过;②**联动键自动重算回写**——`OPENSHELL_GATEWAY_ENDPOINT`/`CODEAUDIT_GATEWAY_DIAL_ADDR` 随 `OPENSHELL_PORT`、`CODEAUDIT_GATEWAY_UPSTREAM` 随 `CODEAUDIT_HOST_GATEWAY`,旋钮 1 的伞仓侧全部闭环(含生成档 config.json 中性化,manager 子仓 `config.py:29` 代码缺省仍待清);③模板曝光 `DSH_IMAGE`。**沙箱素材免下载**(2026-09-08):`ensure_sandbox_artifacts` 先 `fetch*.sh --verify` 离线复核(sbom sha256 逐项),在位即零下载,缺失/漂移才全量拉取;`check` 只报在位性。状态标注:✅=已闭环,⚠️=部分,空=未动。
**xwpt 三仓清源落地**(2026-09-08,人类指令"xwpt 不具通用性"):配置面全零——gateway compose 5 条 lab 域 extra_hosts 删(server_sans→`*.sandbox.codeaudit.internal`)、manager compose extra_host+三处缺省删/中性化、sse 冒烟断言参数化;`openshell.internal` 剩余命中仅叙述性文档(gateway/manager/sse README+docs、sse tool-sbom 注记),随发布分支剔除。
**内外面分离定案**(2026-09-08,人类指令点破):manager 是内部面非交互面——`OPENSHELL_MANAGER_URL` 恒为内部常量 `host.docker.internal:18800`(hosts 别名,零 DNS 零用户输入),不再随访问 IP;用户确认的访问入口只存 `CODEAUDIT_ACCESS_IP` 供横幅/汇总**显示**,不参与任何接线。暴露给用户的只有 web(IP+口)与 engine 网关(IP+口);其余服务宿主口均为运维/内部消费面,生产档可考虑 loopback 化(见 §4 施工表外延)。Windows 部署经 `deploy/windows/bootstrap.ps1` 复用同一 bash 入口(双壳:Git Bash 优先,WSL2 兜底;bash 入口带 MSYS 回退 ss→netstat、ip/hostname→ipconfig、python3→python)。
**零 DNS 依赖不变量**(2026-09-08 代码实证,清源相关推论):同网服务名走 docker 内嵌 DNS、跨栈走 hosts 别名/裸 IP、**沙箱路由域(现 `*.openshell.internal`)仅作 Host 头路由键从不被解析**(dsh-runtime routeReq 拨 `CODEAUDIT_GATEWAY_DIAL_ADDR`、svcURL 仅进 req.Host;bridge.mjs 零出站;dsh-runtime 零 net.LookupHost)。推论:①路由域是纯字符串键,旋钮 2(`ROUTING_DOMAIN`)只影响证书 SAN 与 URL 观感,不影响可达性;②gateway/manager compose 里的 xwpt extra_hosts 条目本身就是"零 DNS 机制"的实现载体(hosts 别名),清洗时改为中性别名即可、不可简单删除;③本机 /etc/hosts 的 xwpt 条目**不进入容器**(内嵌 DNS 不转发宿主 hosts),历史 e2e 绿不依赖它。

| # | 建议旋钮(production.env 新键) | 现状硬编码点 | 期望行为 |
|---|-------------------------------|--------------|----------|
| 1 | `OPENSHELL_GATEWAY_ENDPOINT` | manager `config.py:29` + manager compose:31 + **production-deploy.sh:193 生成的 config.json** 三处缺省 `gateway.internal:8080`(现在能跑通纯靠 extra_hosts 把内网域钉到 host-gateway) | ✅ 全链闭环(2026-09-08 补完):联动键自动重算 + 生成档中性化 + manager 子仓 `config.py:29`/compose:31/env.template 缺省改 `host.docker.internal:8080` + 删 `gateway.internal:host-gateway` extra_host(manager a4c1679) |
| 2 | `ROUTING_DOMAIN`(+sse 侧 `EXPOSE_DOMAIN`) | ~~三处内网域~~ | ✅ 全链闭环(2026-09-08):三仓缺省中性化 `sandbox.codeaudit.internal`(gateway d96aeae/sse ff5efc4),交互确认+deploy_gateway 透传 ROUTING_DOMAIN+deploy_sandbox_image 透传 EXPOSE_DOMAIN 同源联动,gateway tests/run.sh 13 处字面量与 sse static.consistency 契约钉同 commit 改;gateway compose 5 条 xwpt extra_hosts 整块删除 |
| 3 | `CODEAUDIT_MINIO_ROOT_USER/PASSWORD`(→overlay S3 键同源) | prod overlay:18-19 `minioadmin`×2 + engine base compose:52-53 | overlay 改 `${CODEAUDIT_MINIO_ROOT_USER:-minioadmin}:${...}`;S3 ACCESS/SECRET 同值注入;顺手删 base 死键 `CODEAUDIT_MINIO_*` |
| 4 | `CODEAUDIT_PG_PASSWORD`(可选 USER/DB) | engine base compose:14-15 + :283 DSN 内嵌 | compose 改 `${CODEAUDIT_PG_PASSWORD:?}`;DSN 模板拼装;顺手删死键 `CODEAUDIT_DB_*` |
| 5 | (无新键,堵缺省) | engine compose:105,145 `:-changeme`;project-service user.go:47-53 代码 fallback | 去缺省改 `:?`;删代码 fallback——`production.env` 首跑随机生成已就位,只是别留旁门 |
| 6 | `GOPROXY` / `PIP_INDEX_URL` / `NPM_REGISTRY`(可选加速项) | engine 7 Dockerfile+Dockerfile.test ENV;sast Dockerfile:22;sse Dockerfile:64,72-73;fetch-agent-tools.sh:19 | Dockerfile 改 ARG+公共源缺省;compose `build.args` 接 env 透传;国内用户经 production.env 打开加速 |
| 7 | `DSH_IMAGE` | production-deploy.sh 已识别(`${DSH_IMAGE:-...}`)但模板无键,用户不可见 | ✅ 模板已曝光(注释行,取消注释即生效) |
| 8 | `OPENSHELL_MANAGER_PORT` | manager compose:26 `18800:18800` 硬钉;production-deploy check_ports:83 字面 18800 | ⚠️ 冲突可检测+交互拦截(建议腾口/中止),改口仍需 manager 子仓 compose 参数化 |
| 9 | `MANAGER_SUBNET` | manager compose:50 钉 `10.10.109.0/24`(production.env 注释自认钉死) | `${MANAGER_SUBNET:-10.10.109.0/24}` 或去钉 |
| 10 | `CODEAUDIT_MIMO_API_KEY/ENDPOINT` | deploy/prod/env.template 有、production.env.template 无、**engine 代码未发现消费点(疑似遗留)** | 发布前核实:接通进模板或从内网链删除 |
| 11 | (文档项)插件 serverUrl | vscode-plugin 缺省 `localhost:8080`,与一键主机上 openshell-gateway 的 8080 冲突 | 对外部署文档写明应填 `http://<宿主>:8090` |
| 12 | (文档项)admin/admin | engine 种子账号 | 部署完成横幅已提示改密;建议首登强改密或门控种子 |

> 落实施注意:①③④⑤属 engine 仓改动(B 模式,须 claim+verify.sh);①manager 侧、②gateway/sse 侧同理;6 跨 engine+sse。全部完成后伞仓 e2e(`deploy/tests/run.sh`)+`production-deploy.sh check` 收口,U7 同步 dev-prod-map。

---

## 5. 发布施工顺序建议

1. **git 层(先做,否则白清)**:各仓 fresh orphan 分支(或 filter-repo);作者中性化;`.gitmodules` 重写公开托管;dsh-runtime 可走"上游公开仓 + 4 本地补丁重放"天然掉历史;engine `.agent/`、伞仓 `docs//.agent/LESSONS` 不进发布分支。
2. **发布树裁剪(伞仓对外最小面)**:`deploy/{production-deploy.sh, production.env.template, pull-images.sh, check-yaml-dups.py, check-wiring.py, prod/docker-compose.deploy.yml, prod/README(重写)}` + 7 子仓 + 对外 README(**候选定稿已落:`docs/release-readme.md`,2026-09-08,清源字典自检零命中;发布时更名为根 README.md 并删除其尾部维护注记**);**剔除** sandbox-deploy.*、sim*、prod/deploy.sh、prod/env.template、tests/、docker-compose.sim.yml、env.sim.example、docs/(内部)、Makefile(重写)、.agent/。
3. **engine**:§2.1 表逐行(两 IP 缺省→中性、堵 changeme、删死键、删代码 JWT fallback、Dockerfile ARG 化、.gitlab-ci.yml 整体剔除)——改后跑 `make verify` 11 checks。
4. **manager/openshell-gateway/dsh-pentest-sse**:旋钮 1/2/9 落地;**字面量测试门禁同 commit 改**(gateway tests/run.sh 7 处、sse static.consistency:127);各跑仓内门禁。
5. **web/vscode-plugin**:§2.2/2.3 表;VSIX 作废重建(内嵌内网 URL)。
6. **密钥轮换与出网检查**:manager token(值已出本机即轮换);发布包全树跑 §1 grep 字典零命中;`git archive` 型分发(dsh-runtime 强制)。
7. **验收**:干净机器(或 dind)跑 `production-deploy.sh deploy` 全链 + `deploy/tests/run.sh` 移植版 e2e。

---

## 6. 维护

- 本文是发布清源 SSOT;§1 grep 字典=发布收口判据。
- 落实施时改了端口/入口/SSOT 文件 → 同步 dev-prod-map.md §5 与本表(U7)。
- 每完成一个仓的清源,在 `.agent/status.md` 记一行(含"发布清源"关键词,便于对账)。

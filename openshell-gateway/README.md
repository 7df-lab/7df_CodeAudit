# openshell-gateway

OpenShell Gateway（NVIDIA 开源沙箱网关）在 codeaudit 体系里的**部署事实源**
（source of truth）。本仓**不含网关源码**——网关本体用上游预构建镜像
`ghcr.io/nvidia/openshell/gateway`；本仓只承载部署配方（compose / TOML /
生命周期脚本）与运行纪律。**改部署一律改这里，用 `deploy.sh` 下发**，不要
直接改 部署主机 里的运行副本。

网关职责：沙箱容器编排（DooD，在宿主 docker 里拉起兄弟容器）、推理路由与
provider 存储、沙箱端口暴露（服务路由域）。

调用链：`openshell CLI / manager(:18800) → 网关 gRPC :8080 → DooD 拉起沙箱容器 → 沙箱回调 host.openshell.internal:8080`

## 仓库位置与现役部署

本目录是主仓库的 `openshell-gateway/` 模块，自上游
`CD/openshell-gateway/` 原样析出（fresh init，首提交 = 当前文件集）。

现役部署：

- 部署主机（宿主机 `docker`，192.0.2.122）
- 运行 compose 目录 **`/opt/codeaudit/openshell-gateway/`**（生产部署树，

  label 实证。本仓脚本缺省 `DEPLOY_DIR` 同日修订为该目录（原 os-deploy
  死路径退役——`<legacy-path>` 在 <host-id> 已整个不存在），打现网**零 env 直达**）
- 容器 `openshell-gateway-gateway-1`，镜像 `ghcr.io/nvidia/openshell/gateway`
  

## 文件

| 文件 | 作用 |
|---|---|
| `docker-compose.yml` | 网关容器编排：DooD（挂 docker.sock，user 0）、发布 8080/8081、`command: []` 让 TOML 接管配置、同路径 bind `/var/lib/openshell`；镜像 digest 钉扎（`${GATEWAY_IMAGE:-…@sha256:…}`） |
| `gateway.toml` | 网关全部配置：bind、compute_drivers（docker）、`server_sans`（服务路由域）、JWT、auth；容器内挂载于 `/etc/openshell/gateway.toml`（只读） |
| `deploy.sh` | 本仓 → 部署主机 同步（md5 比对、仅推变化文件、远端留 `.bak.<时间戳>`）+ `gateway_lifecycle.sh ensure` 生效 |
| `gateway_lifecycle.sh` | 生命周期：ensure / verify / status / start / stop / restart / recreate / image（钉扎对账）/ logs |
| `gateway_db_backup.sh` | gateway.db+tls 最小备份：backup / verify / restore / list（见"gateway.db 备份"）；进 deploy.sh FILES 下发（cron 在 部署主机 侧执行它） |
| `docs/` | 接口事实文档：`external-interfaces.md`（外部接口输入/输出）、`internal-interfaces.md`（工件间内部接口）、`data-flows.md`（跨仓链路/宿主资产/网络拓扑）、`prod-ops-*.md`（<host-id> 生产操作单） |
| `tests/` | 离线测试套（见下"测试与防回归"）：`run.sh`、`stubs/`（docker/远程执行 桩）、`githooks/pre-commit`、`REGRESSIONS.md`（回归档案与机制） |

## 端口与可达性

| 端口 | 发布 | 实际可达性 |
|---|---|---|
| 8080 gRPC | `0.0.0.0:8080→8080` | **可达**（manager / openshell CLI 全走这里）。TOML 绑容器内 loopback，但 docker driver 自动在桥接接口加监听，发布端口转发可达 |
| 8081 health | `0.0.0.0:8081→8081` | **发布了但不可达**（宿主侧 curl reset）：`/healthz` `/readyz` 在容器内仅绑 loopback，且健康端点**没有**桥接监听。存活探测改用：TCP 8080（`gateway_lifecycle.sh` 的 liveness）或 manager `GET /api/v1/gateway/health`（gRPC 级，部署链实际用的就是这个） |

部署主机上 8080/8081 归本网关占用（CodeAudit API 网关因此改发布 8090——
生产 overlay 为 engine 仓 `deploy/docker-compose.deploy.yml`，端口经 env
`CODEAUDIT_HOST_GATEWAY=8090` 注入）。

## 配置要点（gateway.toml + compose）

- **镜像 digest 钉扎（R9）**：compose `image:
  ${GATEWAY_IMAGE:-ghcr.io/nvidia/openshell/gateway@sha256:05cf77bb…}` 与
  lifecycle `GATEWAY_IMAGE` 缺省两处同源（守卫）；主仓库
  `deploy/internal/sandbox-deploy.toml` images 与 `production-deploy.sh`
  预拉清单同批同 digest（:latest 重拉换版会击穿行为契约，历史上已两次诱发
  事故）。**升级流程**：确定换版 → 取新 digest
  （`docker image inspect <现网镜像ID> --format '{{json .RepoDigests}}'`）→
  四处同改 → 本仓测试 → deploy.sh 下发 + `gateway_lifecycle.sh recreate` 生效
  （`compose restart` 保留容器规格，不换镜像）。漂移对账：`gateway_lifecycle.sh
  image`（只读，比镜像 ID 非 ref 字符串，漂移 exit 1）。
- **TOML 是唯一配置源**：compose `command: []` 清掉镜像默认 CMD
  （`--bind-address 0.0.0.0 --port 8080`）——CLI flags 优先级高于 TOML，
  不清掉则 `bind_address = "127.0.0.1:8080"` 被静默忽略。
- **docker driver**：`default_image`（`sandbox create` 不带 `--from` 时拉
  base:latest）、`supervisor_image`（首启抽取 `openshell-sandbox` 二进制，
  缓存到 `XDG_DATA_HOME` 复用）、`sandbox_namespace = "openshell"`（沙箱
  容器名前缀）、`grpc_endpoint = http://host.openshell.internal:8080`
  （沙箱回调端点——宿主端口必须同号发布 8080 才能路由到网关容器）。
- **鉴权取舍**：`allow_unauthenticated_users = true`——gRPC 控制面无鉴权，
  信任边界 = 部署主机 内部（端口只发布在 部署主机，不出公网）。`gateway_jwt`
  （密钥 `/var/lib/openshell/tls/jwt/`，ttl 3600s）供网关向沙箱签发回调
  令牌，不是控制面鉴权。
- **`OPENSHELL_DB_URL` 只能走 env**（TOML 显式禁止，防密钥 URL 入库）：
  现值 sqlite `/var/lib/openshell/gateway.db`。
- **`/var/lib/openshell` 同路径 bind**（宿主 == 容器绝对路径一致）：沙箱
  创建时宿主 docker daemon 要按容器内写下的路径解析 bind source，命名卷
  无法满足；拓扑约定每宿主单网关。
- **extra_hosts**：仅两条中性别名——`host.docker.internal` /
  `host.openshell.internal` → host-gateway（compose 现状；历史上的 pentest


## 部署与生命周期

```bash
./deploy.sh --check           # 本仓与 部署主机 运行副本的漂移（不改任何东西）
./deploy.sh                   # 差量下发并生效（配置无变化则不重启）
./gateway_lifecycle.sh status # 容器状态 / 路由域 / 存活
./gateway_lifecycle.sh logs 100
```

- **deploy.sh**：对 3 个文件（compose/toml/备份脚本——`FILES` 清单，deploy.sh:36）做 md5 比对，
  仅推变化的（远端先留 `.bak.<时间戳>` 备份），末尾 `ensure` 生效。lifecycle 与
  deploy 脚本本体不在 FILES——远程执行 模式下它们在管理机本地执行，远端只需 compose


  deploy.sh/E1/I6 三处不一致）
- **ensure**：幂等钉 `server_sans`（变了才 `compose restart` 重读 TOML）→
  TCP 8080 liveness → verify。**全新宿主自足**：①JWT 签名密钥
  缺失时一次性 `generate-certs` 预置（网关不自动生成，缺了启动即崩）；②
  supervisor 镜像缺失时拉取（TOML 钉的 `:local` 类本地 tag 上游 404 → 拉同仓
  `:latest` retag）。
- **REMOTE=""（空串）= 本机执行契约**（`-` 非 `:-`，空串不得触发 远程执行 缺省）；
  主仓库 `deploy/internal/production-deploy.sh` 依赖此语义在本机 daemon 直接部署。
- **别随手 `recreate`**（`compose up -d`）：会应用 compose 的
  `command: []`，TOML 的 `bind_address=127.0.0.1:8080` 接管，发布端口
  可达性必须重验。日常走 `deploy.sh` / `ensure` / `restart`
  （`compose restart` 保留现行容器规格，只重读 TOML）。
- **主仓库统一入口**：`../deploy/internal/sandbox-deploy.sh`（读
  `deploy/internal/sandbox-deploy.toml` 分发；本仓条目
  `dir=openshell-gateway, target=lxc-compose, vmid=<host-id>`）。

## 纪律

- **路由域**：`server_sans = ["*.sandbox.codeaudit.internal"]` 由 `ensure` 幂等
  钉住。沙箱暴露服务 URL 形如
  `http://{workspace}--{sandbox}--{service}.sandbox.codeaudit.internal:8080/`；
  旧默认域 `openshell.localhost` 仍被网关接受作兜底。
- **客户端解析**：`*.sandbox.codeaudit.internal` 路由域与网关地址均属纯内部
  解析（hosts 条目/内部解析手段，不依赖公共 DNS）；缺省链路走
  `host.docker.internal` 别名零 DNS 依赖（manager `gatewayEndpoint`
  同口径）。
- 部署主机 运行副本被覆盖前自动留 `*.bak.<时间戳>`。

## 测试与防回归

本仓"无源码"不等于"无测试"——测的就是配方与脚本本身（离线用例 + 变异
注入自检，无需 docker/部署主机；项数以 `tests/run.sh` 当次输出为准）：

```bash
bash tests/run.sh --fast          # 静态层：TOML/compose parse + 全键口径 + REMOTE 契约（秒级）
bash tests/run.sh                 # 全量离线：+ ensure/push/patch/备份 行为级（stub docker/远程执行 + 真 sqlite）
bash tests/run.sh --selfcheck     # 变异自检：注入历史缺陷，证明测试必然拦截
bash tests/run.sh --with-runtime  # + 真 部署主机 只读门禁（check/verify/status/image）
```

- **门禁顺序**：commit 前 `--fast`（pre-commit 钩子自动：`git config core.hooksPath tests/githooks`）；
  改 compose/TOML/脚本后加跑全量 `bash tests/run.sh`；部署前仍走
  `./deploy.sh --check` + `./gateway_lifecycle.sh verify`（或 `--with-runtime`）——
  现网生产面 **缺省零 env 直达**（两脚本 `DEPLOY_DIR` 缺省即
  `/opt/codeaudit/openshell-gateway`，见"现役部署"；`DEPLOY_DIR` env 透传

- **防回归机制**（`tests/REGRESSIONS.md`）：每个历史缺陷类别（REMOTE 空串契约、
  R2 command:[]、R3 ensure 自足三连、R4 server_sans 单行、R5 parse 重键、
  R6 端口同号三角、R7 挂载只读、R8 文档失真、R9 镜像 digest 钉扎、
  R10 gateway.db 备份）→ 静态+行为守卫用例 → 变异注入
  证明。**新缺陷修复必须一次补齐：修复 + 守卫用例 + 注入 + 档案加行**，缺一不算收口。

  契约（`${REMOTE-…}`），T-B1//M-4/M-5 守住。

## 镜像重建（非日常路径）


`COPY deploy/docker/.build/prebuilt-binaries/<arch>/…` 指向的 staging 布局属
**上游 openshell 源码仓**——本仓不含二进制，从源码重建需在上游仓跑其构建工作流
后再取产物）。日常一律用 ghcr.io 预构建镜像（digest 钉扎）。

## gateway.db 备份（R10）

已知事故模式：一键重装会重置 `/var/lib/openshell`，provider/route 注册面全失，
且旧密文配旧密钥同亡——**快照必须 db+tls 成套**。`gateway_db_backup.sh`：

- **backup**：一致性快照（sqlite3 CLI `.backup` 在线备份 API → python3 标准库
  兜底；双工具俱缺=拒拷贝硬失败，绝不裸 cp 可能撕裂的库）→ 快照内
  `integrity_check` 必须过 → tls 树 tar 成套 → manifest（sha256/工具/策略）→
  轮转（`KEEP` 缺省 14）。备份根缺省 `/var/backups/openshell-gateway`——
  **刻意在 `/var/lib/openshell` 之外**（网关侧重装不连带抹备份）；残留风险：
  与 部署主机 同盘，整机重建即失，离机副本属后续运维项。
- **verify [snap]**：双闸=manifest sha256 → `integrity_check`（独立拦截撕裂）。
- **restore <snap>**：网关在跑（TCP 探测通）=拒恢复；恢复前先对现状安全快照
  （回滚锚），再回放 db+tls；恢复源快照受轮转保护（安全快照的轮转不删它）。
- **list**：快照清单（名字/时间/工具/tls 在否）。

定时备份投运是**生产侧动作**：按操作单（cron 行、验证步骤、回滚）经 deploy.sh
下发链执行。**cron 行必须 `REMOTE=""`+bash 前缀**（脚本缺省远程前缀，目标机
内不可用；bash 前缀不依赖执行位）。

## 与其他仓

- `../manager/`：网关的 HTTP/JSON 管理面（纯传输管道；生命周期逻辑只在本
  仓脚本，manager 不暴露任何生命周期接口）。
- 沙箱镜像（如 dsh-pentest-sse）：由网关按需拉起
  （`spec.template.image` 钉定），本仓不管理镜像构建。

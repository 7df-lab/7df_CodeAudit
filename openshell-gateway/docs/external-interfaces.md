# 外部接口 — 预期输入 / 预期输出

> 本仓不含网关源码，"外部接口"= 本仓工件（脚本 / compose / TOML——原两上游重建

> 与**仓外实体**（人、主仓库部署链、docker、上游镜像、部署主机 宿主、网关二进制）之间
> 的全部契约。每条接口给出：调用方、预期输入、预期输出、失败语义。


---

## E1. `deploy.sh` — 差量下发 CLI

**调用方**：人（手动）、主仓库 `deploy/internal/sandbox-deploy.sh`（统一分发器，`./deploy.sh "$action"` 形式）、`deploy/internal/production-deploy.sh` 间接触达（实际直调 gateway_lifecycle.sh）。

**命令行输入**：`deploy.sh [deploy|check|status|start|stop|restart|logs [N]]`，缺省 `deploy`；未知字 → usage 到 stderr、**exit 2**。

**环境变量输入**：

| 变量 | 缺省 | 语义 | 契约要点 |
|---|---|---|---|
| `REMOTE` | `ssh <deploy-host> --` | 远程命令前缀 | **空串 = 本机执行** |
| `VMID` | `<vmid>` | `远程执行 push` 的目标 部署主机 id | 仅文件推送用 |
| `DEPLOY_DIR` | `/opt/codeaudit/openshell-gateway` | 部署主机 内运行目录 | 与 gateway_lifecycle.sh 共享默认 |

**各子命令预期输出**（stdout，人读；exit 除非注明否则 0）：

| 子命令 | 前置动作 | 预期输出 | 失败语义 |
|---|---|---|---|
| `check`/`--check` | 对 3 文件逐一 md5 本地 vs 远端 | 逐行 `drift: <file>`；全同步时 `in sync`；有漂移时追加一行 `^ CD differs from 部署主机 runtime; run deploy.sh to apply` | 仓内文件缺失 → `missing in CD: <file>` stderr、exit 1；**漂移也是失败**——主仓库 sandbox-deploy.sh check 按退出码聚合，旧"漂移 exit 0"曾令 CD↔部署主机 漂移在门禁静默通过 |
| `deploy` | 同上 md5 差量 | **REMOTE 非空**：逐文件 `unchanged: <f>` 或 `pushed: <f>`；**REMOTE=""（本机模式）**：打 `REMOTE 空=本机模式，跳过推送`，不做任何推送（B1-5 审计修复，push 路径纳入 REMOTE 契约）；全同步时先打 `openshell-gateway: in sync, ensure only`；结束前执行 `gateway_lifecycle.sh ensure`（其输出透传） | 任一仓内文件缺失 exit 1；ensure 失败则整体失败 |
| `status`/`start`/`stop`/`restart` | — | `exec gateway_lifecycle.sh <cmd>`，输出与退出码完全透传 | 同 lifecycle 对应子命令 |
| `logs [N]` | — | `exec gateway_lifecycle.sh logs [N]`（N 缺省由 lifecycle 定 50） | 同 lifecycle |

**deploy 子命令的副作用序列**（差量下发契约）：

0. **REMOTE=""（本机模式）短路**：跳过 1–2 的全部远程动作（`远程执行 push` 是宿主→部署主机 专用，本机无 部署主机 可推），打印提示后直接进 3；文件以仓内目录为事实源
1. `run_remote mkdir -p $DEPLOY_DIR`
2. 对每个 md5 有差异的文件：远端 `cp <f> <f>.bak.<YYYYMMDDHHMMSS>`（失败容忍，`|| true`）→ **`远程执行 push $VMID $f $DEPLOY_DIR/$f`** → `pushed: <f>`
3. `./gateway_lifecycle.sh ensure`（同目录相对调用，继承全部环境）

**外部依赖要求**：`md5sum`（两端）、远端 `cp/mkdir`；**文件推送只支持 远程执行 前缀**（硬编码 `远程执行 push`，ssh 类 REMOTE 推不了文件，只能 check/status）。

---

## E2. `gateway_lifecycle.sh` — 生命周期 CLI

**调用方**：`deploy.sh`（ensure/status/start/stop/restart/logs）、主仓库 `deploy/internal/production-deploy.sh`（`REMOTE="" VMID="" DEPLOY_DIR=<主仓库内路径> ./gateway_lifecycle.sh ensure`，生产态本机 daemon 部署链）、人。

**命令行输入**：`gateway_lifecycle.sh <ensure|verify|status|start|stop|restart|recreate|image|logs [N]>`；无参/未知字 → 头注释 2–40 行作 usage 到 stdout、**exit 2**（`image`=digest 钉扎只读对账口，R9，见下表）。

**环境变量输入**（全部有缺省，脚本可零配置直跑）：

| 变量 | 缺省 | 语义 |
|---|---|---|
| `REMOTE` | `ssh <deploy-host> --` | 命令前缀；**空串 = 本机执行**（`${REMOTE-…}`，dind 实测固化的契约，回归档案） |

| `SERVICE` | `gateway` | compose 服务名，必须等于 compose 文件的服务键 |
| `ROUTING_DOMAIN` | `sandbox.codeaudit.internal` | ensure 钉住的路由域 |
| `LIVENESS_HOST`/`LIVENESS_PORT` | `127.0.0.1`/`8080` | TCP 存活探测目标（不是 HTTP！8081 健康端点发布但不可达，见 data-flows.md D2） |
| `LIVENESS_TIMEOUT_SECS` | `60` | 存活等待上限 |
| `JWT_DIR` | `/var/lib/openshell/tls/jwt` | JWT 签名密钥目录（须与 TOML gateway_jwt 段同源） |
| `GATEWAY_IMAGE` | `ghcr.io/nvidia/openshell/gateway@sha256:05cf77bb…` | generate-certs 与 `image` 对账用的镜像 ref（**与 compose image 缺省一字不差**，守卫；全 ref 可覆盖，digest/tag 均可） |

**各子命令预期输出**：

| 子命令 | 预期 stdout | 预期 exit | 副作用 |
|---|---|---|---|
| `ensure` | JWT/supervisor 自举提示（如触发）→ `gateway container absent/not running — compose up -d`（仅容器缺失时）→ `enforcing routing domain …` + `rewrote/inserted server_sans …` + `backup: …`（仅需钉域时）或 `routing domain already enforced: server_sans=…` → `gateway liveness OK (…)` → `verify OK: server_sans=…, liveness …` | 0；任步失败非 0 | 幂等：改 TOML（留 .bak）、compose restart（仅改域时）、compose up -d（仅容器缺失时）、compose up -d **--force-recreate**；**全新宿主自足**：预置 JWT 密钥（一次性 generate-certs，探测=alpine:3.19 容器内 test，见 I4）、补拉 supervisor 镜像（`:local` 404 则拉 `:latest` retag，缺失二进制名则现场构建别名镜像） |
| `verify` | `verify OK: server_sans=["*.sandbox.codeaudit.internal"], liveness 127.0.0.1:8080` | 0；server_sans 不符或 TCP 不通 → `ERROR: …` stderr、1 | 只读，不改任何东西 |
| `status` | `== compose service ==` + compose ps 输出、`== routing domain ==` + `server_sans = <值>`（未设时 `(<unset> -> gateway default: openshell.localhost)`）、`== liveness ==` + `OK (…)`/`DOWN` | 0；DOWN 时 1 | 只读 |
| `start` | compose start 输出 + `gateway liveness OK` | 0 / 超时 1 | compose start |
| `stop` | compose stop 输出 | 0 | compose stop |
| `restart` | compose restart 输出 + liveness OK | 0 / 超时 1 | `compose restart`（**保留现行容器规格**，只重读 TOML——日常口径） |
| `recreate` | stderr 三行 WARNING（command: [] 将生效、须复验发布端口可达性）+ liveness OK | 0 / 超时 1 | `compose up -d`（**应用 compose 文件**——危险操作，纪律=别随手 recreate） |
| `logs [N]` | `compose logs --tail=N gateway` 原样输出（N 缺省 50） | compose 的退出码 | 只读 |
| `image` | `pinned: <GATEWAY_IMAGE>` + pinned/running 两镜像 ID + `image reconcile OK` | 0；漂移/容器不在 → `IMAGE DRIFT`/`ERROR` stderr、1 | 只读——比镜像 ID 非 ref 字符串（Config.Image 停在创建时写法），digest 钉扎（R9）的对账口；漂移消除走 recreate（生产=） |

**对远端宿主的能力要求**：`bash`（heredoc 补丁经 `bash -s` 下发）、`sed/grep/awk/tr/head/cp/test/md5sum`、`docker` CLI、`/dev/tcp`（bash 内建探测）。

---

## E3. 网关运行时服务面（本仓配置所控制、部署链所依赖）

本仓不写网关代码，但 TOML/compose 的每一行都直接决定该服务面对外表现；部署链（manager、openshell CLI、沙箱）按以下口径消费，**改配置前必须核对**：

| 服务面 | 输入 | 预期输出 / 行为 | 由哪个配置决定 |
|---|---|---|---|
| gRPC 控制面 `:8080` | manager(:18800)/openshell CLI 的 sandbox/provider 调用（protobuf over gRPC） | 沙箱编排（DooD 拉起兄弟容器）、provider 存储、令牌签发；`allow_unauthenticated_users = true` → **控制面无鉴权**，信任边界 = 部署主机 内部（端口不得出公网） | `gateway.toml` [openshell.gateway] + [auth]；compose ports 8080 |

| 服务路由域 | `http://{workspace}--{sandbox}--{service}.sandbox.codeaudit.internal:8080/` | 按 SAN 路由到对应沙箱服务端口；旧默认域 `openshell.localhost` 仍兜底接受 | `server_sans`（lifecycle ensure 幂等钉住） |
| 沙箱回调 | 沙箱容器 → `host.openshell.internal:8080`（网关签发的 JWT，ttl 3600s） | 回到网关 gRPC；**宿主端口必须同号发布 8080** 才能路由进网关容器 | `grpc_endpoint` scheme + compose 端口映射 |

---

## E4. `docker-compose.yml` → docker compose 引擎

**输入**：

| 项 | 预期值 |
|---|---|
| 调用形态 | `docker compose --project-directory <dir> -f <dir>/docker-compose.yml <cmd>`（lifecycle 的 compose() 包装）；主仓库 `production-deploy.sh` 用 `--project-directory openshell-gateway -f openshell-gateway/docker-compose.yml` |
| 环境变量 | `GATEWAY_IMAGE`（缺省 digest 钉扎 ref，R9——整 ref 覆盖，compose 与 lifecycle 同源）、`OPENSHELL_PORT`（缺省 8080）、`OPENSHELL_HEALTH_PORT`（缺省 8081） |
| 宿主前置 | `/var/run/docker.sock` 存在（DooD）；`/var/lib/openshell` 宿主目录（`create_host_path: true` 会建）；compose 文件同目录有 `gateway.toml`（相对路径挂载源） |

**预期产物**：容器名随 compose project 目录、发布 `0.0.0.0:8080→8080` 与 `0.0.0.0:8081→8081`、网络 `codeaudit-sandbox-gateway-net`（显式命名，非目录名派生的 docker_default）。

**关键语义（改前必读）**：`command: []` 清掉镜像默认 CMD（`--bind-address 0.0.0.0 --port 8080`），否则 CLI flags 静默压过 TOML 的 `bind_address=127.0.0.1:8080`（回归档案）；`up -d` 应用文件、`restart` 保留规格——两者的差别就是 recreate 纪律的根据。

---

## E5. `gateway.toml` → 网关二进制配置 schema

**输入链**：容器内只读挂载 `/etc/openshell/gateway.toml` ← compose bind `./gateway.toml`；加载开关 env `OPENSHELL_GATEWAY_CONFIG=/etc/openshell/gateway.toml`。

**预期键值**（全键清单 + 违反后果）：

| 键 | 预期值 | 违反后果 |
|---|---|---|
| `[openshell] version` | `1` | 二进制拒绝加载 |
| `server_sans` | `["*.sandbox.codeaudit.internal"]`，**全文件恰好一行** | 偏离 → verify 失败；多行 → lifecycle 只认第一行，第二行成暗雷 |
| `bind_address` | `127.0.0.1:8080` | 配 0.0.0.0 破坏"仅 loopback+桥接自动监听"口径 |
| `health_bind_address` | `127.0.0.1:8081` | — |
| `log_level` | `info` | 网关进程日志级别（A-7 补记：曾三文档+README 零提及） |
| `compute_drivers` | `["docker"]` | — |
| `disable_tls` | `true` | — |
| `drivers.docker.default_image` | `ghcr.io/nvidia/openshell-community/sandboxes/base:latest` | `sandbox create` 不带 `--from` 拉它 |
| `drivers.docker.supervisor_image` | `ghcr.io/nvidia/openshell/supervisor:local` | `:local` 是本地构建约定 tag，上游 404——全新宿主必须靠 ensure 的 :latest retag 自举，缺了启动即崩（R3） |
| `image_pull_policy` | `IfNotPresent` | — |
| `sandbox_namespace` | `openshell` | 沙箱容器名前缀 |
| `grpc_endpoint` | `http://host.openshell.internal:8080` | scheme 保留、host/port 被驱动替换；端口必须与发布端口同号 |
| `auth.allow_unauthenticated_users` | `true` | 改 false → manager/CLI 全挂（部署链未配鉴权） |
| `gateway_jwt.*` | 三路径均在 `/var/lib/openshell/tls/jwt/`（与 lifecycle `JWT_DIR`、compose bind 同源）；`gateway_id=openshell-docker`；`ttl_secs=3600` | 路径漂移 → ensure 预置的密钥落不进网关读取位置，启动即崩 |
| **禁止项** | **`OPENSHELL_DB_URL` 不得出现在 TOML**（上游显式禁止，防密钥 URL 入库；只许 env） | 出现即配置被拒 |

---

## E6. 上游重建路径

- `Dockerfile.gateway` / `Dockerfile.supervisor` 已删除（release-deploy-pipeline §6 2d：
  staging 路径属上游仓、本仓无二进制，从本仓 build 必失败=预期行为；文件仅余 md5
  漂移检查消费=死面）。上游重建需求出现时按上游仓配方另立，不恢复本仓副本。
- `command: []` 对偶语义（compose 清镜像默认 CMD 让 TOML 接管）不变，见 E4。
- **日常路径**：一律 ghcr 预构建镜像（digest 钉扎 R9）。

---

## E7. 远程宿主（部署主机）契约

| 项 | 契约 |
|---|---|
| REMOTE 前缀 | 形如 `ssh <deploy-host> --` 或空串（本机）；`run_remote` 刻意分词（前缀含空格是特性） |
| 运行目录 | `/opt/codeaudit/openshell-gateway`（`DEPLOY_DIR` 可改，改了须同步 compose 相对挂载仍成立；R11 钉值） |
| 文件推送 | 仅 `远程执行 push $VMID`（见 E1）；注意 `远程执行 push` 不经 REMOTE 前缀、直接调本机 远程执行 二进制——REMOTE=""（本机=部署主机 自身）时无 远程执行 可用，该形态只支持 check/status 类只读动作与 lifecycle 的本机 compose 操作 |
| 备份 | 运行副本被覆盖前自动 `*.bak.<时间戳>`（deploy.sh 推送前 + patch_server_sans 改 TOML 前各留一次） |
| 拓扑 | 每宿主单网关（`/var/lib/openshell` 同路径 bind 不命名空间化） |

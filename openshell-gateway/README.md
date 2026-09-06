# openshell-gateway

OpenShell Gateway（NVIDIA 开源沙箱网关）在 codeaudit 体系里的**部署事实源**
（source of truth）。本仓**不含网关源码**——网关本体用上游预构建镜像
`ghcr.io/nvidia/openshell/gateway`；本仓只承载部署配方（compose / TOML /
生命周期脚本）与运行纪律。**改部署一律改这里，用 `deploy.sh` 下发**，不要
直接改 LXC 里的运行副本。

网关职责：沙箱容器编排（DooD，在宿主 docker 里拉起兄弟容器）、推理路由与
provider 存储、沙箱端口暴露（服务路由域）。

调用链：`openshell CLI / manager(:18800) → 网关 gRPC :8080 → DooD 拉起沙箱容器 → 沙箱回调 host.openshell.internal:8080`

## 仓库位置与现役部署

本仓是 `codeaudit-umbrella` 的子模块 `openshell-gateway/`（GitLab
`admins/codeaudit/openshell-gateway`），2026-09-05 自
`CD/openshell-gateway/` 原样析出（fresh init，首提交 = 当前文件集）。

现役部署（2026-09-05 实测）：

- LXC 107（PVE 客户机 `docker`，gateway.internal = `gateway.internal`）
- 运行目录 `/root/os-deploy/deploy/docker/`（本仓 `deploy.sh` 的下发目标）
- 容器 `docker-gateway-1`，镜像 `ghcr.io/nvidia/openshell/gateway:latest`

## 文件

| 文件 | 作用 |
|---|---|
| `docker-compose.yml` | 网关容器编排：DooD（挂 docker.sock，user 0）、发布 8080/8081、`command: []` 让 TOML 接管配置、同路径 bind `/var/lib/openshell` |
| `gateway.toml` | 网关全部配置：bind、compute_drivers（docker）、`server_sans`（服务路由域）、JWT、auth；容器内挂载于 `/etc/openshell/gateway.toml`（只读） |
| `deploy.sh` | 本仓 → LXC 同步（md5 比对、仅推变化文件、远端留 `.bak.<时间戳>`）+ `gateway_lifecycle.sh ensure` 生效 |
| `gateway_lifecycle.sh` | 生命周期：ensure / verify / status / start / stop / restart / recreate / logs |
| `Dockerfile.gateway` / `Dockerfile.supervisor` | 从上游源码重建镜像用（staging 路径属上游仓，本仓无二进制；**日常部署用 ghcr 预构建镜像，不参与**） |

## 端口与可达性（2026-09-05 实测）

| 端口 | 发布 | 实际可达性 |
|---|---|---|
| 8080 gRPC | `0.0.0.0:8080→8080` | **可达**（manager / openshell CLI 全走这里）。TOML 绑容器内 loopback，但 docker driver 自动在桥接接口加监听，发布端口转发可达 |
| 8081 health | `0.0.0.0:8081→8081` | **发布了但不可达**（宿主侧 curl reset）：`/healthz` `/readyz` 在容器内仅绑 loopback，且健康端点**没有**桥接监听。存活探测改用：TCP 8080（`gateway_lifecycle.sh` 的 liveness）或 manager `GET /api/v1/gateway/health`（gRPC 级，部署链实际用的就是这个） |

LXC 107 上 8080/8081 归本网关占用（CodeAudit API 网关因此改发布 8090，
见伞仓 `deploy/prod/`）。

## 配置要点（gateway.toml + compose）

- **TOML 是唯一配置源**：compose `command: []` 清掉镜像默认 CMD
  （`--bind-address 0.0.0.0 --port 8080`）——CLI flags 优先级高于 TOML，
  不清掉则 `bind_address = "127.0.0.1:8080"` 被静默忽略。
- **docker driver**：`default_image`（`sandbox create` 不带 `--from` 时拉
  base:latest）、`supervisor_image`（首启抽取 `openshell-sandbox` 二进制，
  缓存到 `XDG_DATA_HOME` 复用）、`sandbox_namespace = "openshell"`（沙箱
  容器名前缀）、`grpc_endpoint = http://host.openshell.internal:8080`
  （沙箱回调端点——宿主端口必须同号发布 8080 才能路由到网关容器）。
- **鉴权取舍**：`allow_unauthenticated_users = true`——gRPC 控制面无鉴权，
  信任边界 = LXC 内网（端口只发布在 LXC，不出公网）。`gateway_jwt`
  （密钥 `/var/lib/openshell/tls/jwt/`，ttl 3600s）供网关向沙箱签发回调
  令牌，不是控制面鉴权。
- **`OPENSHELL_DB_URL` 只能走 env**（TOML 显式禁止，防密钥 URL 入库）：
  现值 sqlite `/var/lib/openshell/gateway.db`。
- **`/var/lib/openshell` 同路径 bind**（宿主 == 容器绝对路径一致）：沙箱
  创建时宿主 docker daemon 要按容器内写下的路径解析 bind source，命名卷
  无法满足；拓扑约定每宿主单网关。
- **extra_hosts**：`host.docker.internal` / `host.openshell.internal` →
  host-gateway；pentest lab 域（`target/db/redis/s3/gateway.internal`）→
  host-gateway——公共 DNS 的 `*.internal` 已停靠过期域页面，内网自解析。

## 部署与生命周期

```bash
./deploy.sh --check           # 本仓与 LXC 运行副本的漂移（不改任何东西）
./deploy.sh                   # 差量下发并生效（配置无变化则不重启）
./gateway_lifecycle.sh status # 容器状态 / 路由域 / 存活
./gateway_lifecycle.sh logs 100
```

- **deploy.sh**：对 4 个文件（compose/toml/两个 Dockerfile）做 md5 比对，
  仅推变化的（远端先留 `.bak.<时间戳>` 备份），末尾 `ensure` 生效。
- **ensure**：幂等钉 `server_sans`（变了才 `compose restart` 重读 TOML）→
  TCP 8080 liveness → verify。**全新宿主自足**（2026-09-05 起）：①JWT 签名密钥
  缺失时一次性 `generate-certs` 预置（网关不自动生成，缺了启动即崩）；②
  supervisor 镜像缺失时拉取（TOML 钉的 `:local` 类本地 tag 上游 404 → 拉同仓
  `:latest` retag）。
- **REMOTE=""（空串）= 本机执行契约**（`-` 非 `:-`，空串不得触发 pct 缺省）；
  伞仓 `deploy/production-deploy.sh` 依赖此语义在本机 daemon 直接部署。
- **别随手 `recreate`**（`compose up -d`）：会应用 compose 的
  `command: []`，TOML 的 `bind_address=127.0.0.1:8080` 接管，发布端口
  可达性必须重验。日常走 `deploy.sh` / `ensure` / `restart`
  （`compose restart` 保留现行容器规格，只重读 TOML）。
- **伞仓统一入口**：`../deploy/sandbox-deploy.sh`（读
  `deploy/sandbox-deploy.toml` 分发；本仓条目
  `dir=openshell-gateway, target=lxc-compose, vmid=107`）。

## 纪律

- **路由域**：`server_sans = ["*.openshell.internal"]` 由 `ensure` 幂等
  钉住。沙箱暴露服务 URL 形如
  `http://{workspace}--{sandbox}--{service}.openshell.internal:8080/`；
  旧默认域 `openshell.localhost` 仍被网关接受作兜底。
- **客户端解析**：`gateway.internal` 与 `*.openshell.internal` →
  gateway.internal（hosts / 内网 DNS，公共 DNS 不可用）。
- LXC 运行副本被覆盖前自动留 `*.bak.<时间戳>`。

## 镜像重建（非日常路径）

`Dockerfile.gateway`（debian:13-slim + 预构建网关二进制）与
`Dockerfile.supervisor`（alpine:3.22 + `openshell-sandbox` 静态二进制 +
nftables）的 `COPY deploy/docker/.build/prebuilt-binaries/<arch>/…` 是
**上游 openshell 源码仓**的 staging 布局——本仓不含二进制，从源码重建需
在上游仓跑其构建工作流后再取产物。日常一律用 ghcr.io 预构建镜像。

## 与其他仓

- `../manager/`：网关的 HTTP/JSON 管理面（纯传输管道；生命周期逻辑只在本
  仓脚本，manager 不暴露任何生命周期接口）。
- 沙箱镜像（如 dsh-pentest-sse）：由网关按需拉起
  （`spec.template.image` 钉定），本仓不管理镜像构建。

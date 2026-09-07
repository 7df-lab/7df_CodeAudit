# 其他数据流转与代码交互

> 前两份文档覆盖了"仓工件 ↔ 仓外实体"（external-interfaces.md）与
> "仓工件 ↔ 仓工件"（internal-interfaces.md）。本文档收录剩余的全部数据流：
> 跨仓链路、宿主态资产、网络拓扑、时序契约。tests/run.sh 与伞仓 e2e
> （deploy/tests/run.sh）对其中的关键不变量有守卫，标注见各节。

---

## D1. 沙箱编排全链路（跨仓时序，核心业务流）

```
openshell CLI / manager(:18800, 纯HTTP→gRPC管道)
        │  gRPC :8080（控制面，无鉴权，信任边界=LXC内网）
        ▼
   网关容器 docker-gateway-1
        │  挂宿主 /var/run/docker.sock（DooD，非 DinD，非 privileged）
        ▼
   宿主 docker daemon 拉起兄弟沙箱容器
   （名前缀 openshell-*，镜像 = spec.template.image，如 dsh-pentest-sse:latest；
     base 兜底 = gateway.toml default_image）
        │  首启：从 supervisor 镜像抽取 openshell-sandbox 静态二进制
        │       → 缓存到 XDG_DATA_HOME=/var/lib/openshell（同路径 bind 才可被
        │         宿主 daemon 当 bind source 解析——命名卷不可替代的原因）
        ▼
   沙箱容器 ──回调──▶ host.openshell.internal:8080
        （extra_hosts → host-gateway；网关签发 JWT，ttl 3600s，密钥
          /var/lib/openshell/tls/jwt/；宿主端口必须同号发布 8080）
```

**关键不变量与守卫**：
- 端口同号三角：TOML `bind_address` 端口 8080 == compose 宿主发布缺省 8080 == `grpc_endpoint` 端口 8080。破一角 → 沙箱回调失联（守卫 T-D2）。
- `/var/lib/openshell` 宿主与容器**绝对路径相同**（守卫 T-A3 断言 bind source==target）。
- manager 侧真实健康探测 = `GET /api/v1/gateway/health`（gRPC 级），部署链实际用它，而非 8081。

## D2. 服务路由域暴露流

```
gateway.toml server_sans = ["*.openshell.internal"]
        │  （lifecycle ensure 幂等钉住；TOML 是唯一配置源）
        ▼
服务 URL：http://{workspace}--{sandbox}--{service}.openshell.internal:8080/
        │  客户端解析：gateway.internal 与 *.openshell.internal → gateway.internal
        │  （hosts/内网 DNS；公共 DNS 的 *.internal 停靠过期域页面，不可用）
        ▼
8080 发布端口 → 容器内 loopback bind —— docker driver 自动在桥接接口加监听，
沙箱/外部经发布端口可达（网关自己不加 0.0.0.0）
```

**8081 特例（实测口径，勿凭直觉改）**：健康端点发布了但宿主侧不可达——容器内仅绑 loopback 且健康端点**没有**桥接监听，curl reset。存活探测一律 TCP 8080（lifecycle liveness）或 manager gateway/health。

## D3. 宿主态资产与"清空→自愈"矩阵（ensure 自足的根据）

| 资产（均在宿主 `/var/lib/openshell`） | 产出者 | 清空后后果 | ensure 能否自愈 |
|---|---|---|---|
| `gateway.db`（SQLite：provider 存储、沙箱账目） | 网关 | provider 全丢（README 明示"清空即丢"，需重新 `provider create`） | **否** |
| `tls/jwt/{signing.pem,public.pem,kid}` | ensure 一次性 `generate-certs` | 网关启动即崩（签名密钥不自动生成，7fa75d6） | **是**（探测 `signing.pem` 缺失 → 预置） |
| supervisor 二进制缓存（XDG_DATA_HOME 下） | 网关首启从 supervisor 镜像抽取 | 下一沙箱首启重抽，无感 | 间接是（镜像自愈后） |
| supervisor 镜像（`…:local`，本地约定 tag） | ensure retag 自举 | 网关启动即拉取失败退出（6a04978） | **是**（inspect 缺失 → pull `:local` → 404 则 pull `:latest` retag） |
| 网关容器本体 | compose | ensure 对着空栈等 60s 超时（7ca41c4 原始缺陷） | **是**（`compose ps -q` 空 → 先 `up -d`） |
| LXC 运行目录 `/root/os-deploy/deploy/docker/*.bak.*` | deploy.sh / patch 备份链 | 只增不删，无消费方（人工回滚用） | — |

**时序契约**：ensure 的自举必须**全部先于** `wait_liveness`——顺序颠倒 = 自举失效（网关没起来，探测必超时）。

## D4. 网络拓扑

| 项 | 值 | 用途 |
|---|---|---|
| compose 显式网络 | `codeaudit-sandbox-gateway-net` | 伞仓部署链网络统一 `codeaudit-` 前缀；显式命名防目录名派生的 `docker_default`（7ca41c4 同批固化） |
| `extra_hosts` → host-gateway | `host.docker.internal`、`host.openshell.internal` | Linux docker 不自动提供；回调与宿主服务寻址的根 |
| `extra_hosts` → host-gateway | `target/db/redis/s3/gateway.internal` | pentest lab 域内网自解析（公共 DNS 不可用）；网关拉起的沙箱经同款别名访问 lab 设施 |

## D5. 部署差量数据流（deploy.sh 内部时序）

```
本地 4 文件 md5 ──比──▶ 远端同路径 md5
   ├─ 全等 → "in sync, ensure only"（仍跑 ensure 幂等收尾）
   └─ 差异 → 远端 cp 留 .bak.<ts> → pct push 覆盖 → "pushed: <f>"
                └──────────▶ gateway_lifecycle.sh ensure（继承 REMOTE/DEPLOY_DIR）
                                ├─ 自举（JWT/镜像/容器缺失）
                                ├─ server_sans ≠ 期望 → patch + restart
                                └─ wait_liveness → verify
```

**漂移检查（check）是纯读**：md5 比对不改任何东西，漂移以 stdout 报告且 **exit 0**（"漂移≠失败"，失败仅限本地文件缺失 exit 1）——伞仓 `sandbox-deploy.sh check` 聚合依赖此语义。

## D6. 与 manager 仓的关系边界

- manager = 网关的 **HTTP/JSON 管理面纯传输管道**（HTTP :18800 → gRPC :8080），不持业务状态、**不暴露任何生命周期接口**——生命周期逻辑只在本仓两个脚本里，这是刻意的架构裁决（gateway_lifecycle.sh 头注释固化）。
- manager 的 `GET /api/v1/gateway/health` 是部署链事实上的网关存活信号（gRPC 级），与本仓 TCP 8080 liveness 双轨并存。
- provider 凭据实际存网关容器 `/var/lib/openshell/gateway.db`——备份/清空语义见 D3，manager 侧不落盘。

## D7. 镜像版本流转

| 镜像 | 流转 | 契约 |
|---|---|---|
| `ghcr.io/nvidia/openshell/gateway:latest` | compose `${IMAGE_TAG:-latest}`；`GATEWAY_IMAGE` 缺省同源 | 生命周期与 compose 必须同 tag 口径（T-D1 守卫） |
| `ghcr.io/nvidia/openshell/supervisor:local` | TOML 钉死；ensure 用同批次 `:latest` retag 兜底 | retag 推导须同时容忍 `@digest` 形态（剥 digest 再剥 tag）；版本对齐 = 与 gateway 同批次发布 |
| 沙箱镜像（dsh-pentest-sse 等） | 网关按 `spec.template.image` 按需拉起；伞仓口径一律 `:latest` 不钉版本 | 本仓不管理沙箱镜像构建（pb-C 管辖） |
| `default_image`（base:latest） | `sandbox create` 无 `--from` 时的兜底 | 上游 community 镜像，仅 TOML 引用 |

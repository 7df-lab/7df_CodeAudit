# manager 数据流转与代码交互 — 接口之外的链路

> 覆盖三份接口文档（docs/api-external.md、docs/api-internal.md）之外的
> 端到端数据流：配置、启动、上传管道、部署、可观测、错误传播。

## 1. 配置解析链（进程启动前定型）

```
os.environ ──覆盖──> config.json ──回落──> 内置默认
                        ▲
   OPENSHELL_MANAGER_CONFIG 可改指路径；全局缓存 _config_cache（只读一次）
```

| 值 | env | config.json 键 | 默认 | 消费者 |
|---|---|---|---|---|
| 监听地址 | `OPENSHELL_MANAGER_BIND` | `bind` | 127.0.0.1 | serve() |
| 监听端口 | `OPENSHELL_MANAGER_PORT` | `port` | 18800（坏值回落） | serve() |
| Bearer token | `OPENSHELL_MANAGER_TOKEN` | `tokenFile`（相对服务根）→ `token` | 空=免鉴权 | require_token |
| 网关端点 | `OPENSHELL_GATEWAY_ENDPOINT` | `gatewayEndpoint` | gateway.internal:8080 | SDK 客户端 |
| SDK 位置 | `OPENSHELL_LIB_PATH` | `libPath` | 服务根 libs/OpenShell/python | sys.path 注入 |
| 上传策略上限 | `OPENSHELL_MANAGER_MAX_UPLOAD_BYTES` | `maxUploadBytes` | 0=不限 | 上传 413 |
| 服务地址 | `OPENSHELL_MANAGER_URL` | `url` | http://127.0.0.1:18800 | **仅引擎侧读取**（共享 SSOT，服务自身不用） |

要点：
- **token 三级解析**：env > tokenFile（文件缺失=静默空，不致命）> config `token` 键。
- **共享 SSOT 防漂移**：引擎 `openshell_manager_client` 读同一份 config.json 的
  `url`/`token`/`tokenFile`，两端不会各配各的。
- **坏 config 不致命**：config.json 损坏/非对象 → 降级 `{}`，env-only 部署照常存活。
- **`OPENSHELL_LIB_PATH` 指错目录是 fail-loud**（RuntimeError：目录里没有 openshell/ 包），
  与其他键的静默回落 deliberate 不同——SDK 缺位必须启动期暴露。

## 2. 启动链（`python3 -m openshell_manager`）

```
__main__.py → api.serve():
  config.validate()                    # 非环回 bind 无 token → RuntimeError 拒启（硬红线）
  打印监听/网关/鉴权状态行 → uvicorn.run(create_app(), bind, port, log_level=warning)
```

`create_app()`：注册 19 条路由 + 4 个异常处理器（见 api-internal.md §4）；
`docs_url/redoc_url/openapi_url=None` 关闭（不泄漏接口面）。
鉴权收敛为 `Depends(require_token)`，`/healthz` 以"不挂依赖"实现豁免。

## 3. 上传端到端管道（最复杂的数据流）

```
HTTP multipart 请求体
  │  前置：415 非 multipart / 411 无 Content-Length / 413 超 max_upload_bytes（头与流两处）
  ▼
SpooledTemporaryFile（≤1MiB 留内存，超出落盘）── 全量收完后 seek(0)
  ▼
StreamingMultipartParser.parse()        # 手写 stdlib 解析器，64KiB 读块 + 回看尾
  │  fields{path,mode?} + file 流；结构坏 → 400
  ▼
校验 path 绝对 / mode 八进制            # 失败 400，未触达网关
  ▼
resolve_sandbox_id(name, workspace)     # 沙箱名 → UUID；不存在 → 404
  ▼
write_file_stream()   （run_in_threadpool，全程同步阻塞线程池）
  │  mkdir -p "$(dirname '…')" → 逐 720KiB(3字节对齐)块 base64 -d >|>> path.part → mv → chmod?
  │  任一远端命令非零退出 → rm -f path.part（尽力）→ RuntimeError
  ▼
200 {"path","bytes","chunks"}
```

**尺寸账（为什么是 720KiB）**：ExecSandbox 的 stdin 是单个 proto bytes 字段，
网关拒收 >1MiB 的请求消息（2026-09-06 实测 OUT_OF_RANGE "limit is 1048576"，
并非 gRPC 默认 4MiB）。720KiB 原始 → 960KiB base64 文本，留 ~64KiB 给命令框架。
3 字节对齐保证每块编码无 padding，沙箱内单条 `base64 -d` 流式解码不截断。

**内存账**：manager 侧内存恒定 <1MiB（spool + 单块缓冲），与上传文件大小无关——
32MiB 与 32GiB 走同一条路，仅受 maxUploadBytes 策略约束（纯防误操作）。

**失败语义**：`.part` 原子改名意味着失败的上传永远不会在目标路径留下截断文件；
清理是尽力而为（advisory，清理失败不掩盖原始异常）。

## 4. 请求执行路径上的线程模型

- FastAPI 路由分两类：`def`（同步，Starlette 自动丢线程池）与 `async def`（事件循环）。
  JSON 端点里 `create/exec/wait-ready/update-config/services/inference` 是 async def
  但内部 facade 调用是**同步阻塞 gRPC**——会占住事件循环直到 gRPC 返回
  （timeout=60s 兜底）。上传用 `run_in_threadpool` 显式离循环（大 body 解析不能占循环）。
- 这是已知取舍：manager 是内部管理面，QPS 极低（引擎单任务串行调用），
  简单性优先于吞吐。若未来出现并发场景需把同步 facade 调用全面线程池化。

## 5. 部署链（deploy/ → LXC 107）

```
manager/deploy/deploy.sh deploy
  → rsync/tar 源码 + Dockerfile.manager + env(600) 到 107:/root/os-deploy/deploy/openshell-manager
  → docker compose build（python:3.12-slim + pip fastapi/uvicorn + COPY 源码与 vendored SDK）
  → compose up（bind 0.0.0.0 + OPENSHELL_MANAGER_TOKEN 必配；healthcheck 探 /healthz）
  → 冒烟：healthz + 网关可达性
```

- **token 单一事实源**：`manager/deploy/env`（600，gitignore）→ 容器 `.env` →
  `OPENSHELL_MANAGER_TOKEN`；同一值另行供给引擎 prod compose（`OPENSHELL_MANAGER_TOKEN`）
  与 dsh-runtime 的凭据生成。
- **网络**：`codeaudit-sandbox-gateway-manager-net`（10.10.109.0/24 显式钉网段，
  避免撞 daemon 默认池里的物理 LAN 192.168.0.0/16）；`gateway.internal` 经
  extra_hosts → host-gateway 走 LXC 发布的 8080。
- **不挂 docker.sock**：manager 只经 gRPC 找网关，沙箱容器由网关 DooD 拉起——
  manager 被攻破也拿不到宿主 docker 控制面。
- **root Dockerfile（离线路径）与 deploy/Dockerfile.manager（自包含）同语义**，
  现役镜像 2.0.0 出自前者；改动源码后两条路径都必须能重建（契约测试离线，
  镜像冒烟在 deploy.sh）。

## 6. 可观测与健康链

| 探针 | 谁用 | 语义 |
|---|---|---|
| `GET /healthz` | compose healthcheck（10s 间隔）、人肉探活 | 进程活着（不触网关） |
| `GET /api/v1/gateway/health` | 伞仓文档口径的网关探测（8081 不可达时的替代）、deploy.sh 冒烟 | 网关 gRPC 可达 |
| 沙箱日志 `GET …/logs` | 引擎 `GET /api/v1/openshell/sandboxes[/…/logs]` 透传 | 沙箱 stdout/stderr 观测 |

服务自身 `log_level=warning, access_log=False`：不产访问日志，排障靠上游引擎日志
与响应错误体。502 兜底会把异常类型名带出（`RuntimeError: …`），是有意设计——
内部管理面要可诊断，而非对外保密面。

## 7. 错误传播全景（从网关到调用方）

```
gRPC 错误/SDK 异常 ─┐
ParseDict 失败 ─────┼─→ GatewayFacade 抛 ValueError/LookupError/RuntimeError/SDK 原生异常
远端命令失败 ───────┘         │
                              ▼
              api.py 异常处理器（api-internal.md §4 映射表）
                              ▼
        400 / 404 / 502 {"error": …} ──→ 引擎按码分流：400=调用方 bug 不重试；
        404=对象不存在；502=南向不可达，引擎侧按"网关不可达"重试/降级
```

502 的语义负担最重（触发上游重试/降级），所以纪律是**客户端格式错误绝不 502**——
历史上数值参数、spec/policy 未知字段、command 裸字符串三类先后泄漏成过 502，
已逐一收口为 400 并由契约测试锁定（REGRESSIONS.md R4/R10）。

## 8. 与兄弟仓的交互边界（不经理理 HTTP 面的部分）

| 对象 | 交互 | 边界 |
|---|---|---|
| `libs/OpenShell`（嵌套上游仓 NVIDIA/OpenShell） | 只取 `python` 子树（920K，已入库）入 sys.path 与镜像 | 上游升级时 `_stub`/`_channel` 私有属性是断点（api-internal.md §2） |
| engine 共享 config.json | 两端读同一份 `url`/`token`/`tokenFile` | manager 改端口/路径必须同步此文件（U7 SSOT） |
| openshell-gateway 仓 | 不直接交互；manager 只认 `gatewayEndpoint` 一个地址 | 网关 server_sans 路由域决定 services url 形态 |
| dsh-pentest-sse 仓 | 其 deploy.sh smoke 经 manager HTTP 面 | SKIP_SMOKE=1 可跳；冒烟失败不阻塞镜像就位 |

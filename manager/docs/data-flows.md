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
| Bearer token | `OPENSHELL_MANAGER_TOKEN` | `tokenFile`（相对服务根）→ `token` | 无配置=免鉴权（仅环回）；文件在而为空=503 fail-closed（R24） | require_token |
| 网关端点 | `OPENSHELL_GATEWAY_ENDPOINT` | `gatewayEndpoint` | host.docker.internal:8080 | SDK 客户端 |
| 上传策略上限 | `OPENSHELL_MANAGER_MAX_UPLOAD_BYTES` | `maxUploadBytes` | 2 GiB=2147483648（0=不限） | 上传 413 |
| 服务地址 | `OPENSHELL_MANAGER_URL` | `url` | http://127.0.0.1:18800 | **仅引擎侧读取**（共享 SSOT，服务自身不用） |

要点：
- **token 三级解析**：env > tokenFile（相对服务根）> config `token` 键。
  tokenFile **文件缺失**=未配置（静默空，不致命）；**存在但读失败**
  （EACCES/EIO 等 OSError）→ `TokenFileError` fail-closed（B3-2 审计修复：
  require_token 503 + stderr 日志；修复前吞异常当空，读失败瞬间鉴权失效）。
  文件解析结果 5s 缓存（`_token_cache`；env 分支不缓存）——require_token 是
  async 依赖，防每请求一次阻塞磁盘读；异常不落缓存，权限恢复即自愈。
- **共享 SSOT 防漂移**：引擎 `openshell_manager_client` 读同一份 config.json 的
  `url`/`token`/`tokenFile`，两端不会各配各的。
- **坏 config 不致命**：config.json 损坏/非对象 → 降级 `{}`，env-only 部署照常存活。
- **openshell SDK 是服务根顶层包**：无路径解析键，
  缺席 = 仓坏，ImportError 启动期直接暴露（原 `OPENSHELL_LIB_PATH`/`libPath`
  三级解析与 sys.path 注入机制随解绑退役）。

## 2. 启动链（`python3 -m openshell_manager`）

```
__main__.py → api.serve():
  config.validate()                    # 非环回 bind 无 token → RuntimeError 拒启（硬红线）
  打印监听/网关/鉴权状态行 → uvicorn.run(create_app(), bind, port, log_level=warning)
```

`create_app()`：注册路由 + 异常处理器（计数不写死——以 `tests/test_guardrails.py`
`EXPECTED_ROUTES` 快照与 api.py 处理器注册当次输出为准；处理器映射表见
api-internal.md §4）；`docs_url/redoc_url/openapi_url=None` 关闭（不泄漏接口面）。
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

并非 gRPC 默认 4MiB）。720KiB 原始 → 960KiB base64 文本，留 ~64KiB 给命令框架。
3 字节对齐保证每块编码无 padding，沙箱内单条 `base64 -d` 流式解码不截断。

**内存账**：manager 侧内存恒定 <1MiB（spool + 单块缓冲），与上传文件大小无关——
32MiB 与 32GiB 走同一条路，仅受 maxUploadBytes 策略约束（纯防误操作）。

**失败语义**：`.part` 原子改名意味着失败的上传永远不会在目标路径留下截断文件；
清理是尽力而为（advisory，清理失败不掩盖原始异常）。

## 4. 请求执行路径上的线程模型

- FastAPI 路由分两类：`def`（同步，Starlette 自动丢线程池）与 `async def`（事件循环）。
  全部 async def 端点的南向 facade 调用一律 `await run_in_threadpool(…)`
  （B3-1 审计修复；async 端点集以 api.py 当次 `async def` 路由注册为准，不写死
  枚举——旧文清单曾漏 files 上传与 workspaces 建区）——同步阻塞 gRPC 裸跑在
  事件循环上曾把 /healthz 探活与全部并发请求卡到 gRPC 返回（timeout=60s
  兜底），修复前实测慢 exec 在途时探活排队 2s+。
  上传路径自 起即为该形态（大 body 解析不能占循环），本次对齐。

## 5. 部署链（deploy/ → 部署主机）

```
manager/deploy/deploy.sh deploy
  → rsync/tar 源码 + Dockerfile.manager + env(600) 到 <host-id>:/opt/codeaudit/manager
  → docker compose build（python:3.12-slim + pip fastapi/uvicorn + COPY 源码与 vendored SDK；
    镜像 tag 变量化 ${MANAGER_IMAGE_TAG:-2.0.0}，deploy.sh 按树 sha 刷缺省——Phase2 2b）
  → compose up（bind 0.0.0.0 + OPENSHELL_MANAGER_TOKEN 必配；healthcheck 探 /healthz）
  → 冒烟：healthz + 网关可达性
```

- **token 单一事实源**：`manager/deploy/env`（600，gitignore）→ 容器 `.env` →
  `OPENSHELL_MANAGER_TOKEN`；同一值另行供给引擎 prod compose（`OPENSHELL_MANAGER_TOKEN`）
  与 dsh-runtime 的凭据生成。
- **网络**：`codeaudit-sandbox-gateway-manager-net`（172.28.109.0/24 显式钉网段，
  避免撞 daemon 默认池里的物理 LAN 192.168.0.0/16）；网关端点缺省
  `host.docker.internal:8080`（hosts 别名 → host-gateway 走 部署主机 发布的 8080，
  a4c1679 中性化；C3 同步：本表与 README 缺省列曾残留旧域样例值）。
- **不挂 docker.sock**：manager 只经 gRPC 找网关，沙箱容器由网关 DooD 拉起——
  manager 被攻破也拿不到宿主 docker 控制面。
- **无固定 container_name**（HA P2 878b449，/Q7 同批）：manager 无状态
  多副本就绪——服务寻址一律走 compose 服务名（DNS 轮询），不依赖容器名。
- **镜像单一路径 = deploy/Dockerfile.manager**（python:3.12-slim 自包含配方，

  退役（R31；其产物携带基础镜像 .token 的历史警示见 REGRESSIONS R29 注记）。
  契约测试离线，镜像冒烟在 deploy.sh。

## 6. 可观测与健康链

| 探针 | 谁用 | 语义 |
|---|---|---|
| `GET /healthz` | compose healthcheck（10s 间隔）、人肉探活 | 进程活着（不触网关） |
| `GET /api/v1/gateway/health` | 主仓库文档口径的网关探测（8081 不可达时的替代）、deploy.sh 冒烟 | 网关 gRPC 可达 |
| 沙箱日志 `GET …/logs` | 运维/冒烟调试面 | 沙箱 stdout/stderr 观测 |

服务自身 `log_level=warning, access_log=False`：不产访问日志，排障靠上游引擎日志
与服务端 stderr。未捕获异常兜底为 500 + 通用文案 `internal error`，`ExcType: msg`
细节进服务端 stderr 日志（B3-3：客户端不再看到异常类型/消息，服务端可诊断性不变）。

## 7. 错误传播全景（从网关到调用方）

```
gRPC 错误/SDK 异常 ─┐
ParseDict 失败 ─────┼─→ GatewayFacade 抛 ValueError/LookupError/RuntimeError/SDK 原生异常
远端命令失败 ───────┘         │
                              ▼
              api.py 异常处理器（api-internal.md §4 映射表）
                              ▼
        400 / 404 / 409 / 413 / 500 / 503 {"error": …} ──→ 引擎按码分流：400=调用方 bug 不重试；
        404=对象不存在；409=资源状态冲突（workspace 重名/删区含资源，切片3b，
        details 透传）；413=请求体/上传/exec 输出超限（R27）；500=manager 自身/
        南向未捕获故障（细节在服务端日志）；503=token 鉴权材料不可得（fail-closed，B3-2）
```

客户端格式错误**绝不 5xx** 是硬纪律（5xx 触发上游重试/降级）——历史上数值参数、
spec/policy 未知字段、command 裸字符串三类先后泄漏成过 502，已逐一收口为 400 并由
契约测试锁定（REGRESSIONS.md R4/R10）；B3-3 起兜底处理器本身也改 500 + 通用文案，
502 不再出现于本服务错误面。

## 8. 与兄弟仓的交互边界（不经理理 HTTP 面的部分）

| 对象 | 交互 | 边界 |
|---|---|---|
| `openshell/` 顶层包 | 普通包直接 import，随服务源码入镜像 | 上游同步时 `_stub`/`_channel` 私有属性是断点（api-internal.md §2；流程见 REGRESSIONS R31） |
| engine 共享 config.json | 两端读同一份 `url`/`token`/`tokenFile` | manager 改端口/路径必须同步此文件（U7 SSOT） |
| openshell-gateway 仓 | 不直接交互；manager 只认 `gatewayEndpoint` 一个地址 | 网关 server_sans 路由域决定 services url 形态 |
| dsh-pentest-sse 仓 | 其 deploy.sh smoke 经 manager HTTP 面 | SKIP_SMOKE=1 可跳；冒烟失败不阻塞镜像就位 |

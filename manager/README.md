# openshell-manager

OpenShell Gateway 的 **HTTP/JSON 管理面**：把网关 gRPC SDK 包装成 REST 风格
接口的**薄传输层**。自 2026-08-31 起它是引擎访问 OpenShell Gateway 的
**唯一**南向传输（引擎仓库的直连 gRPC 代码已删除，本服务不可达即南向能力
不可用，fail-loud）。

**信任边界（重要）**：本服务是**纯管道**——只执行、只观测、绝不裁决。
租约（TaskExecutionLease）、ScopeGuard、oracle/结论纪律全部留在引擎侧；
本服务不持有业务状态，凭据只透传（网关侧加密存储）。因为它能在沙箱内执行
命令，绑定纪律是硬红线：默认只绑 `127.0.0.1`；绑非环回地址**必须**配
token，否则拒绝启动（`config.py validate()`）；`/api/*` 全部 Bearer 鉴权
（`/healthz` 豁免）。

调用链：`引擎 / dsh-agent → manager(:18800, HTTP/JSON) → gateway.internal:8080 (gRPC) → 沙箱容器（网关 DooD 拉起）`

## 仓库位置

本仓是 `codeaudit-umbrella` 的子模块 `manager/`（GitLab
`admins/codeaudit/manager`）。2026-09-05 仓库重构时由原 `openshell-manager`
源码仓 + `CD/openshell-manager` 部署 overlay 合并而成：**源码与部署事实源
同居一仓**（部署配方见 `deploy/`；CD 已析出归档）。

架构沿革：stdlib `http.server` 起家 → ADR-174（2026-09-01，人类指令）整体
迁移 FastAPI/uvicorn，南向 gRPC 与对外 JSON 契约逐字节不变（契约测试锁定）；
镜像 1.0.0 → 2.0.0。

## 代码地图（以当前代码为准）

| 路径 | 职责 |
|---|---|
| `openshell_manager/__main__.py` | `python3 -m openshell_manager` 入口 → `api.serve()` |
| `openshell_manager/api.py` | FastAPI app 工厂：全部路由、鉴权依赖注入（`/healthz` 豁免）、统一错误契约 `{"error": msg}`、流式上传端点；JSON body 上限 8 MiB（`MAX_BODY_BYTES`） |
| `openshell_manager/gateway.py` | `GatewayFacade`：懒加载 vendored SDK、南向 gRPC 全操作、沙箱 name→UUID 解析（ADR-173） |
| `openshell_manager/upload.py` | 手写流式 multipart 解析器：720 KiB 分块（3 字节对齐 base64，编码后 960KiB < 网关 gRPC 实测 1 MiB 收包上限）经 exec stdin 写入沙箱，先建父目录再写 `.part` 后原子 mv，失败自清理 |
| `openshell_manager/config.py` | 配置解析（env > config.json > 内置默认）+ `validate()` 绑定纪律 |
| `openshell_manager/http_api.py` | 旧 stdlib 实现，**不再接线**，保留作行为参照 |
| `tests/test_contract.py` | 26 条契约测试（假 SDK 门面 + 真 HTTP 层，离线无需网关） |
| `libs/OpenShell/python` | vendored openshell SDK（供应商树；整树 7.2G 几乎全是 Rust 构建产物，仅 python 子树 <1M 入镜像）。**python 子树 2026-09-05 起纳管入库**（Dockerfile COPY 输入，gitignored 会让 fresh clone 构建必败）；`libs/OpenShell` 本体是嵌套上游仓（NVIDIA/OpenShell），仅存于开发机本地 |
| `config.json` | 全局配置——与引擎共享的 SSOT（引擎 `openshell_manager_client.py` 读同一份 `url`/`token`/`tokenFile`，两端不会漂移） |
| `run.sh` | 宿主机开发态启动（127.0.0.1:18800） |
| `Dockerfile` | 2.0.0 镜像（离线构建路径，现役镜像由此产出）：基于 1.0.0 叠加 fastapi/uvicorn wheels（`build/wheels/` 不入 git） |
| `deploy/` | 生产部署事实源（compose + `deploy.sh` + `env.template`）→ LXC 107，详见 `deploy/README.md` |

## 运行

```bash
# 开发态（宿主机；免 token 仅限环回）：
./run.sh                          # = python3 -m openshell_manager，127.0.0.1:18800

# 契约测试（离线，26 条）：
python3 -m pytest tests/ -q       # 或 python3 tests/test_contract.py

# 生产（LXC 107 docker，容器 openshell-manager，镜像 openshell-manager:2.0.0）：
deploy/deploy.sh deploy           # 同步源码+产物+.env → compose build+up → healthz → 网关可达性
deploy/deploy.sh check|status|logs [N]
```

生产现役实例发布 `http://gateway.internal:18800`（bind 0.0.0.0，token 必配，
与 `deploy/env` 同值）。

## 镜像构建（2.0.0，ADR-174）

现役镜像 = 根 `Dockerfile` 离线路径（在 1.0.0 基底上叠加，自带 SDK/grpcio/
config.json/.token，构建期 `USER root` 装 fastapi+uvicorn）：

```bash
# 1) 宿主机备离线 wheels（构建上下文 build/wheels，不入 git）
pip3 download fastapi uvicorn -d build/wheels \
    --platform manylinux2014_x86_64 --python-version 3.12 --only-binary=:all:
# 2) 打包上下文推 LXC 107 构建镜像
tar --exclude='.git' --exclude='__pycache__' --exclude='build' \
    -czf /tmp/om_ctx.tgz Dockerfile openshell_manager build/wheels tests config.json
pct push 107 /tmp/om_ctx.tgz /tmp/om_ctx.tgz
pct exec 107 -- bash -c 'mkdir -p /root/om-build && tar -xzf /tmp/om_ctx.tgz -C /root/om-build \
    && cd /root/om-build && docker build -t openshell-manager:2.0.0 .'
# 3) 重建容器（沿用原容器 env/network 参数；或改走 deploy/ 的 compose 路径）
```

`deploy/Dockerfile.manager` 是自包含替代配方（python:3.12-slim 起步、pip
直装、compose 编排），与根 Dockerfile 产物同语义；首次在 107 走 compose
部署时用它收敛。

## 配置

解析优先级一律 **环境变量 > config.json > 内置默认**；config.json 路径可用
`OPENSHELL_MANAGER_CONFIG` 改指（默认服务根下）。

```json
{
  "url":             "http://127.0.0.1:18800",
  "bind":            "127.0.0.1",
  "port":            18800,
  "tokenFile":       ".token",
  "gatewayEndpoint": "gateway.internal:8080",
  "libPath":         "libs/OpenShell/python"
}
```

| 键 / 环境变量 | 默认 | 说明 |
|---|---|---|
| `bind` / `OPENSHELL_MANAGER_BIND` | `127.0.0.1` | 监听地址；非环回无 token = `validate()` 拒启 |
| `port` / `OPENSHELL_MANAGER_PORT` | `18800` | 监听端口 |
| `tokenFile` / `OPENSHELL_MANAGER_TOKEN` | `.token` | Bearer token；优先级 env > tokenFile（相对服务根）> config `token`；空 = 免鉴权（仅环回） |
| `gatewayEndpoint` / `OPENSHELL_GATEWAY_ENDPOINT` | `gateway.internal:8080` | 网关 gRPC 端点 |
| `libPath` / `OPENSHELL_LIB_PATH` | `libs/OpenShell/python` | vendored SDK 位置；回退链 = 服务自带树 > 引擎旧检出遗留路径 |
| `maxUploadBytes` / `OPENSHELL_MANAGER_MAX_UPLOAD_BYTES` | `0`（不限） | 上传策略上限，纯防误操作——上传为流式转发，内存恒定 <1 MiB，与文件大小无关 |
| `url` / `OPENSHELL_MANAGER_URL` | `http://127.0.0.1:18800` | **引擎侧**读取的服务地址（本服务自身不用） |

红线：`.token`、`deploy/env` 等密钥文件不入 git（gitignore）。

## API 面

`/healthz` 豁免鉴权；`/api/*` 全部要求 `Authorization: Bearer <token>`。

| 方法/路径 | 说明 |
|---|---|
| `GET /healthz` | 存活探针（免鉴权） |
| `GET /api/v1/gateway/health` | 网关可达性 |
| `POST /api/v1/sandboxes` | 创建 `{workspace, name?, spec?}`（spec = openshell SandboxSpec JSON，`spec.template.image` 钉沙箱镜像） |
| `GET /api/v1/sandboxes?limit=` | 全工作区清单（limit 默认 500） |
| `GET /api/v1/sandboxes/{name}?workspace=` | 查询单个 |
| `DELETE /api/v1/sandboxes/{name}?workspace=` | 删除 → `{deleted}` |
| `POST /api/v1/sandboxes/{name}/wait-ready` | `{workspace, timeout_seconds?=300}` |
| `POST /api/v1/sandboxes/exec` | 执行命令 `{sandbox_id, command, env?, workdir?, stdin_b64?, timeout_seconds?}`。**sandbox_id 必须传创建响应的 UUID `id` 字段，传沙箱名会 NOT_FOUND**；`command` 必须是字符串列表（裸字符串/混合类型 400，不触达网关）；`stdin_b64` 非法 base64 → 400 |
| `GET /api/v1/sandboxes/{name}/logs?workspace=&lines=&since_ms=` | 日志（lines 默认 2000） |
| `POST /api/v1/sandboxes/{name}/update-config` | 热更新策略 `{workspace, policy}` |
| `POST /api/v1/sandboxes/{name}/files` | 流式上传，**仅 multipart/form-data**（否则 415）、**必带 Content-Length**（否则 411）：表单字段 `path`（绝对路径必填，父目录自动 `mkdir -p`，含空格/通配符路径安全）、`mode`（八进制可选如 `0755`）+ 文件部分 `file` → `{path,bytes,chunks}`；`?workspace=` 缺省 default。流式转发：边收边按 720 KiB 分块经 exec stdin 写沙箱（网关收包上限实测 1 MiB），内存恒定 <1 MiB，单请求大小不限（仅受 `maxUploadBytes` 约束；8 MiB 的 `MAX_BODY_BYTES` 只管 JSON 接口） |
| `POST /api/v1/sandboxes/{name}/services` | ExposeService：沙箱端口暴露为网关服务 `{workspace, service, target_port, domain?=false}` → `{name,sandbox_id,sandbox_name,target_port,domain,url}`；重暴露同名即更新 |
| `GET /api/v1/sandboxes/{name}/services` | 该沙箱暴露服务清单（`?all_workspaces=true` 免 workspace；`limit`/`offset` 分页） |
| `DELETE /api/v1/sandboxes/{name}/services/{service}?workspace=` | 删除暴露；不存在也返回 `{deleted:false}` |
| `GET /api/v1/inference/route?workspace=` | 读推理路由 |
| `PUT /api/v1/inference/route` | 切路由 `{workspace, provider, model, no_verify?=false}` |
| `GET /api/v1/inference/providers?workspace=` | provider 清单 |
| `GET /api/v1/inference/providers/{name}?workspace=` | 单个 provider |
| `PUT /api/v1/inference/providers` | 创建/更新 `{workspace, name, type, credentials?, config?}` |

通用约定：

- **寻址双轨**：REST 路径参数一律用沙箱名（接口层内部解析 UUID，ADR-173）；
  唯 `/exec` 的 `sandbox_id` 走 UUID。
- **错误契约**统一 `{"error": msg}`：400（缺字段/坏 JSON/非对象 body/非法数值参数/
  command 非字符串列表/stdin_b64 非法/spec·policy 未知字段）、
  401、404（含 `no route for METHOD /path`）、411（上传缺 Content-Length）、
  413（JSON > 8 MiB 或超 `maxUploadBytes`）、415（上传非 multipart）、
  502（南向异常/未捕获兜底）。客户端格式错误一律 400，绝不泄漏成 502
  （502 会被上游按"网关不可达"重试/降级）。
- 上传先写 `.part` 全部落盘后原子改名，失败自动清理。

## 网关生命周期（兄弟仓）

manager 进程本身是纯传输层；LXC 107 内网关容器的部署与生命周期收敛在伞仓
兄弟子模块 `../openshell-gateway/`（原 `CD/openshell-gateway/`，不暴露
HTTP 接口）：`deploy.sh` 差量下发，`gateway_lifecycle.sh`
ensure/verify/status/start/stop/restart；`ensure` 幂等强制服务路由域
`server_sans = ["*.openshell.internal"]`（沙箱服务 URL 形如
`http://{workspace}--{sandbox}--{service}.openshell.internal:8080/`，旧默认
域 `openshell.localhost` 仍兜底）。拓扑与纪律见其 `README.md`。

## 引擎侧接入

```bash
# 生产（容器化实例，现役）：
export OPENSHELL_MANAGER_URL=http://gateway.internal:18800
export OPENSHELL_MANAGER_TOKEN=$(cut -d= -f2 deploy/env)
# 宿主机开发态实例（./run.sh）才用 127.0.0.1:18800
python3 engine/llm_config.py get     # 经微服务读路由
```

引擎内所有网关传输经 `engine/openshell_manager_client.py`（唯一传输层）；
`llm_config` / `scripts/configure_llm.py` 的 provider/route 管理同理。沙箱
日志可观测：引擎 `GET /api/v1/openshell/sandboxes[/…/logs]` 透传本服务。对
`RealOpenShellRuntime`、headless 子代理、探针与 live 脚本全部透明（同一
dict 边界契约）。联调基线（2026-08-31 全通）：llm get/force、
configure_llm get、沙箱生命周期+沙箱内推理、gateway_probe、cleanup。

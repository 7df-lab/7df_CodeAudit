# openshell-manager

OpenShell Gateway 的 **HTTP/JSON 管理面**：把网关 gRPC SDK 包装成 REST 风格

**唯一**南向传输（引擎仓库的直连 gRPC 代码已删除，本服务不可达即南向能力
不可用，fail-loud）。

**信任边界（重要）**：本服务是**纯管道**——只执行、只观测、绝不裁决。
租约（TaskExecutionLease）、ScopeGuard、oracle/结论纪律全部留在引擎侧；
本服务不持有业务状态，凭据只透传（网关侧加密存储）。因为它能在沙箱内执行
命令，绑定纪律是硬红线：默认只绑 `127.0.0.1`；绑非环回地址**必须**配
token，否则拒绝启动（`config.py validate()`）；`/api/*` 全部 Bearer 鉴权
（`/healthz` 豁免）。

调用链：`引擎 / dsh-agent → manager(:18800, HTTP/JSON) → host.docker.internal:8080 (gRPC，缺省经 hosts 别名解析；有自定义域名时 env/config 覆盖) → 沙箱容器（网关 DooD 拉起）`

## 仓库位置

本目录是主仓库的 `manager/` 模块。由原 `openshell-manager`
源码仓 + `CD/openshell-manager` 部署 overlay 合并而成：**源码与部署事实源
同居一仓**（部署配方见 `deploy/`）。

架构沿革：stdlib `http.server` 起家 → 整体
迁移 FastAPI/uvicorn，南向 gRPC 与对外 JSON 契约逐字节不变（契约测试锁定）；
镜像 1.0.0 → 2.0.0。

## 代码地图（以当前代码为准）

| 路径 | 职责 |
|---|---|
| `openshell_manager/__main__.py` | `python3 -m openshell_manager` 入口 → `api.serve()` |
| `openshell_manager/api.py` | FastAPI app 工厂：全部路由、鉴权依赖注入（`/healthz` 豁免）、统一错误契约 `{"error": msg}`、流式上传端点；JSON body 上限 20 MiB（`MAX_BODY_BYTES`） |
| `openshell_manager/gateway.py` | `GatewayFacade`：南向 gRPC 全操作、沙箱 name→UUID 解析 |
| `openshell_manager/upload.py` | 手写流式 multipart 解析器：720 KiB 分块（3 字节对齐 base64，编码后 960KiB < 网关 gRPC 实测 1 MiB 收包上限）经 exec stdin 写入沙箱，先建父目录再写 `.part` 后原子 mv，失败自清理 |
| `openshell_manager/config.py` | 配置解析（env > config.json > 内置默认）+ `validate()` 绑定纪律 |
| `tests/test_contract.py` | 契约测试（假 SDK 门面 + 真 HTTP 层，离线无需网关）；可 pytest 或直跑；用例数以 `verify.sh` 输出为准 |
| `tests/test_guardrails.py` | 守门测试：鉴权全覆盖/路由快照/README 文档实测化/分块上限/常量时间比较/SDK 在库/档案完整 |
| `REGRESSIONS.md` | 缺陷档案：R1 起每条缺陷绑定具名锁定测试 + 修复流程纪律（先红后修再记档）；末条编号以档案为准 |
| `.agent/verify.sh` | 交付门禁：pytest 全量 + 直跑模式 + deploy.sh 行为关卡三绿才可交付 |
| `docs/` | 接口文档三件套：`api-external.md`（外部 HTTP 契约）/`api-internal.md`（内部模块契约）/`data-flows.md`（数据流转与部署链） |
| `openshell/` | openshell SDK（网关客户端 + 生成 proto）——并入仓根为顶层包（原 `libs/OpenShell/python/openshell`，git mv 保历史；包源码零改动） |
| `config.json` | 全局配置——与引擎共享的 SSOT（引擎 runtime-service 的 `sandbox.ManagerRunner` 读同一份 `url`/`token`/`tokenFile`，解析序两侧同构：env > 引擎配置 > 共享 config.json > 默认，；两端不会漂移）。**gitignored 不随仓走**：fresh clone 须经 `deploy/config.template.json` 供给（deploy.sh `ensure-config`）或按模板手工重建 |
| `run.sh` | 宿主机开发态启动（127.0.0.1:18800） |
| `requirements.txt` | 运行时依赖清单（host 验证与镜像共用 SSOT） |
| `deploy/` | 生产部署事实源（compose + `deploy.sh` + `env.template`），详见 `deploy/README.md` |

## 运行

```bash
# 开发态（宿主机；免 token 仅限环回）：
./run.sh                          # = python3 -m openshell_manager，127.0.0.1:18800

# 契约+守门测试（离线；交付门禁）：
bash .agent/verify.sh            # = pytest tests/ 全量 + 直跑模式双检
python3 -m pytest tests/ -q      # 或 python3 tests/test_contract.py

# 生产（部署主机 docker，容器 openshell-manager，镜像 openshell-manager:2.0.0）：
deploy/deploy.sh deploy           # 同步源码+产物+.env → compose build+up → healthz → 网关可达性
deploy/deploy.sh check|status|logs [N]
```

生产现役实例发布 `http://192.0.2.122:18800`（bind 0.0.0.0，token 必配，
与 `deploy/env` 同值）。

## 镜像构建

唯一构建路径 = `deploy/Dockerfile.manager`（python:3.12-slim 起步、`pip
install -r requirements.txt` 直装、`USER 65534` 非 root 运行），由
`deploy/docker-compose.yml` 编排（build context=仓根，`.dockerignore` 挡
密钥/过程文件入上下文）。原根 `Dockerfile`（1.0.0 基底离线叠加路径，产物


```bash
# 生产（部署主机）：deploy/deploy.sh deploy   # 同步仓根树+deploy/ 产物 → compose build+up → healthz 门
# 只构建不动在跑容器（本地验证）：
OPENSHELL_MANAGER_TOKEN=dummy docker compose -f deploy/docker-compose.yml build
```

## 配置

解析优先级一律 **环境变量 > config.json > 内置默认**；config.json 路径可用
`OPENSHELL_MANAGER_CONFIG` 改指（默认服务根下）。

```json
{
  "url":             "http://127.0.0.1:18800",
  "bind":            "127.0.0.1",
  "port":            18800,
  "tokenFile":       ".token",
  "gatewayEndpoint": "host.docker.internal:8080"
}
```

| 键 / 环境变量 | 默认 | 说明 |
|---|---|---|
| `bind` / `OPENSHELL_MANAGER_BIND` | `127.0.0.1` | 监听地址；非环回无 token = `validate()` 拒启 |
| `port` / `OPENSHELL_MANAGER_PORT` | `18800` | 监听端口 |
| `tokenFile` / `OPENSHELL_MANAGER_TOKEN` | `.token` | Bearer token；优先级 env > tokenFile（相对服务根）> config `token`；文件不存在 = 未配置（免鉴权，仅环回）；**文件存在但为空 = 配置错误 → 503 fail-closed（R24）** |
| `gatewayEndpoint` / `OPENSHELL_GATEWAY_ENDPOINT` | `host.docker.internal:8080` | 网关 gRPC 端点（缺省经 hosts 别名解析，零 DNS 依赖；有自定义域名时覆盖） |
| `maxUploadBytes` / `OPENSHELL_MANAGER_MAX_UPLOAD_BYTES` | `2147483648`（2 GiB；0=不限） | 上传策略上限，纯防误操作——上传为流式转发，内存恒定 <1 MiB，与文件大小无关 |
| `url` / `OPENSHELL_MANAGER_URL` | `http://127.0.0.1:18800` | **引擎侧**读取的服务地址（本服务自身不用） |

红线：`.token`、`deploy/env` 等密钥文件不入 git（gitignore）。

## API 面

`/healthz` 免鉴权；`/api/*` 全部要求 `Authorization: Bearer <token>`（tokenFile
已配置但读失败时 fail-closed 503，B3-2——绝不静默放行）。

| 方法/路径 | 说明 |
|---|---|
| `GET /healthz` | 存活探针（免鉴权） |
| `GET /api/v1/gateway/health` | 网关可达性 |
| `POST /api/v1/sandboxes` | 创建 `{workspace, name?, spec?}`（spec = openshell SandboxSpec JSON，`spec.template.image` 钉沙箱镜像） |
| `GET /api/v1/sandboxes?limit=` | 全工作区清单（limit 默认 500） |
| `GET /api/v1/sandboxes/{name}?workspace=` | 查询单个 |
| `DELETE /api/v1/sandboxes/{name}?workspace=` | 删除 → `{deleted}` |
| `POST /api/v1/sandboxes/{name}/wait-ready` | `{workspace, timeout_seconds?=300}`（服务端上限 600s，超限 400） |
| `POST /api/v1/sandboxes/exec` | 执行命令 `{sandbox_id, command, env?, workdir?, stdin_b64?, timeout_seconds?}`。**sandbox_id 必须传创建响应的 UUID `id` 字段，传沙箱名会 NOT_FOUND**；`command` 必须是字符串列表（裸字符串/混合类型 400，不触达网关）；`stdin_b64` 非法 base64 → 400 |
| `GET /api/v1/sandboxes/{name}/logs?workspace=&lines=&since_ms=` | 日志（lines 默认 2000；`name` 接口层先解析成 UUID 再查——B1-15 审计修复，GetSandboxLogs 只认 UUID） |
| `POST /api/v1/sandboxes/{name}/update-config` | 热更新策略 `{workspace, policy}` |
| `POST /api/v1/sandboxes/{name}/files` | 流式上传，**仅 multipart/form-data**（否则 415）、**必带 Content-Length**（否则 411）：表单字段 `path`（绝对路径必填，父目录自动 `mkdir -p`，含空格/通配符路径安全）、`mode`（八进制可选如 `0755`）+ 文件部分 `file` → `{path,bytes,chunks}`；`?workspace=` 缺省 default。流式转发：边收边按 720 KiB 分块经 exec stdin 写沙箱（网关收包上限实测 1 MiB），内存恒定 <1 MiB，单请求大小受 `maxUploadBytes` 约束（缺省 2 GiB；20 MiB 的 `MAX_BODY_BYTES` 只管 JSON 接口） |
| `POST /api/v1/sandboxes/{name}/services` | ExposeService：沙箱端口暴露为网关服务 `{workspace, service, target_port, domain?=false}` → `{name,sandbox_id,sandbox_name,target_port,domain,url}`；重暴露同名即更新 |
| `GET /api/v1/sandboxes/{name}/services` | 该沙箱暴露服务清单（`?all_workspaces=true` 免 workspace；`limit`/`offset` 分页） |
| `DELETE /api/v1/sandboxes/{name}/services/{service}?workspace=` | 删除暴露；不存在也返回 `{deleted:false}` |
| `GET /api/v1/inference/route?workspace=&route_name=` | 读推理路由（`route_name` 可选：缺省/空串或显式 `inference.local`=用户面默认路由，`sandbox-system`=系统路由；响应含 `route_name/timeout_secs`） |
| `PUT /api/v1/inference/route` | 切路由 `{workspace, provider, model, no_verify?=false, route_name?="", timeout_secs?=0}`，回执含 `validation_performed/validated_endpoints`（网关连通性验证结果透传）与 `route_name/timeout_secs` |
| `DELETE /api/v1/inference/route?workspace=&route_name=` | 删除路由 → `{deleted}`（幂等，缺失=false；`route_name` 合法值同上） |
| `GET /api/v1/workspaces?limit=&offset=` | workspace 清单（`[{name,phase,labels}]`；切片3b team 上车） |
| `POST /api/v1/workspaces` | 建区 `{name, labels?}` → `{name,phase,labels}`；重名 409、名字非法 400（网关 DNS-1123 校验透传） |
| `DELETE /api/v1/workspaces/{name}` | 删区（空区才可删）→ `{name,deleted}`；含资源 409（上游资源守卫 details 透传）、未知 404 |
| `GET /api/v1/inference/providers?workspace=` | provider 清单（对象数组 `[{name,type,config}]`，凭据按省略屏蔽） |
| `GET /api/v1/inference/providers/{name}?workspace=` | 单个 provider |
| `PUT /api/v1/inference/providers` | 创建/更新 `{workspace, name, type, credentials?, config?}` |
| `DELETE /api/v1/inference/providers/{name}?workspace=` | 删除 provider → `{name, deleted}` |

注：无 `GET /api/v1/inference/bundle`——`GetInferenceBundle` 是沙箱主体专享 RPC（manager 的 bearer 身份调用必被网关 PERMISSION_DENIED 拒绝，透传即死面），决策记录见 `docs/api-external.md` §3.22（R32）。

通用约定：

- **寻址双轨**：REST 路径参数一律用沙箱名（接口层内部解析 UUID，）；
  唯 `/exec` 的 `sandbox_id` 走 UUID。
- **错误契约**统一 `{"error": msg}`：400（缺字段/坏 JSON/非对象 body/非法数值参数/
  **字符串字段收非字符串**/布尔字段收布尔或 "true"/"false" 以外的值/env 非"字符串到字符串"映射/command 非字符串列表/
  stdin_b64 非法/spec·policy 未知字段/数值越界（query 整数 0..2³¹-1、
  target_port 1..65535、exec timeout 0..600、wait_ready 0..600 且拒 NaN））、
  401、503（tokenFile 已配置但读失败或内容为空——fail-closed，B3-2/R24）、
  404（含 `no route for METHOD /path`；沙箱不存在 → 404 自 R22 审计修复起
  真实生效）、405（也是 `{"error":…}` 形态）、
  409（workspace 资源冲突：建区重名/删区含资源，切片3b，details 透传）、
  411（上传缺 Content-Length）、413（JSON > 20 MiB、超 `maxUploadBytes`、
  exec 输出累计超 64 MiB——R27）、
  415（上传非 multipart）、
  500（南向非 NOT_FOUND 异常/未捕获兜底，通用文案 `internal error`，细节进
  服务端日志——B3-3）。
  客户端格式错误一律 400，绝不泄漏成 5xx
  （5xx 会被上游按服务端故障重试/降级）——该红线由
  `tests/test_guardrails.py` 结构守门 + `REGRESSIONS.md` R6/R11 档案锁定。
- 上传先写 `.part` 全部落盘后原子改名，失败（含 finalize mv/chmod，R26d）
  自动清理。

## 网关生命周期（兄弟仓）

manager 进程本身是纯传输层；网关容器的部署与生命周期收敛在主仓库
兄弟子模块 `../openshell-gateway/`（原 `CD/openshell-gateway/`，不暴露
HTTP 接口）：`deploy.sh` 差量下发，`gateway_lifecycle.sh`
ensure/verify/status/start/stop/restart；`ensure` 幂等强制服务路由域
`server_sans = ["*.sandbox.codeaudit.internal"]`（沙箱服务 URL 形如
`http://{workspace}--{sandbox}--{service}.sandbox.codeaudit.internal:8080/`，旧默认
域 `openshell.localhost` 仍兜底）。拓扑与纪律见其 `README.md`。

## 引擎侧接入

```bash
# 引擎侧环境变量（生产容器化实例，现役）：
export OPENSHELL_MANAGER_URL=http://192.0.2.122:18800
export OPENSHELL_MANAGER_TOKEN=$(grep -m1 '^OPENSHELL_MANAGER_TOKEN=' deploy/env | cut -d= -f2-)
# 注意不要用 `cut -d= -f2`：token 常见 base64 padding '='，-f2 会从值内
# 第一个 = 截断 → token 残缺全线 401（B1-14 同族教训）
# 宿主机开发态实例（./run.sh）才用 127.0.0.1:18800
```

引擎（Go 化后）对本服务的消费集中在 runtime-service：

- **传输与凭据**：`sandbox.ManagerRunner` 是唯一的 manager 客户端，地址/token
  解析序与本服务 `config.py` 同构（env > 引擎配置 > 共享 config.json > 默认，
  ）；共享 config.json 候选路径含 `../openshell-manager/config.json`
  与按可执行文件相对回溯（`.toolchain/bin/<svc>` 布局下任意 cwd 启动不丢
  token，补遗）。
- **provider/路由管理**：经引擎对外 API 面 `/v1/inference/*`（JWT+admin，
  见 engine `docs/api-external.md` §3.11）——原 `engine/llm_config.py` /
  `scripts/configure_llm.py`（FastAPI 时代工具）已随 Go 化删除。
- **沙箱生命周期**：创建/exec/日志等操作经 ManagerRunner 透传本服务。

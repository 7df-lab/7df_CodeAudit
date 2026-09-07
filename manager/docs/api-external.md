# manager 外部接口契约 — 预期输入 / 预期输出

> 事实源：`openshell_manager/api.py`（路由与校验）+ `tests/test_contract.py`（行为锁定）。
> 本文档由守门测试 `tests/test_guardrails.py::test_readme_api_table_matches_routes`
> 与路由快照共同看护：接口或本文与代码漂移，门禁即红。
> 最后核对：2026-09-07（d662aeb 基线 + 字符串字段 400 收口）。

## 0. 服务定位与调用链

```
引擎(dsh-runtime-service, Go) ─┐
dsh-pentest-sse deploy.sh 冒烟 ─┼─ HTTP/JSON → manager(:18800) → gRPC → openshell-gateway(:8080) → 沙箱
compose healthcheck / 运维 ────┘
```

manager 是**纯传输薄层**：只执行、只观测、绝不裁决；不持业务状态，凭据只透传。

## 1. 真实消费者与所用端点

| 消费者 | 位置 | 所用端点 |
|---|---|---|
| engine dsh-runtime-service | `engine/services/dsh-runtime-service/internal/sandbox/{sandbox,session}.go` | `POST /api/v1/sandboxes`、`POST …/{name}/wait-ready`、`POST …/{name}/services`、`POST /api/v1/sandboxes/exec`、`POST …/{name}/files`、`DELETE /api/v1/sandboxes/{name}` |
| dsh-pentest-sse 镜像冒烟 | `dsh-pentest-sse/deploy.sh` smoke | 创建 → wait-ready → exec → services → delete（全链） |
| compose healthcheck | `deploy/docker-compose.yml` | `GET /healthz` |
| 网关可达性探测（伞仓口径） | `docs/dev-prod-map.md` §2.5 | `GET /api/v1/gateway/health`（8081 health 不可达时的替代探针） |

引擎侧寻址配置：`OPENSHELL_MANAGER_URL` > 引擎自身配置 > 共享 `config.json` 的 `url` 字段
（该字段**只被引擎读**，服务自身不消费，见 `config.py` 注释）；token 同理 `OPENSHELL_MANAGER_TOKEN` 优先。

## 2. 通用约定

- **Base URL**：开发态 `http://127.0.0.1:18800`；生产现役 `http://gateway.internal:18800`。
- **鉴权**：`/healthz` 豁免；`/api/*` 全部要求 `Authorization: Bearer <token>`
  （严格前缀 `Bearer `，大小写敏感；无 token 配置且环回绑定时豁免）。错误 = 401。
- **请求体**：JSON 端点 `Content-Type: application/json`，body 上限 **8 MiB**（`MAX_BODY_BYTES`，
  超限 413）；唯一例外 `POST …/files` 只收 `multipart/form-data` 且**必须带 Content-Length**。
- **错误契约**：所有错误响应统一单键 `{"error": <msg>}`（含 404/405/411/413/415/502）。
  客户端格式错误一律 **400**，绝不泄漏成 502（502 会被上游按"网关不可达"重试/降级）。
- **寻址双轨（ADR-173）**：REST 路径参数一律沙箱**名**（接口层内部解析 UUID）；
  唯 `/exec` 的 `sandbox_id` 走创建响应返回的 UUID **`id` 字段**——传沙箱名会 404。

### 错误码总表

| 码 | 触发条件 | error 文案样例 |
|---|---|---|
| 400 | 缺必填字段 / 坏 JSON / 非对象 body / 字段类型不符（字符串字段收非字符串、command 非字符串列表、数值字段非数值）/ stdin_b64 非法 / spec·policy 未知字段 / path 非绝对 / mode 非 3-4 位八进制 / multipart 结构坏 / 查询参数数值非法 | `missing required field(s): workspace` |
| 401 | Bearer 缺失/不匹配 | `unauthorized (bearer token required)` |
| 404 | 未知路由（`no route for METHOD /path`）/ 沙箱或 provider 不存在 | `no route for GET /api/v1/nope` |
| 405 | 路径存在但方法不匹配（Starlette detail 透传，仍 `{"error":…}` 形态） | `Method Not Allowed` |
| 411 | 上传缺 Content-Length | `Content-Length required for file upload` |
| 413 | JSON body > 8 MiB；上传超 `maxUploadBytes` | `body too large (N bytes)` |
| 415 | 上传端点非 multipart | `content-type must be multipart/form-data …` |
| 502 | 南向 SDK/gRPC 异常、未捕获兜底 | `RuntimeError: gateway unreachable` |

> 传输层注：非法 `Content-Length` 头（非数字）由 uvicorn/h11 在解析层直接 400，
> 不会进入本服务代码——契约上等同于 400。

## 3. 端点明细

### 3.1 `GET /healthz` — 存活探针（免鉴权）

- 输入：无头无体。
- 输出 `200`：`{"ok": true}`（常量，不触达网关）。
- 其他方法（如 `DELETE /healthz`）→ 405。

### 3.2 `GET /api/v1/gateway/health` — 网关可达性

- 输入：无参数。SDK `client.health()` 非空即视为可达。
- 输出 `200`：`{"ok": true, "endpoint": "gateway.internal:8080"}`；
  SDK 返回 None → `{"ok": false, "endpoint": …}`（仍 200，网关在线但应答异常）。
- 错误：SDK 异常（连接拒绝等）→ 502 `{"error": "<ExcType>: …"}`。

### 3.3 `POST /api/v1/sandboxes` — 创建沙箱

- 输入 body：`{"workspace": str(必填), "name": str(可选,缺省""),
  "spec": SandboxSpec-JSON(可选,缺省{})}`。
  `spec` 是 openshell `SandboxSpec` proto 的 JSON 投影；钉沙箱镜像用
  `spec.template.image`。**所有字符串字段必须为 JSON string**（数字/布尔 → 400）。
- 流程：spec 经 `ParseDict` 严格解析（`ignore_unknown_fields=False`，未知字段 400）。
- 输出 `200`：沙箱引用投影
  `{"id": UUID, "name": str, "workspace": str, "phase": int, "phase_name": "SANDBOX_PHASE_*",
    "current_policy_version": int, "labels": {str:str}, "conditions": [dict,…]}`
  （`conditions` 原样保留网关诊断，`gateway_probe.py` 依赖它）。
- 错误：缺 workspace 400；spec 未知字段 400 `invalid spec: …`；南向异常 502。

### 3.4 `GET /api/v1/sandboxes?limit=<int>` — 全工作区清单

- 输入：`limit` 可选整数，默认 500；非整数 → 400。
- 输出 `200`：`{"sandboxes": [<沙箱引用投影>…]}`（跨全部 workspace）。

### 3.5 `GET /api/v1/sandboxes/{name}?workspace=<str>` — 查询单个

- 输入：路径沙箱名；`workspace` 必填查询参数（缺失/空 → 400）。
- 输出 `200`：沙箱引用投影（同 3.3）。沙箱不存在 → 404 `sandbox '…' not found`。

### 3.6 `DELETE /api/v1/sandboxes/{name}?workspace=<str>` — 删除

- 输入：同 3.5。输出 `200`：`{"deleted": true|false}`（网关返回 falsy 也如实透出 `false`）。

### 3.7 `POST /api/v1/sandboxes/{name}/wait-ready` — 等待就绪

- 输入 body：`{"workspace": str(必填), "timeout_seconds": number(可选,默认300)}`。
  `timeout_seconds` 非数值（含数字字符串）→ 400。
- 输出 `200`：就绪后的沙箱引用投影；超时/不存在 → 南向异常 502 / 404。

### 3.8 `POST /api/v1/sandboxes/exec` — 沙箱内执行（唯一 UUID 寻址端点）

- 输入 body：
  - `sandbox_id`：str 必填，**创建响应的 `id` UUID**；
  - `command`：**字符串列表**必填（裸字符串 / 混合类型 → 400 且**不触达网关**——
    历史缺陷：裸字符串曾被 `list()` 拆成单字符数组静默执行垃圾命令）；
  - `env`：可选 `{str: str}` 映射（非映射 → 400）；
  - `workdir`：可选 str（非 str → 400）；
  - `stdin_b64`：可选 base64 文本（非法 → 400 且不触达网关）；
  - `timeout_seconds`：可选整数（非数值 → 400）。
- 输出 `200`：`{"exit_code": int, "stdout": str, "stderr": str}`
  （bytes 经 UTF-8 替换解码；非 UTF-8 字节不会报错）。

### 3.9 `GET /api/v1/sandboxes/{name}/logs?workspace=&lines=&since_ms=` — 日志

- 输入：`workspace` 必填；`lines` 可选 int 默认 2000；`since_ms` 可选 int 默认 0；
  非整数 → 400。`{name}` 为沙箱名（接口层透传给 `GetSandboxLogsRequest.sandbox_id`，
  网关按名解析——与 /exec 的 UUID 口径不同，**这是网关侧语义**）。
- 输出 `200`：`{"logs": [ {…MessageToDict 投影}… ]}`。

### 3.10 `POST /api/v1/sandboxes/{name}/update-config` — 热更新策略

- 输入 body：`{"workspace": str(必填), "policy": SandboxPolicy-JSON(必填)}`。
  policy 未知字段 → 400 `invalid policy: …`。
- 输出 `200`：`{"version": int, "policy_hash": str}`。

### 3.11 `POST /api/v1/sandboxes/{name}/files?workspace=` — 流式上传

- 输入（**仅 multipart/form-data**，否则 415；**必须带 Content-Length**，否则 411）：
  - 表单字段 `path`（必填，**绝对路径**，父目录自动 `mkdir -p`；含空格/通配符安全）；
  - 表单字段 `mode`（可选，3-4 位八进制如 `0644`，否则 400）；
  - 文件部分名必须为 `file`（缺失 → 400）。
  - `?workspace=` 可选，缺省 `default`；`{name}` 为沙箱名，接口层解析 UUID。
- 语义：边收边落盘（spool，有界内存 <1 MiB）→ 解析 → 按 720 KiB（3 字节对齐）
  分块 base64 经 exec stdin 写入沙箱 `.part` 文件 → 全部落盘后原子 `mv` → 可选 `chmod`。
  单请求大小不限（仅受 `maxUploadBytes` 策略约束，超限 413）。
- 输出 `200`：`{"path": str, "bytes": int, "chunks": int}`。
- 失败语义：任一分块失败即清理 `.part` 并保持目标路径不动（404=沙箱不存在，
  502=写盘失败 `upload … failed with exit code …`）。

### 3.12 `POST /api/v1/sandboxes/{name}/services` — 暴露服务（ExposeService）

- 输入 body：`{"workspace": str(必填), "service": str(必填), "target_port": int(必填),
  "domain": bool(可选,默认false)}`；`target_port` 非整数 → 400。
- 语义：把沙箱端口暴露为网关服务；**重暴露同名即原位更新**（不重复）。
- 输出 `200`：`{"name": str, "sandbox_id": str, "sandbox_name": str,
  "target_port": int, "domain": bool, "url": "http://{ws}--{sbx}--{svc}.openshell.internal:8080/"}`。

### 3.13 `GET /api/v1/sandboxes/{name}/services` — 暴露服务清单

- 输入：`?workspace=<str>`（默认必填）**或** `?all_workspaces=true`（`1/true/yes`
  大小写不敏感，免 workspace）；`limit`（默认 100）/`offset`（默认 0）可选 int 分页。
- 输出 `200`：`{"services": [<3.12 投影>…]}`。

### 3.14 `DELETE /api/v1/sandboxes/{name}/services/{service}?workspace=` — 删除暴露

- 输入：`workspace` 必填。输出 `200`：`{"deleted": true|false}`
  （**不存在也 200 + `false`**，幂等删除）。

### 3.15 `GET /api/v1/inference/route?workspace=` — 读推理路由

- 输入：`workspace` 必填。输出 `200`：`{"provider": str, "model": str, "version": int}`。

### 3.16 `PUT /api/v1/inference/route` — 切换路由

- 输入 body：`{"workspace": str(必填), "provider": str(必填), "model": str(必填),
  "no_verify": bool(可选,默认false)}`。
- 输出 `200`：`{"provider": str, "model": str, "version": int,
  "validation_performed": bool, "validated_endpoints": [{"url": str,
  "protocol": str}]}`。后两个字段透传网关 `SetInferenceRoute` 回执
  （连通性验证结果；`no_verify:true` 时 `validation_performed:false`、列表空）。
  直连 inference stub 而非 SDK `InferenceRouteClient`，因 SDK 客户端丢弃这两字段。

### 3.17 `GET /api/v1/inference/providers?workspace=` — provider 清单

- 输入：`workspace` 必填。输出 `200`：`{"providers": [{"name": str,
  "type": str, "config": {str:str}}]}`（对象数组；**凭据按省略屏蔽**，
  与 3.18 同一脱敏纪律）。

### 3.18 `GET /api/v1/inference/providers/{name}?workspace=` — 单个 provider

- 输出 `200`：`{"name": str, "type": str, "config": {str:str}}`。
  **凭据按省略屏蔽**——响应永远没有 `credentials` 键（秘密只在网关加密存储）。
- 不存在 → 404 `provider '…' not found in workspace '…'`。

### 3.19 `PUT /api/v1/inference/providers` — 创建/更新 provider

- 输入 body：`{"workspace": str(必填), "name": str(必填), "type": str(必填),
  "credentials": {str:str}(可选,缺省{}), "config": {str:str}(可选,缺省{})}`。
- 语义：名字已存在 → Update（`created:false`）；不存在 → Create（`created:true`）。
- 输出 `200`：`{"name": str, "created": bool}`。

### 3.20 `DELETE /api/v1/inference/providers/{name}?workspace=` — 删除 provider

- 输入：`workspace` 必填。网关 `DeleteProvider` 直通。
- 输出 `200`：`{"name": str, "deleted": bool}`（不存在 → `deleted:false`）。

## 4. 部署面接口（非 HTTP，外部可见）

| 接口 | 输入 | 输出/语义 |
|---|---|---|
| `deploy/deploy.sh {deploy\|check\|status\|logs [N]\|start\|stop\|restart}` | 子命令；token 来自 `deploy/env`（600，gitignore，单一事实源） | deploy = 同步源码+配方 → compose build+up → healthz → 网关可达性；check = 漂移检查（源码/产物/.env） |
| 环境变量族 | `OPENSHELL_MANAGER_BIND/_PORT/_TOKEN`、`OPENSHELL_GATEWAY_ENDPOINT`、`OPENSHELL_LIB_PATH`、`OPENSHELL_MANAGER_MAX_UPLOAD_BYTES`、`OPENSHELL_MANAGER_CONFIG` | 见 `docs/data-flows.md` §1 配置解析链 |
| 共享 `config.json` | `url`/`bind`/`port`/`tokenFile`/`gatewayEndpoint`/`libPath` | 与引擎共享 SSOT；`url` 仅引擎侧读取 |

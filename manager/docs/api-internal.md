# manager 内部接口契约 — 模块间预期输入 / 预期输出

> 事实源：`openshell_manager/{api,gateway,upload,config}.py`。
> 层次：`__main__` → `api.serve()` → FastAPI app（HTTP 层）→ `GatewayFacade`（南向门面）
> → openshell SDK；`upload.py`（multipart 解析器）与
> `config.py`（配置解析）为 api/gateway 共同依赖。依赖方向严格单向，禁止反向 import。

```
__main__.py ──> api.serve() ──> uvicorn ──> create_app() 的路由
                                  │
              ┌───────────────────┼──────────────────────┐
              ▼                   ▼                      ▼
         config.py          upload.py            gateway.GatewayFacade
        (配置/鉴权值)     (multipart 解析)             │ (client_factory 缝)
                                          demanded lazily ▼
                                              vendored openshell SDK (gRPC)
```

## 1. HTTP 层（api.py）→ GatewayFacade 方法契约

facade 无状态、懒加载单例（`api.facade` 模块级；测试经 `client_factory` 注入假 SDK）。
HTTP 层保证：调进 facade 的参数**已经过类型校验**；facade 抛出的异常按 §4 映射。
全部 async def 端点的南向调用一律 `await run_in_threadpool(facade.xxx, …)`
（B3-1 审计修复：同步 gRPC 裸跑在 event loop 上会阻塞 /healthz 探活与全部并发
请求；对齐上传路径既有形态）。async 端点集以 api.py 当次 `async def` 路由注册
为准，不写死计数（旧文"七个"两度失实：漏 files 上传、workspaces 建区）。

| facade 方法 | 输入（HTTP 层校验后保证） | 输出 dict 形态 | 可能异常 |
|---|---|---|---|
| `health()` | 无 | `{"ok": bool, "endpoint": str}`（SDK health() 为 None → ok=False） | SDK 异常直抛（→500） |
| `create(workspace=, name=, spec=)` | workspace/name: str；spec: dict | 沙箱引用投影（§1.1） | ValueError（ParseDict 失败，api 层转 400） |
| `get(name=, workspace=)` | str ×2 | 沙箱引用投影 | LookupError（不存在 → 404） |
| `list_all(limit=)` | int | `[沙箱引用投影…]` | SDK 异常 |
| `wait_ready(name=, workspace=, timeout_seconds=)` | str,str,float | 沙箱引用投影 | LookupError / SDK 超时 |
| `exec(sandbox_id=, command=, workdir=, environment=, stdin=, timeout_seconds=)` | str, List[str], str\|None, Dict\|None, bytes\|None, int\|None | `{"exit_code": int, "stdout": str, "stderr": str}` | SDK 异常 |
| `delete(name=, workspace=)` | str,str | bool | SDK 异常 |
| `resolve_sandbox_id(name=, workspace=)` | str,str | UUID str | LookupError（→ 上传 404） |
| `write_file_stream(sandbox_id=, path=, chunks=, mode=)` | str, str(绝对路径), Iterator[bytes], str\|None | `{"path","bytes","chunks"}` | ValueError（非绝对路径）；RuntimeError（远端命令失败；触发 .part 自清理） |
| `get_logs(sandbox_id=, workspace=, lines=, since_ms=)` | str,str,int,int | `[log dict…]` | SDK 异常 |
| `update_config(name=, workspace=, policy=)` | str,str,dict | `{"version": int, "policy_hash": str}` | ValueError（ParseDict） |
| `expose_service(sandbox=, service=, target_port=, workspace=, domain=)` | str×4, int | 服务投影（§1.2） | SDK 异常 |
| `list_services(sandbox=, workspace=, limit=, offset=, all_workspaces=)` | str,str(可空),int,int,bool | `[服务投影…]` | SDK 异常 |
| `delete_service(sandbox=, service=, workspace=)` | str×3 | `{"deleted": bool}` | SDK 异常 |
| `get_route(workspace=, route_name=)` | str×2 | `{"provider","model","version","route_name","timeout_secs"}` | LookupError（未配置 → 404，R30）/ InferenceAdminRejected；SDK 异常 |
| `set_route(workspace=, provider=, model=, no_verify=, route_name=, timeout_secs=)` | str×3, bool, str, int | 同上 + `{"validation_performed","validated_endpoints"}` | InferenceVerificationFailed（→400，R30）/ InferenceAdminRejected/ SDK 异常 |
| `delete_route(workspace=, route_name=)` | str×2 | `{"deleted": bool}`（缺失=false，幂等；源码同证无 NOT_FOUND 路径） | InferenceAdminRejected/ SDK 异常 |
| `list_providers(workspace=)` | str | `[{"name","type","config"}…]`（凭据按省略屏蔽） | SDK 异常 |
| `get_provider(workspace=, name=)` | str×2 | 同上单条（无 credentials 键）；实现=**进程内过滤 list_providers 结果**，无独立南向 RPC | LookupError（→404） |
| `upsert_provider(workspace=, name=, type_=, credentials=, conf=)` | str×3, dict×2 | `{"name": str, "created": bool}` | SDK 异常 |
| `delete_provider(workspace=, name=)` | str×2 | `{"name": str, "deleted": bool}` | SDK 异常 |
| `list_workspaces(limit=, offset=)` | int,int | `{"workspaces": [{"name","phase","labels"}…]}` | SDK 异常 |
| `create_workspace(name=, labels=)` | str, Dict[str,str]\|None | `{"name","phase","labels"}` | InferenceAdminRejected（→400，名字非法——DNS-1123 校验在网关透传）/ StateConflict（→409，重名）/ SDK 异常 |
| `delete_workspace(name=)` | str | `{"name": str, "deleted": bool}` | LookupError（→404，未知区）/ StateConflict（→409，区内含资源——上游资源守卫 details 透传）/ SDK 异常 |

（workspace 三方法为切片3b team 上车新增；manager 纯透传不执法。）

**inference 面 `route_name` 值域**（get/set/delete 同）：缺省/空串与显式
`inference.local` 等价（=用户面默认路由；前后空白被网关 trim）、
`sandbox-system`=系统路由；其余名网关 INVALID_ARGUMENT→400
透传（源码 d51a653f `inference.rs` `effective_route_name`）。
`GetInferenceBundle` 不透传——沙箱主体专享 RPC（决策记录 api-external §3.22）。

**1.1 沙箱引用投影**（`_ref()`，dict 边界——protobuf 只存在于 facade 南向侧）：
`{"id": str, "name": str, "workspace": str, "phase": int, "phase_name": str`,
`"current_policy_version": int, "labels": dict, "conditions": [dict…]}`。
`phase_name` = 枚举名（未知值回落 `str(数值)`）；`conditions` 原样保留（R34/P2-4：vendored SDK SandboxStatusRef 补 conditions 字段——此前 dataclass 边界丢弃致投影恒空；旧文引用的 gateway_probe 消费者不存在，已除名）。

**1.2 服务投影**（`_service_projection()`）：
`{"name": ep.service_name, "sandbox_id": ep.sandbox_id, "sandbox_name": ep.sandbox_name,
"target_port": ep.target_port, "domain": bool, "url": resp.url}`。

## 2. GatewayFacade → openshell SDK 契约

- **直接导入**：SDK 是服务根顶层包，模块顶部
  `from openshell import SandboxClient` / `from openshell._proto import …`
  直 import——缺席 = 仓坏，启动期 ImportError（原 sys.path 注入 +
  "无 SDK 也可导入"降级语义已随解绑删除）。
- **客户端**：`_default_client_factory()` → `openshell.SandboxClient(endpoint, timeout=60)`；
  `client_factory` 构造参数是测试缝（假 SDK 注入）。
- **dict↔proto 边界**：
  - dict→proto 走 `_parse_dict()`：`ParseDict(js, msg, ignore_unknown_fields=False)`，
    任何 ParseError **统一转 ValueError**（新 protobuf 的 ParseError 已非 ValueError
    子类，必须显式转）→ HTTP 层 400。
  - proto→dict 走 `MessageToDict(…, preserving_proto_field_name=True)`（snake_case 保持）。
- **绕过 SDK 高层、直用 stub 的操作**（SDK 无对应封装）：
  `get_logs`/`update_config` 经 `client._stub`；`expose_service`/`list_services`/
  `delete_service`/`list_providers`/`upsert_provider`/
  `delete_provider` 经 `pb_grpc_stub(client)`（`OpenShellStub(client._channel)`）。
  依赖 SDK 私有属性 `_stub`/`_channel`——SDK 升级时是断点，契约测试兜底。
  `get_provider` **不在列**——进程内过滤 `list_providers` 结果（§1），无独立南向调用。
  workspace 族走 SDK 高层 `WorkspaceClient.from_sandbox_client`（`_workspace_client`
  缝，测试替身注入同 `_inference_stub` 形态，切片3b）。
- **InferenceRoute**：`_inference_stub()` → `InferenceStub(sdk._channel)` 直连

  以 `test_inference_stub_projection_covers_proto_fields` 当次输出为准）：
  `GetInferenceRoute`/`SetInferenceRoute`/`DeleteInferenceRoute` 三 RPC；
  `GetInferenceBundle` 有意不透传——沙箱主体专享（R32）。不走 SDK
  `InferenceRouteClient`——其 `set_route` 丢弃回执的
  `validation_performed/validated_endpoints`（3.16 对外契约要求透传），
  `InferenceRouteConfig` 又只投影 provider/model/version（丢
  `route_name/timeout_secs`），delete 无封装。字段对齐锁 =
  `test_inference_stub_projection_covers_proto_fields`（proto 增字段即红，
  弃置须给理由：回执 `workspace` 回环不投影；bundle 整族不透传——R32；
  请求侧 `verify` 不暴露——SDK 客户端同样不发，verify/no_verify 组合
  语义无实证，网关闭源行为不猜）。
- **provider upsert 语义**：先 `ListProviders` 探存在性 → 存在 `UpdateProvider` /
  不存在 `CreateProvider` → 返回 `created = not exists`。
- **provider 读取屏蔽**：`list_providers`/`get_provider` 只投影 name/type/config，
  凭据永不南向回流。

### 上传的南向协议（write_file_stream，详见 data-flows.md §3）

对沙箱内 `/bin/sh -c` 依次下发：
1. `mkdir -p "$(dirname <shlex.quote(path)>)"`（**先于**首个分块；引号防词拆分/glob）；
2. 每块一条 `base64 -d >|>> <quoted>.part`，stdin = 该块 base64 文本
   （块大小 `UPLOAD_CHUNK_BYTES=720KiB`，先 3 字节对齐再编码——非对齐块各自带
   padding 会截断流式解码；编码后 960KiB < 网关 gRPC 实测 1MiB 收包上限）；
3. `mv <quoted>.part <quoted>`（全部落盘后原子改名）；
4. `chmod <mode> <path>`（可选）。
任一步非零退出 → RuntimeError → `rm -f <path>.part` 尽力清理 → 异常上抛。

## 3. HTTP 层（api.py）→ upload.py 契约

- `boundary_from_content_type(content_type: str) -> bytes`：提取 boundary 参数
  （去引号，latin-1 编码）；缺失 → `UploadError`（api 层转 400 invalid multipart body）。
- `StreamingMultipartParser(stream: BinaryIO, boundary: bytes,
  max_field_bytes=64KiB).parse() -> (fields: dict[bytes,bytes], file_stream: Iterator[bytes])`：
  - 只认 `name="file"` 且带 filename 的部分为文件；文本字段以 bytes 键入 fields；
  - 文件流惰性产出，耗尽即校验收尾边界；提前放弃 = 请求体未读完（连接将被关闭）；
  - 跨读块边界的分隔符靠 `len(delimiter)-1` 回看尾处理；payload 内疑似 boundary
    字节串（后随非 `--`/非 CRLF）作为载荷放行；
  - 文本字段超 64 KiB → `UploadError`；结构坏 → `UploadError`（均 → 400）。
- api 层职责：415（非 multipart）/411（无 Content-Length）/413（超
  `max_upload_bytes`）前置判断；body spool 到 `SpooledTemporaryFile`（>1MiB 落盘）；
  `path` 绝对性、`mode` 八进制校验在解析之后；解析 + 写盘全程 `run_in_threadpool`。

## 4. 异常 → HTTP 映射总表（api.py 异常处理器）

| 异常 | 处理器 | HTTP | body |
|---|---|---|---|
| `ApiError(status, msg)` | `_api_error` | status | `{"error": msg}` |
| `LookupError`（含其子类） | `_lookup` | 404 | `{"error": str(exc)}` |
| `InferenceVerificationFailed` | `_inference_verification` | 400 | `{"error": str(exc)}`（set_route 南向 FAILED_PRECONDITION=endpoint 实核失败，客户端可修正——5xx 会被上游按服务端故障误重试/降级） |
| `InferenceAdminRejected` | `_inference_admin_rejected` | 400 | `{"error": str(exc)}` |
| `StateConflict` | `_state_conflict` | 409 | `{"error": str(exc)}`（切片3b：南向 ALREADY_EXISTS/FAILED_PRECONDITION=workspace 重名/删区含资源，details 透传） |
| `StarletteHTTPException` 404 | `_http_exc` | 404 | `{"error": "no route for METHOD /path"}`（尾斜杠 rstrip） |
| `StarletteHTTPException` 其余（如 405） | `_http_exc` | 原码 | `{"error": detail}` |
| 其他一切 `Exception` | `_unhandled` 兜底 | 500 | `{"error": "internal error"}`（`ExcType: msg` 进服务端 stderr 日志——B3-3：原 502+细节泄漏既触发上游"网关不可达"误判又暴露内部信息） |

api 层主动转换：`ValueError`（ParseDict/绝对路径）→ 400；`UploadError` → 400；
`json_format.ParseError` → 400；校验失败 → `ApiError(400, …)` 且**绝不触达 facade**。

## 5. api.py ↔ config.py 契约（api/gateway 消费的配置值）

| 函数 | 返回 | 消费点 | 失败行为 |
|---|---|---|---|
| `manager_token()` | str（空=免鉴权） | `require_token`、`serve()` 提示语 | tokenFile **不存在**=未配置 → 空（放行维持现状）；**存在但读失败**（EACCES/EIO 等 OSError）→ `TokenFileError`（require_token 503 fail-closed + stderr 日志，B3-2；修复前吞异常当空=读失败瞬间鉴权失效）。文件解析结果 5s 缓存（`_token_cache`；env 分支不缓存直读）——require_token 是 async 依赖，防每请求阻塞磁盘 IO；异常不落缓存，权限恢复即自愈 |
| `manager_bind()` / `manager_port()` | str / int | `serve()` → uvicorn | port 坏值回落 18800 |
| `gateway_endpoint()` | str | `GatewayFacade._default_client_factory`、health 投影 | 有内置默认，不失败 |
| `max_upload_bytes()` | int（缺省 2 GiB；0=不限） | `_handle_upload` 413 判断 | 坏值回落 2 GiB（B3-3：原缺省/回落 0 让防误操作上限形同虚设） |
| `validate()` | None / raise | `serve()` 启动前 | 非环回 bind 且无 token → RuntimeError 拒启 |

解析优先级一律 **env > config.json > 内置默认**；config.json 全局缓存
（`_config_cache`，进程内只读一次；坏 JSON 降级 `{}` 不致命——env-only 使用必须存活）。
测试隔离手段：`OPENSHELL_MANAGER_CONFIG` 指向空 JSON + `config._config_cache = None`。

## 6. 测试注入缝汇总（内部接口的正式组成部分）

| 缝 | 位置 | 用途 |
|---|---|---|
| `GatewayFacade(client_factory=…)` | gateway.py | 假 SDK 客户端（离线契约测试根基） |
| `facade._inference_stub` | gateway.py | 整体替换 inference 直连 stub（契约测试用） |
| `facade._workspace_client` | gateway.py | 整体替换 workspace 客户端（切片3b，`_inference_stub` 同型） |
| `gw.pb_grpc_stub` | gateway.py 模块函数 | 假 admin stub（services/providers） |
| `api.facade` | api.py 模块级单例 | app 工厂绑定被测 facade |
| `OPENSHELL_MANAGER_CONFIG` + `_config_cache=None` + `_token_cache=None` | config.py | 配置/ token 缓存隔离 |
| `GatewayFacade.UPLOAD_CHUNK_BYTES` | gateway.py 类属性 | 缩小分块触发多块路径 |

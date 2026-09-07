# manager 内部接口契约 — 模块间预期输入 / 预期输出

> 事实源：`openshell_manager/{api,gateway,upload,config}.py`。
> 层次：`__main__` → `api.serve()` → FastAPI app（HTTP 层）→ `GatewayFacade`（南向门面）
> → vendored SDK（`libs/OpenShell/python`）；`upload.py`（multipart 解析器）与
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

| facade 方法 | 输入（HTTP 层校验后保证） | 输出 dict 形态 | 可能异常 |
|---|---|---|---|
| `health()` | 无 | `{"ok": bool, "endpoint": str}`（SDK health() 为 None → ok=False） | SDK 异常直抛（→502） |
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
| `get_route(workspace=)` | str | `{"provider","model","version"}` | SDK 异常 |
| `set_route(workspace=, provider=, model=, no_verify=)` | str×3, bool | 同上 | SDK 异常 |
| `list_providers(workspace=)` | str | `[name…]` | SDK 异常 |
| `get_provider(workspace=, name=)` | str×2 | `{"name","type","config"}`（无 credentials 键） | LookupError（→404） |
| `upsert_provider(workspace=, name=, type_=, credentials=, conf=)` | str×3, dict×2 | `{"name": str, "created": bool}` | SDK 异常 |

**1.1 沙箱引用投影**（`_ref()`，dict 边界——protobuf 只存在于 facade 南向侧）：
`{"id": str, "name": str, "workspace": str, "phase": int, "phase_name": str`,
`"current_policy_version": int, "labels": dict, "conditions": [dict…]}`。
`phase_name` = 枚举名（未知值回落 `str(数值)`）；`conditions` 原样保留（gateway_probe 依赖）。

**1.2 服务投影**（`_service_projection()`）：
`{"name": ep.service_name, "sandbox_id": ep.sandbox_id, "sandbox_name": ep.sandbox_name,
"target_port": ep.target_port, "domain": bool, "url": resp.url}`。

## 2. GatewayFacade → vendored SDK 契约

- **懒加载**：`_ensure_sdk_path()` 把 `config.openshell_lib_path()` 插到 `sys.path[0]`
  恰好一次（`_sdk_path_done` 门闩）；所有 protobuf/SDK import 点都先调它——
  SDK 缺失时 HTTP 层仍可自描述（import 不炸）。
- **客户端**：`_default_client_factory()` → `openshell.SandboxClient(endpoint, timeout=60)`；
  `client_factory` 构造参数是测试缝（假 SDK 注入）。
- **dict↔proto 边界**：
  - dict→proto 走 `_parse_dict()`：`ParseDict(js, msg, ignore_unknown_fields=False)`，
    任何 ParseError **统一转 ValueError**（新 protobuf 的 ParseError 已非 ValueError
    子类，必须显式转）→ HTTP 层 400。
  - proto→dict 走 `MessageToDict(…, preserving_proto_field_name=True)`（snake_case 保持）。
- **绕过 SDK 高层、直用 stub 的操作**（SDK 无对应封装）：
  `get_logs`/`update_config` 经 `client._stub`；`expose_service`/`list_services`/
  `delete_service`/`list_providers`/`get_provider`/`upsert_provider`/
  `delete_provider` 经 `pb_grpc_stub(client)`（`OpenShellStub(client._channel)`）。
  依赖 SDK 私有属性 `_stub`/`_channel`——SDK 升级时是断点，契约测试兜底。
- **InferenceRoute**：`_inference_stub()` → `InferenceStub(sdk._channel)` 直连
  （也是测试缝：`facade._inference_stub` 可整体替换）。不走 SDK
  `InferenceRouteClient`——其 `set_route` 丢弃回执的
  `validation_performed/validated_endpoints`（3.16 对外契约要求透传）。
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
| `StarletteHTTPException` 404 | `_http_exc` | 404 | `{"error": "no route for METHOD /path"}`（尾斜杠 rstrip） |
| `StarletteHTTPException` 其余（如 405） | `_http_exc` | 原码 | `{"error": detail}` |
| 其他一切 `Exception` | `_unhandled` 兜底 | 502 | `{"error": "<ExcType>: <msg>"}` |

api 层主动转换：`ValueError`（ParseDict/绝对路径）→ 400；`UploadError` → 400；
`json_format.ParseError` → 400；校验失败 → `ApiError(400, …)` 且**绝不触达 facade**。

## 5. api.py ↔ config.py 契约（api/gateway 消费的配置值）

| 函数 | 返回 | 消费点 | 失败行为 |
|---|---|---|---|
| `manager_token()` | str（空=免鉴权） | `require_token`、`serve()` 提示语 | tokenFile 读失败视为空 |
| `manager_bind()` / `manager_port()` | str / int | `serve()` → uvicorn | port 坏值回落 18800 |
| `gateway_endpoint()` | str | `GatewayFacade._default_client_factory`、health 投影 | 有内置默认，不失败 |
| `max_upload_bytes()` | int（0=不限） | `_handle_upload` 413 判断 | 坏值回落 0 |
| `openshell_lib_path()` | Path | `_ensure_sdk_path` | 找不到 SDK 目录 → RuntimeError（fail-loud） |
| `validate()` | None / raise | `serve()` 启动前 | 非环回 bind 且无 token → RuntimeError 拒启 |

解析优先级一律 **env > config.json > 内置默认**；config.json 全局缓存
（`_config_cache`，进程内只读一次；坏 JSON 降级 `{}` 不致命——env-only 使用必须存活）。
测试隔离手段：`OPENSHELL_MANAGER_CONFIG` 指向空 JSON + `config._config_cache = None`。

## 6. 测试注入缝汇总（内部接口的正式组成部分）

| 缝 | 位置 | 用途 |
|---|---|---|
| `GatewayFacade(client_factory=…)` | gateway.py | 假 SDK 客户端（离线契约测试根基） |
| `facade._route_client` | gateway.py | 替换 InferenceRouteClient |
| `gw.pb_grpc_stub` | gateway.py 模块函数 | 假 admin stub（services/providers） |
| `api.facade` | api.py 模块级单例 | app 工厂绑定被测 facade |
| `OPENSHELL_MANAGER_CONFIG` + `_config_cache=None` | config.py | 配置隔离 |
| `GatewayFacade.UPLOAD_CHUNK_BYTES` | gateway.py 类属性 | 缩小分块触发多块路径 |

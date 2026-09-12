"""OpenShell manager HTTP surface — FastAPI 架构（ADR-174，人类指令 2026-09-01）。

原 stdlib http.server 实现（旧 http_api.py，2026-09-08 死代码清理中随
人类指令"消除重复接口实现"退役，git 历史可考）整体迁移 FastAPI/uvicorn：
  - 路由声明式注册（替代 regex ROUTES 表 + 手写 dispatch）；
  - 鉴权收敛为依赖注入（/healthz 豁免，其余 Bearer token）；
  - 异常处理器统一错误契约 {"error": msg}（ApiError/LookupError/404 no route/
    兜底 500 "internal error"——B3-3：细节进服务端 stderr，且 502 会让上游按
    "网关不可达"误重试/降级），
    JSON 端点手工解包 body 保持既有错误语义（"invalid JSON body"/413/非对象 400）；
  - async 端点的南向调用一律 run_in_threadpool（B3-1：同步 gRPC 调用裸跑在
    event loop 上会阻塞探活与全部并发请求，对齐 _handle_upload 既有形态）；
  - /files 流式上传：原始 body spool 到磁盘（有界内存）后沿用 upload.py 流式解析器，
    接口层收沙箱 name（+?workspace=，缺省 default）内部自解析 UUID（ADR-173）。

南向 gRPC（gateway.py）与对外 JSON 契约逐字节不变（tests/test_contract.py 锁定）。
"""
from __future__ import annotations

import base64
import hmac
import json
import re
import sys
import tempfile
from typing import Any, Dict, Iterator, Optional
from urllib.parse import parse_qs

from fastapi import Depends, FastAPI, Request
from fastapi.responses import JSONResponse
from starlette.concurrency import run_in_threadpool
from starlette.exceptions import HTTPException as StarletteHTTPException

from . import config
from .gateway import GatewayFacade
from .upload import StreamingMultipartParser, UploadError, boundary_from_content_type

MAX_BODY_BYTES = 20 * 1024 * 1024
# wait_ready 服务端超时上限（B3-1）：缺省 300s，硬上限 600s——客户端曾可传
# 1e9 让南向连接无限占用；超过上限 = 客户端错误 400。
WAIT_READY_TIMEOUT_DEFAULT = 300.0
WAIT_READY_TIMEOUT_MAX = 600.0

facade = GatewayFacade()


class ApiError(Exception):
    """与旧实现同形：status + 面向客户端的 message（{"error": ...}）。"""

    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


# ---------------------------------------------------------------------------
# 契约保持：body/query 解析与字段校验（错误文案逐字对齐旧实现）
# ---------------------------------------------------------------------------

def _need(body: Dict[str, Any], *keys: str) -> None:
    missing = [k for k in keys if not body.get(k)]
    if missing:
        raise ApiError(400, f"missing required field(s): {', '.join(missing)}")


def _need_str(body: Dict[str, Any], *keys: str) -> None:
    """_need 的强类型版：必填且必须是非空字符串。非字符串曾透传进 SDK/proto
    层炸成 502（proto 对 str 字段传非 str 抛 TypeError），而 502 会被上游按
    "网关不可达"重试/降级——客户端格式错误一律 400。"""
    _need(body, *keys)
    bad = [k for k in keys if not isinstance(body[k], str)]
    if bad:
        raise ApiError(400, f"field(s) {', '.join(bad)} must be string(s)")


def _opt_str(body: Dict[str, Any], key: str) -> Optional[str]:
    """可选字符串字段：缺失/None → None；存在但非字符串 → 400。"""
    value = body.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise ApiError(400, f"field {key} must be a string")
    return value


async def _json_body(request: Request) -> Dict[str, Any]:
    # B1-13 审计修复：Transfer-Encoding: chunked 请求没有 Content-Length，
    # 原 length==0 短路把整个 body 丢弃（静默当空对象 → 400 missing field）。
    # chunked 改走流式读取，同样受 MAX_BODY_BYTES 上限约束。
    if "chunked" in (request.headers.get("Transfer-Encoding") or "").lower():
        chunks: list = []
        received = 0
        async for chunk in request.stream():
            received += len(chunk)
            if received > MAX_BODY_BYTES:
                raise ApiError(413, f"body too large (>{MAX_BODY_BYTES} bytes)")
            chunks.append(chunk)
        raw = b"".join(chunks)
    else:
        length = int(request.headers.get("Content-Length") or 0)
        if length == 0:
            return {}
        if length > MAX_BODY_BYTES:
            raise ApiError(413, f"body too large ({length} bytes)")
        raw = await request.body()
    if not raw:
        return {}
    try:
        body = json.loads(raw.decode("utf-8"))
    except Exception as exc:  # noqa: BLE001
        raise ApiError(400, f"invalid JSON body: {exc}") from exc
    if not isinstance(body, dict):
        raise ApiError(400, "JSON body must be an object")
    return body


def _one(request: Request, key: str) -> str:
    vals = parse_qs(request.url.query).get(key)
    if not vals or not vals[0]:
        raise ApiError(400, f"missing query parameter: {key}")
    return vals[0]


def _query_int(request: Request, key: str, default: int) -> int:
    """Optional integer query parameter; malformed values are a CLIENT error
    (400) — the pre-fix behavior leaked them as 502 "upstream failure"."""
    vals = parse_qs(request.url.query).get(key)
    if not vals or not vals[0]:
        return default
    try:
        return int(vals[0])
    except ValueError:
        raise ApiError(400, f"invalid query parameter {key}={vals[0]!r} "
                            "(expect integer)") from None


def _int_field(body: Dict[str, Any], key: str) -> int:
    """B3-3 严格化（_opt_bool 同款）：只收 int。bool（int 子类，恒真陷阱）、
    float（含整值形式）、字符串数字一律 400——原 int() 强转放过这些垃圾
    类型（"8123"/8123.5/True 都曾被静默接受）。"""
    raw = body[key]
    if isinstance(raw, bool) or not isinstance(raw, int):
        raise ApiError(400, f"invalid field {key}={raw!r} "
                            "(expect integer)") from None
    return raw


def _opt_bool(body: Dict[str, Any], key: str) -> bool:
    """可选布尔字段（B1-12 审计修复）：只接受 JSON 布尔或 "true"/"false"
    字符串，其余 400。bool("false") is True 的恒真陷阱在此堵死——字符串
    "false" 曾把 domain/no_verify 打开（行为与字面相反）。缺省/None → False。"""
    value = body.get(key)
    if value is None:
        return False
    if isinstance(value, bool):
        return value
    if isinstance(value, str) and value.lower() in ("true", "false"):
        return value.lower() == "true"
    raise ApiError(400, f'field {key} must be boolean or "true"/"false"')


# ---------------------------------------------------------------------------
# 鉴权依赖（/healthz 豁免由路由不挂依赖实现）
# ---------------------------------------------------------------------------

async def require_token(request: Request) -> None:
    # B3-2 fail-closed：tokenFile 已配置但读失败（EACCES/EIO）时鉴权材料
    # 不可得——503 + 服务端 stderr 日志，绝不静默放行（原实现吞异常当
    # 无 token，读失败瞬间整面鉴权失效）。
    try:
        token = config.manager_token()
    except config.TokenFileError as exc:
        print(f"[manager] auth fail-closed: {exc}", file=sys.stderr, flush=True)
        raise ApiError(503, "token unavailable (auth source unreadable)") \
            from exc
    if not token:
        return
    # 常量时间比较（bytes 形态，避免非 ASCII 头触发 TypeError）
    if not hmac.compare_digest(
            request.headers.get("Authorization", "").encode("utf-8"),
            f"Bearer {token}".encode("utf-8")):
        raise ApiError(401, "unauthorized (bearer token required)")


# ---------------------------------------------------------------------------
# app 工厂
# ---------------------------------------------------------------------------

def create_app() -> FastAPI:
    app = FastAPI(title="openshell-manager", docs_url=None, redoc_url=None, openapi_url=None)

    @app.exception_handler(ApiError)
    async def _api_error(_req: Request, exc: ApiError):
        return JSONResponse({"error": exc.message}, status_code=exc.status)

    @app.exception_handler(LookupError)
    async def _lookup(_req: Request, exc: LookupError):
        return JSONResponse({"error": str(exc)}, status_code=404)

    @app.exception_handler(StarletteHTTPException)
    async def _http_exc(req: Request, exc: StarletteHTTPException):
        if exc.status_code == 404:
            path = req.url.path.rstrip("/") or "/"
            return JSONResponse({"error": f"no route for {req.method} {path}"}, status_code=404)
        return JSONResponse({"error": str(exc.detail)}, status_code=exc.status_code)

    @app.exception_handler(Exception)
    async def _unhandled(req: Request, exc: Exception):
        # B3-3：兜底 = 500 + 通用文案。原 502 让上游把服务端缺陷按"网关
        # 不可达"重试/降级，且 {type: msg} 直接向客户端泄漏内部细节；
        # 细节只进服务端 stderr 日志。
        print(f"[manager] unhandled error on {req.method} {req.url.path}: "
              f"{type(exc).__name__}: {exc}", file=sys.stderr, flush=True)
        return JSONResponse({"error": "internal error"}, status_code=500)

    # -- health（豁免鉴权，探活口径不变） -----------------------------------

    @app.get("/healthz")
    def healthz() -> Dict[str, Any]:
        return {"ok": True}

    # -- gateway ------------------------------------------------------------

    @app.get("/api/v1/gateway/health", dependencies=[Depends(require_token)])
    def gateway_health() -> Dict[str, Any]:
        return facade.health()

    # -- sandboxes ----------------------------------------------------------

    @app.post("/api/v1/sandboxes", dependencies=[Depends(require_token)])
    async def sandbox_create(request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need_str(body, "workspace")
        name = _opt_str(body, "name")
        try:
            return await run_in_threadpool(
                facade.create, workspace=body["workspace"],
                name=name or "", spec=body.get("spec") or {})
        except ValueError as exc:  # json_format.ParseError 是 ValueError 子类
            raise ApiError(400, f"invalid spec: {exc}") from exc

    @app.get("/api/v1/sandboxes", dependencies=[Depends(require_token)])
    def sandbox_list(request: Request) -> Dict[str, Any]:
        return {"sandboxes": facade.list_all(
            limit=_query_int(request, "limit", 500))}

    @app.get("/api/v1/sandboxes/{name}", dependencies=[Depends(require_token)])
    def sandbox_get(name: str, request: Request) -> Dict[str, Any]:
        return facade.get(name=name, workspace=_one(request, "workspace"))

    @app.delete("/api/v1/sandboxes/{name}", dependencies=[Depends(require_token)])
    def sandbox_delete(name: str, request: Request) -> Dict[str, Any]:
        return {"deleted": facade.delete(name=name, workspace=_one(request, "workspace"))}

    @app.post("/api/v1/sandboxes/{name}/wait-ready", dependencies=[Depends(require_token)])
    async def wait_ready(name: str, request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need_str(body, "workspace")
        raw_timeout = body.get("timeout_seconds", WAIT_READY_TIMEOUT_DEFAULT)
        try:
            timeout = float(raw_timeout)
        except (TypeError, ValueError):
            raise ApiError(400, f"invalid field timeout_seconds={raw_timeout!r} "
                                "(expect number)") from None
        # B3-1 服务端上限：客户端曾可传 1e9 让南向连接无限占用。
        if timeout > WAIT_READY_TIMEOUT_MAX:
            raise ApiError(400, f"invalid field timeout_seconds={raw_timeout!r} "
                                f"(exceeds server-side cap of "
                                f"{WAIT_READY_TIMEOUT_MAX:.0f} seconds)")
        return await run_in_threadpool(
            facade.wait_ready, name=name, workspace=body["workspace"],
            timeout_seconds=timeout)

    @app.post("/api/v1/sandboxes/exec", dependencies=[Depends(require_token)])
    async def sandbox_exec(request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need(body, "sandbox_id", "command")
        _need_str(body, "sandbox_id")
        # command 必须是字符串列表：裸字符串若被 list() 拆成单字符数组，
        # 会在沙箱里静默执行垃圾命令（如 ['e','c','h','o',…]）
        command = body["command"]
        if (not isinstance(command, list)
                or not all(isinstance(arg, str) for arg in command)):
            raise ApiError(400, "command must be a list of strings")
        env = body.get("env") or {}
        if not isinstance(env, dict) or not all(
                isinstance(k, str) and isinstance(v, str)
                for k, v in env.items()):
            raise ApiError(400, "env must be an object with string keys "
                                "and string values")
        workdir = _opt_str(body, "workdir")
        timeout = body.get("timeout_seconds")
        if timeout is not None:
            try:
                timeout = int(timeout)
            except (TypeError, ValueError):
                raise ApiError(400, f"invalid field timeout_seconds={timeout!r} "
                                    "(expect integer)") from None
        stdin = None
        if body.get("stdin_b64"):
            try:
                stdin = base64.b64decode(body["stdin_b64"])
            except ValueError as exc:  # binascii.Error 的基类
                raise ApiError(400, f"invalid stdin_b64: {exc}") from exc
        return await run_in_threadpool(
            facade.exec, sandbox_id=body["sandbox_id"], command=command,
            workdir=workdir, environment=env, stdin=stdin,
            timeout_seconds=timeout)

    @app.get("/api/v1/sandboxes/{name}/logs", dependencies=[Depends(require_token)])
    def sandbox_logs(name: str, request: Request) -> Dict[str, Any]:
        # B1-15 审计修复：GetSandboxLogs 属 ExecSandbox 系 RPC，只认
        # sandbox_id=UUID——原实现把路由 name 原样当 sandbox_id 查询，网关侧
        # 必 NOT_FOUND。与 /files 同口径（ADR-173）：接口层收 name 自解析 UUID。
        workspace = _one(request, "workspace")
        sandbox_id = facade.resolve_sandbox_id(name=name, workspace=workspace)
        return {"logs": facade.get_logs(
            sandbox_id=sandbox_id, workspace=workspace,
            lines=_query_int(request, "lines", 2000),
            since_ms=_query_int(request, "since_ms", 0))}

    @app.post("/api/v1/sandboxes/{name}/update-config", dependencies=[Depends(require_token)])
    async def update_config(name: str, request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need(body, "policy")
        _need_str(body, "workspace")
        try:
            return await run_in_threadpool(
                facade.update_config, name=name, workspace=body["workspace"],
                policy=body["policy"])
        except ValueError as exc:  # json_format.ParseError 是 ValueError 子类
            raise ApiError(400, f"invalid policy: {exc}") from exc

    # -- 文件上传（流式；接口层收 name 自解析 UUID，ADR-173/174） -------------

    @app.post("/api/v1/sandboxes/{name}/files", dependencies=[Depends(require_token)])
    async def sandbox_upload(name: str, request: Request) -> Dict[str, Any]:
        return await _handle_upload(name, request)

    # -- services -----------------------------------------------------------

    @app.post("/api/v1/sandboxes/{name}/services", dependencies=[Depends(require_token)])
    async def service_expose(name: str, request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need(body, "target_port")
        _need_str(body, "workspace", "service")
        return await run_in_threadpool(
            facade.expose_service, sandbox=name, service=body["service"],
            target_port=_int_field(body, "target_port"),
            workspace=body["workspace"], domain=_opt_bool(body, "domain"))

    @app.get("/api/v1/sandboxes/{name}/services", dependencies=[Depends(require_token)])
    def services_list(name: str, request: Request) -> Dict[str, Any]:
        all_ws = parse_qs(request.url.query).get(
            "all_workspaces", ["false"])[0].lower() in ("1", "true", "yes")
        workspace = "" if all_ws else _one(request, "workspace")
        return {"services": facade.list_services(
            sandbox=name, workspace=workspace,
            limit=_query_int(request, "limit", 100),
            offset=_query_int(request, "offset", 0),
            all_workspaces=all_ws)}

    @app.delete("/api/v1/sandboxes/{name}/services/{service}", dependencies=[Depends(require_token)])
    def service_delete(name: str, service: str, request: Request) -> Dict[str, Any]:
        return facade.delete_service(sandbox=name, service=service,
                                     workspace=_one(request, "workspace"))

    # -- inference 路由/providers --------------------------------------------

    @app.get("/api/v1/inference/route", dependencies=[Depends(require_token)])
    def route_get(request: Request) -> Dict[str, Any]:
        return facade.get_route(workspace=_one(request, "workspace"))

    @app.put("/api/v1/inference/route", dependencies=[Depends(require_token)])
    async def route_set(request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need_str(body, "workspace", "provider", "model")
        return await run_in_threadpool(
            facade.set_route, workspace=body["workspace"],
            provider=body["provider"], model=body["model"],
            no_verify=_opt_bool(body, "no_verify"))

    @app.get("/api/v1/inference/providers", dependencies=[Depends(require_token)])
    def providers_list(request: Request) -> Dict[str, Any]:
        return {"providers": facade.list_providers(workspace=_one(request, "workspace"))}

    @app.get("/api/v1/inference/providers/{name}", dependencies=[Depends(require_token)])
    def provider_get(name: str, request: Request) -> Dict[str, Any]:
        return facade.get_provider(name=name, workspace=_one(request, "workspace"))

    @app.put("/api/v1/inference/providers", dependencies=[Depends(require_token)])
    async def providers_upsert(request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need_str(body, "workspace", "name", "type")
        return await run_in_threadpool(
            facade.upsert_provider, workspace=body["workspace"],
            name=body["name"], type_=body["type"],
            credentials=body.get("credentials") or {},
            conf=body.get("config") or {})

    @app.delete("/api/v1/inference/providers/{name}",
                dependencies=[Depends(require_token)])
    def provider_delete(name: str, request: Request) -> Dict[str, Any]:
        return facade.delete_provider(name=name,
                                      workspace=_one(request, "workspace"))

    return app


# ---------------------------------------------------------------------------
# 流式上传（原 _handle_upload 语义，FastAPI/uvicorn 传输）
# ---------------------------------------------------------------------------

async def _handle_upload(name: str, request: Request) -> Dict[str, Any]:
    """接口层对外收沙箱 name（+可选 workspace 查询参数，缺省 default），
    内部先解析为 UUID 再走网关流式写盘；原始 body spool 到磁盘（有界内存）。"""
    content_type = request.headers.get("Content-Type", "")
    if not content_type.startswith("multipart/form-data"):
        raise ApiError(415, "content-type must be multipart/form-data "
                            "(fields: path, mode?; file part: file)")
    length_header = request.headers.get("Content-Length")
    if not length_header:
        raise ApiError(411, "Content-Length required for file upload")
    length = int(length_header)
    limit = config.max_upload_bytes()
    if limit and length > limit:
        raise ApiError(413, f"upload of {length} bytes exceeds max_upload_bytes "
                            f"limit of {limit} (OPENSHELL_MANAGER_MAX_UPLOAD_BYTES; 0 = unlimited)")

    tmp: tempfile.SpooledTemporaryFile = tempfile.SpooledTemporaryFile(max_size=1 << 20)
    # B1-12 审计修复：spool 文件全程 try/finally 关闭。原接收循环在 finally
    # 保护之外，客户端中途断连（request.stream() 抛出）等异常路径会泄漏
    # spool 临时文件（大文件直落磁盘，靠 GC 兜底不可靠）。
    try:
        received = 0
        async for chunk in request.stream():  # 有界内存：边收边落 spool
            received += len(chunk)
            if limit and received > limit:
                raise ApiError(413, "upload stream exceeded declared Content-Length / limit")
            tmp.write(chunk)
        tmp.seek(0)

        workspace = parse_qs(request.url.query).get("workspace", ["default"])[0]

        def work() -> Dict[str, Any]:
            try:
                boundary = boundary_from_content_type(content_type)
                parser = StreamingMultipartParser(tmp, boundary)
                fields, file_stream = parser.parse()
            except UploadError as exc:
                raise ApiError(400, f"invalid multipart body: {exc}") from exc
            path = fields.get(b"path", b"").decode("utf-8", errors="replace")
            mode_raw = fields.get(b"mode")
            mode = (mode_raw.decode("ascii", errors="replace").strip()
                    if mode_raw else None)
            if mode is not None and not re.fullmatch(r"[0-7]{3,4}", mode):
                raise ApiError(400, f"invalid mode: {mode!r} (expect octal like 0644)")
            if not path.startswith("/"):
                raise ApiError(400, "path must be absolute")

            def capped() -> "Iterator[bytes]":
                sent = 0
                for piece in file_stream:
                    sent += len(piece)
                    if limit and sent > limit:
                        raise ApiError(413, "upload stream exceeded declared Content-Length / limit")
                    yield piece

            sandbox_id = facade.resolve_sandbox_id(name=name, workspace=workspace)
            return facade.write_file_stream(sandbox_id=sandbox_id, path=path,
                                            chunks=capped(), mode=mode or None)

        return await run_in_threadpool(work)
    except ValueError as exc:
        raise ApiError(400, str(exc)) from exc
    finally:
        tmp.close()


def serve() -> None:
    """入口（__main__）：uvicorn 承载 FastAPI app（ADR-174）。"""
    import uvicorn

    config.validate()
    bind, port = config.manager_bind(), config.manager_port()
    if config.manager_token():
        auth_note = f"token auth ENABLED ({len(config.manager_token())} chars)"
    else:
        auth_note = "token auth DISABLED (loopback bind only)"
    print(f"[manager] (fastapi/uvicorn) listening on {bind}:{port} | gateway="
          f"{config.gateway_endpoint()} | {auth_note}", flush=True)
    uvicorn.run(create_app(), host=bind, port=port, log_level="warning", access_log=False)

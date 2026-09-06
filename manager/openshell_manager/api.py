"""OpenShell manager HTTP surface — FastAPI 架构（ADR-174，人类指令 2026-09-01）。

原 stdlib http.server 实现（http_api.py，保留作参照）整体迁移 FastAPI/uvicorn：
  - 路由声明式注册（替代 regex ROUTES 表 + 手写 dispatch）；
  - 鉴权收敛为依赖注入（/healthz 豁免，其余 Bearer token）；
  - 异常处理器统一错误契约 {"error": msg}（ApiError/LookupError/404 no route/兜底 502），
    JSON 端点手工解包 body 保持既有错误语义（"invalid JSON body"/413/非对象 400）；
  - /files 流式上传：原始 body spool 到磁盘（有界内存）后沿用 upload.py 流式解析器，
    接口层收沙箱 name（+?workspace=，缺省 default）内部自解析 UUID（ADR-173）。

南向 gRPC（gateway.py）与对外 JSON 契约逐字节不变（tests/test_contract.py 锁定）。
"""
from __future__ import annotations

import base64  # noqa: F401 — exec 端点 stdin_b64 解码
import hmac
import json
import re
import tempfile
from typing import Any, Dict, Optional
from urllib.parse import parse_qs

from fastapi import Depends, FastAPI, Request
from fastapi.responses import JSONResponse
from starlette.concurrency import run_in_threadpool
from starlette.exceptions import HTTPException as StarletteHTTPException

from . import config
from .gateway import GatewayFacade
from .upload import StreamingMultipartParser, UploadError, boundary_from_content_type

MAX_BODY_BYTES = 8 * 1024 * 1024

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


async def _json_body(request: Request) -> Dict[str, Any]:
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
    raw = body[key]
    try:
        return int(raw)
    except (TypeError, ValueError):
        raise ApiError(400, f"invalid field {key}={raw!r} "
                            "(expect integer)") from None


# ---------------------------------------------------------------------------
# 鉴权依赖（/healthz 豁免由路由不挂依赖实现）
# ---------------------------------------------------------------------------

async def require_token(request: Request) -> None:
    token = config.manager_token()
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
    async def _unhandled(_req: Request, exc: Exception):
        return JSONResponse({"error": f"{type(exc).__name__}: {exc}"}, status_code=502)

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
        _need(body, "workspace")
        try:
            return facade.create(workspace=body["workspace"],
                                 name=body.get("name") or "",
                                 spec=body.get("spec") or {})
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
        _need(body, "workspace")
        raw_timeout = body.get("timeout_seconds", 300)
        try:
            timeout = float(raw_timeout)
        except (TypeError, ValueError):
            raise ApiError(400, f"invalid field timeout_seconds={raw_timeout!r} "
                                "(expect number)") from None
        return facade.wait_ready(name=name, workspace=body["workspace"],
                                 timeout_seconds=timeout)

    @app.post("/api/v1/sandboxes/exec", dependencies=[Depends(require_token)])
    async def sandbox_exec(request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need(body, "sandbox_id", "command")
        # command 必须是字符串列表：裸字符串若被 list() 拆成单字符数组，
        # 会在沙箱里静默执行垃圾命令（如 ['e','c','h','o',…]）
        command = body["command"]
        if (not isinstance(command, list)
                or not all(isinstance(arg, str) for arg in command)):
            raise ApiError(400, "command must be a list of strings")
        stdin = None
        if body.get("stdin_b64"):
            try:
                stdin = base64.b64decode(body["stdin_b64"])
            except ValueError as exc:  # binascii.Error 的基类
                raise ApiError(400, f"invalid stdin_b64: {exc}") from exc
        return facade.exec(sandbox_id=body["sandbox_id"],
                           command=command,
                           workdir=body.get("workdir"),
                           environment=body.get("env") or {},
                           stdin=stdin,
                           timeout_seconds=body.get("timeout_seconds"))

    @app.get("/api/v1/sandboxes/{name}/logs", dependencies=[Depends(require_token)])
    def sandbox_logs(name: str, request: Request) -> Dict[str, Any]:
        return {"logs": facade.get_logs(
            sandbox_id=name, workspace=_one(request, "workspace"),
            lines=_query_int(request, "lines", 2000),
            since_ms=_query_int(request, "since_ms", 0))}

    @app.post("/api/v1/sandboxes/{name}/update-config", dependencies=[Depends(require_token)])
    async def update_config(name: str, request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need(body, "workspace", "policy")
        try:
            return facade.update_config(name=name, workspace=body["workspace"],
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
        _need(body, "workspace", "service", "target_port")
        return facade.expose_service(sandbox=name, service=body["service"],
                                     target_port=_int_field(body, "target_port"),
                                     workspace=body["workspace"],
                                     domain=bool(body.get("domain", False)))

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
        _need(body, "workspace", "provider", "model")
        return facade.set_route(workspace=body["workspace"],
                                provider=body["provider"], model=body["model"],
                                no_verify=bool(body.get("no_verify", False)))

    @app.get("/api/v1/inference/providers", dependencies=[Depends(require_token)])
    def providers_list(request: Request) -> Dict[str, Any]:
        return {"providers": facade.list_providers(workspace=_one(request, "workspace"))}

    @app.get("/api/v1/inference/providers/{name}", dependencies=[Depends(require_token)])
    def provider_get(name: str, request: Request) -> Dict[str, Any]:
        return facade.get_provider(name=name, workspace=_one(request, "workspace"))

    @app.put("/api/v1/inference/providers", dependencies=[Depends(require_token)])
    async def providers_upsert(request: Request) -> Dict[str, Any]:
        body = await _json_body(request)
        _need(body, "workspace", "name", "type")
        return facade.upsert_provider(workspace=body["workspace"],
                                      name=body["name"], type_=body["type"],
                                      credentials=body.get("credentials") or {},
                                      conf=body.get("config") or {})

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
    received = 0
    async for chunk in request.stream():  # 有界内存：边收边落 spool
        received += len(chunk)
        if limit and received > limit:
            tmp.close()
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

    try:
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

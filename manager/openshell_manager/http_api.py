"""HTTP surface of the OpenShell manager microservice (stdlib only).

Transport-only: JSON in/out, bearer-token auth on /api/*, protobuf handling
stays server-side. The engine (four-direction-pentest-engine) talks to this
service via ``engine/openshell_manager_client.py`` when
``OPENSHELL_MANAGER_URL`` is configured; otherwise it speaks gRPC to the
gateway directly (unchanged default).
"""
from __future__ import annotations

import base64
import json
import re
import socket
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse
from typing import Any, Callable, Dict, Optional, Tuple

from . import config
from .gateway import GatewayFacade

MAX_BODY_BYTES = 8 * 1024 * 1024

facade = GatewayFacade()


class ApiError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


# ---------------------------------------------------------------------------
# route table: (method, regex) -> handler(path_args, query, body)
# ---------------------------------------------------------------------------

def _need(body: Dict[str, Any], *keys: str) -> None:
    missing = [k for k in keys if not body.get(k)]
    if missing:
        raise ApiError(400, f"missing required field(s): {', '.join(missing)}")


def h_gateway_health(_a, _q, _b) -> Dict[str, Any]:
    return facade.health()


def h_sandbox_create(_a, _q, body) -> Dict[str, Any]:
    _need(body, "workspace")
    return facade.create(workspace=body["workspace"],
                         name=body.get("name") or "",
                         spec=body.get("spec") or {})


def h_sandbox_get(a, q, _b) -> Dict[str, Any]:
    return facade.get(name=a["name"], workspace=_one(q, "workspace"))


def h_sandbox_delete(a, q, _b) -> Dict[str, Any]:
    return {"deleted": facade.delete(name=a["name"],
                                     workspace=_one(q, "workspace"))}


def h_sandbox_wait_ready(a, _q, body) -> Dict[str, Any]:
    _need(body, "workspace")
    return facade.wait_ready(name=a["name"], workspace=body["workspace"],
                             timeout_seconds=float(body.get("timeout_seconds", 300)))


def h_sandbox_exec(_a, _q, body) -> Dict[str, Any]:
    _need(body, "sandbox_id", "command")
    stdin = None
    if body.get("stdin_b64"):
        stdin = base64.b64decode(body["stdin_b64"])
    return facade.exec(sandbox_id=body["sandbox_id"],
                       command=body["command"],
                       workdir=body.get("workdir"),
                       environment=body.get("env") or {},
                       stdin=stdin,
                       timeout_seconds=body.get("timeout_seconds"))


def h_sandbox_upload(a, _q, _b) -> Dict[str, Any]:
    """Route marker only — the Handler special-cases this route and streams
    the multipart body via ``_handle_upload`` (see _dispatch). Reaching this
    function means the request was not multipart."""
    raise ApiError(415, "content-type must be multipart/form-data "
                        "(fields: path, mode?; file part: file)")


def h_sandbox_logs(a, q, _b) -> Dict[str, Any]:
    return {"logs": facade.get_logs(
        sandbox_id=a["name"], workspace=_one(q, "workspace"),
        lines=int(q.get("lines", ["2000"])[0]),
        since_ms=int(q.get("since_ms", ["0"])[0]))}


def h_sandbox_update_config(a, _q, body) -> Dict[str, Any]:
    _need(body, "workspace", "policy")
    return facade.update_config(name=a["name"], workspace=body["workspace"],
                                policy=body["policy"])


def h_sandbox_list_all(_a, q, _b) -> Dict[str, Any]:
    return {"sandboxes": facade.list_all(limit=int(q.get("limit", ["500"])[0]))}


def h_service_expose(a, _q, body) -> Dict[str, Any]:
    _need(body, "workspace", "service", "target_port")
    return facade.expose_service(sandbox=a["name"], service=body["service"],
                                 target_port=int(body["target_port"]),
                                 workspace=body["workspace"],
                                 domain=bool(body.get("domain", False)))


def h_services_list(a, q, _b) -> Dict[str, Any]:
    all_ws = q.get("all_workspaces", ["false"])[0].lower() in ("1", "true", "yes")
    workspace = "" if all_ws else _one(q, "workspace")
    return {"services": facade.list_services(
        sandbox=a["name"], workspace=workspace,
        limit=int(q.get("limit", ["100"])[0]),
        offset=int(q.get("offset", ["0"])[0]),
        all_workspaces=all_ws)}


def h_service_delete(a, q, _b) -> Dict[str, Any]:
    return facade.delete_service(sandbox=a["name"], service=a["service"],
                                 workspace=_one(q, "workspace"))


def h_route_get(_a, q, _b) -> Dict[str, Any]:
    return facade.get_route(workspace=_one(q, "workspace"))


def h_route_set(_a, _q, body) -> Dict[str, Any]:
    _need(body, "workspace", "provider", "model")
    return facade.set_route(workspace=body["workspace"],
                            provider=body["provider"], model=body["model"],
                            no_verify=bool(body.get("no_verify", False)))


def h_providers_list(_a, q, _b) -> Dict[str, Any]:
    return {"providers": facade.list_providers(workspace=_one(q, "workspace"))}


def h_provider_get(a, q, _b) -> Dict[str, Any]:
    return facade.get_provider(name=a["name"], workspace=_one(q, "workspace"))


def h_providers_upsert(_a, _q, body) -> Dict[str, Any]:
    _need(body, "workspace", "name", "type")
    return facade.upsert_provider(workspace=body["workspace"],
                                  name=body["name"], type_=body["type"],
                                  credentials=body.get("credentials") or {},
                                  conf=body.get("config") or {})


ROUTES: list[Tuple[str, re.Pattern, Callable]] = [
    ("GET", re.compile(r"^/api/v1/gateway/health$"), h_gateway_health),
    ("POST", re.compile(r"^/api/v1/sandboxes$"), h_sandbox_create),
    ("GET", re.compile(r"^/api/v1/sandboxes$"), h_sandbox_list_all),
    ("GET", re.compile(
        r"^/api/v1/sandboxes/(?P<name>[^/]+)/logs$"), h_sandbox_logs),
    ("POST", re.compile(
        r"^/api/v1/sandboxes/(?P<name>[^/]+)/wait-ready$"), h_sandbox_wait_ready),
    ("POST", re.compile(
        r"^/api/v1/sandboxes/(?P<name>[^/]+)/update-config$"),
     h_sandbox_update_config),
    ("POST", re.compile(r"^/api/v1/sandboxes/exec$"), h_sandbox_exec),
    ("POST", re.compile(
        r"^/api/v1/sandboxes/(?P<name>[^/]+)/files$"), h_sandbox_upload),
    ("POST", re.compile(
        r"^/api/v1/sandboxes/(?P<name>[^/]+)/services$"), h_service_expose),
    ("GET", re.compile(
        r"^/api/v1/sandboxes/(?P<name>[^/]+)/services$"), h_services_list),
    ("DELETE", re.compile(
        r"^/api/v1/sandboxes/(?P<name>[^/]+)/services/(?P<service>[^/]+)$"),
     h_service_delete),
    ("GET", re.compile(r"^/api/v1/sandboxes/(?P<name>[^/]+)$"), h_sandbox_get),
    ("DELETE", re.compile(r"^/api/v1/sandboxes/(?P<name>[^/]+)$"), h_sandbox_delete),
    ("GET", re.compile(r"^/api/v1/inference/route$"), h_route_get),
    ("PUT", re.compile(r"^/api/v1/inference/route$"), h_route_set),
    ("GET", re.compile(r"^/api/v1/inference/providers$"), h_providers_list),
    ("GET", re.compile(
        r"^/api/v1/inference/providers/(?P<name>[^/]+)$"), h_provider_get),
    ("PUT", re.compile(r"^/api/v1/inference/providers$"), h_providers_upsert),
]


def _one(q, key: str) -> str:
    vals = q.get(key)
    if not vals or not vals[0]:
        raise ApiError(400, f"missing query parameter: {key}")
    return vals[0]


# ---------------------------------------------------------------------------
# HTTP server
# ---------------------------------------------------------------------------

class Handler(BaseHTTPRequestHandler):
    server_version = "openshell-manager/1.0"
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # quiet, one-line to stdout
        sys.stdout.write("[manager] %s\n" % (fmt % args))
        sys.stdout.flush()

    # -- helpers -------------------------------------------------------------

    def _json(self, status: int, payload: Dict[str, Any]) -> None:
        data = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _authorized(self) -> bool:
        token = config.manager_token()
        if not token:
            return True
        header = self.headers.get("Authorization", "")
        return header == f"Bearer {token}"

    def _read_body(self) -> Dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        if length == 0:
            return {}
        if length > MAX_BODY_BYTES:
            raise ApiError(413, f"body too large ({length} bytes)")
        raw = self.rfile.read(length)
        if not raw:
            return {}
        try:
            body = json.loads(raw.decode("utf-8"))
        except Exception as exc:  # noqa: BLE001
            raise ApiError(400, f"invalid JSON body: {exc}") from exc
        if not isinstance(body, dict):
            raise ApiError(400, "JSON body must be an object")
        return body

    def _dispatch(self, method: str) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        query = parse_qs(parsed.query)
        try:
            if path == "/healthz":
                self._json(200, {"ok": True})
                return
            if not self._authorized():
                self._json(401, {"error": "unauthorized (bearer token required)"})
                return
            for route_method, pattern, handler in ROUTES:
                if route_method != method:
                    continue
                match = pattern.match(path)
                if not match:
                    continue
                if handler is h_sandbox_upload:
                    # The upload route streams its multipart body straight
                    # through to the sandbox; every other endpoint keeps its
                    # original buffered JSON handling.
                    self._json(200, self._handle_upload(
                        match.groupdict()["name"], query.get("workspace", ["default"])[0]))
                    return
                body = self._read_body() if method in ("POST", "PUT") else {}
                self._json(200, handler(match.groupdict(), query, body))
                return
            raise ApiError(404, f"no route for {method} {path}")
        except ApiError as exc:
            self._json(exc.status, {"error": exc.message})
        except LookupError as exc:
            self._json(404, {"error": str(exc)})
        except Exception as exc:  # noqa: BLE001 - upstream/transport failures
            self._json(502, {"error": f"{type(exc).__name__}: {exc}"})

    def _handle_upload(self, name: str, workspace: str) -> Dict[str, Any]:
        """Stream a multipart upload into the sandbox without buffering it.

        接口层对外收沙箱 name（+可选 workspace 查询参数，缺省 default），
        内部先解析为 UUID 再走网关流式写盘（人类指令 2026-09-01）。

        Manager memory stays constant (~2MiB chunk window) regardless of
        file size: parser output is forwarded chunk-by-chunk through
        ``write_file_stream`` while the request is still arriving.
        """
        content_type = self.headers.get("Content-Type", "")
        if not content_type.startswith("multipart/form-data"):
            raise ApiError(415, "content-type must be multipart/form-data "
                                "(fields: path, mode?; file part: file)")
        length_header = self.headers.get("Content-Length")
        if not length_header:
            # Streaming needs a known length; chunked upload bodies are not
            # worth implementing for a curl -F / requests-files clientele.
            self.close_connection = True
            raise ApiError(411, "Content-Length required for file upload")
        length = int(length_header)
        limit = config.max_upload_bytes()
        if limit and length > limit:
            self.close_connection = True
            raise ApiError(
                413, f"upload of {length} bytes exceeds max_upload_bytes "
                     f"limit of {limit} (OPENSHELL_MANAGER_MAX_UPLOAD_BYTES; "
                     f"0 = unlimited)")
        from .upload import (StreamingMultipartParser, UploadError,
                             boundary_from_content_type)

        class _BoundedBody:
            """Reads at most ``length`` bytes: rfile.read(n) on a keep-alive
            connection blocks until n bytes arrive, so the parser must never
            ask for more than the request body contains."""

            def __init__(self, rfile, length):
                self._rfile = rfile
                self._remaining = length

            def read(self, n: int) -> bytes:
                if self._remaining <= 0:
                    return b""
                data = self._rfile.read(min(n, self._remaining))
                self._remaining -= len(data)
                return data

        body_reader = _BoundedBody(self.rfile, length)
        try:
            boundary = boundary_from_content_type(content_type)
            parser = StreamingMultipartParser(body_reader, boundary)
            fields, file_stream = parser.parse()
        except UploadError as exc:
            self.close_connection = True
            raise ApiError(400, f"invalid multipart body: {exc}") from exc
        path = fields.get(b"path", b"").decode("utf-8", errors="replace")
        mode_raw = fields.get(b"mode")
        mode = (mode_raw.decode("ascii", errors="replace").strip()
                if mode_raw else None)
        if mode is not None and not re.fullmatch(r"[0-7]{3,4}", mode):
            self.close_connection = True
            raise ApiError(400, f"invalid mode: {mode!r} "
                                "(expect octal like 0644)")

        def capped_stream():
            sent = 0
            for chunk in file_stream:
                sent += len(chunk)
                if limit and sent > limit:
                    # Body framing disagrees with the declared length.
                    self.close_connection = True
                    raise ApiError(413, "upload stream exceeded declared "
                                        "Content-Length / limit")
                yield chunk

        sandbox_id = facade.resolve_sandbox_id(name=name, workspace=workspace)
        try:
            result = facade.write_file_stream(
                sandbox_id=sandbox_id, path=path, chunks=capped_stream(),
                mode=mode or None)
        except ApiError:
            raise
        except ValueError as exc:
            self.close_connection = True
            raise ApiError(400, str(exc)) from exc
        # Drain the unread remainder (epilogue after the closing boundary)
        # so the keep-alive connection stays usable for the next request.
        while True:
            block = body_reader.read(65536)
            if not block:
                break
        return result

    # -- verbs ---------------------------------------------------------------

    def do_GET(self):  # noqa: N802
        self._dispatch("GET")

    def do_POST(self):  # noqa: N802
        self._dispatch("POST")

    def do_PUT(self):  # noqa: N802
        self._dispatch("PUT")

    def do_DELETE(self):  # noqa: N802
        self._dispatch("DELETE")


def serve() -> None:
    config.validate()
    bind, port = config.manager_bind(), config.manager_port()
    if config.manager_token():
        auth_note = f"token auth ENABLED ({len(config.manager_token())} chars)"
    else:
        auth_note = "token auth DISABLED (loopback bind only)"
    server = ThreadingHTTPServer((bind, port), Handler)
    host, actual_port = server.server_address[:2]
    print(f"[manager] listening on {host}:{actual_port} | gateway="
          f"{config.gateway_endpoint()} | {auth_note}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    serve()

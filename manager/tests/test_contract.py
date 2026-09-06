#!/usr/bin/env python3
"""Contract tests for the OpenShell manager HTTP surface.

Runs the real ThreadingHTTPServer against a GatewayFacade wired to a FAKE
SDK client (injected via client_factory) — no gateway, no network. Covers:
auth (token on/off, 401, tokenFile fallback priority), healthz, gateway
health, sandbox create/get/exec/delete/wait-ready/list-all, inference route
get/set, provider list/upsert, service expose/list/delete, and error mapping (404 unknown route/lookup,
400 missing fields/invalid JSON/non-object body, 413 oversized body, 502
upstream failure), plus config.validate() bind discipline.

Requires the vendored protobuf modules (openshell._proto) for the
dict->SandboxSpec parse; SKIPs (exit 0) where the vendor tree is absent —
mirrors the engine's "libs optional" discipline.
"""
from __future__ import annotations

import base64
import json
import os
import socket
import sys
import tempfile
import threading
import time
import uvicorn
from openshell_manager import api
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from typing import Dict, Tuple

SERVICE_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(SERVICE_ROOT))

from openshell_manager import config  # noqa: E402
from openshell_manager import http_api  # noqa: E402
import openshell_manager.gateway as gw  # noqa: E402
from openshell_manager.gateway import GatewayFacade  # noqa: E402

RESULTS = []


def run_case(fn):
    try:
        fn()
        RESULTS.append((fn.__name__, True, ""))
        print(f"  PASS {fn.__name__}")
    except AssertionError as exc:
        RESULTS.append((fn.__name__, False, str(exc)))
        print(f"  FAIL {fn.__name__}: {exc}")
    except Exception as exc:  # noqa: BLE001
        RESULTS.append((fn.__name__, False, f"{type(exc).__name__}: {exc}"))
        print(f"  FAIL {fn.__name__}: {type(exc).__name__}: {exc}")


# ---------------------------------------------------------------------------
# fake SDK client (the facade's client_factory seam)
# ---------------------------------------------------------------------------

class FakeRef:
    def __init__(self, name="dsh-fake", sandbox_id="sb-1"):
        self.id = sandbox_id
        self.name = name
        self.workspace = "default"
        self.status = SimpleNamespace(phase=2, current_policy_version=7)
        self.labels = {"k": "v"}


class FakeExecResult:
    exit_code = 0
    stdout = b"hello-out"
    stderr = b"hello-err"


class FakeInnerStub:
    def GetSandboxLogs(self, request, timeout=None):
        return SimpleNamespace(logs=[])

    def UpdateConfig(self, request, timeout=None):
        assert request.name == "dsh-fake"
        return SimpleNamespace(version=3, policy_hash="abc123")


class FakeSandboxClient:
    def __init__(self):
        self.calls = []
        self._stub = FakeInnerStub()

    def health(self):
        return object()

    def create(self, *, workspace, name, spec):
        self.calls.append(("create", workspace, name, spec))
        return FakeRef(name=name or "dsh-fake")

    def get(self, name, workspace=None):
        self.calls.append(("get", name, workspace))
        return FakeRef(name=name)

    def wait_ready(self, name, *, workspace, timeout_seconds=None):
        self.calls.append(("wait_ready", name, workspace, timeout_seconds))
        return FakeRef(name=name)

    def exec(self, sandbox_id, command, *, workdir=None, env=None, stdin=None,
             timeout_seconds=None):
        self.calls.append(("exec", sandbox_id, command, workdir, env, stdin,
                           timeout_seconds))
        return FakeExecResult()

    def delete(self, name, workspace=None):
        self.calls.append(("delete", name, workspace))
        return True

    def list_for_all_workspaces(self, limit=None):
        return [FakeRef(name="dsh-a"), FakeRef(name="dsh-b")]


class FakeRouteClient:
    ROUTE = SimpleNamespace(provider_name="prov-x", model_id="model-y",
                            version=4)

    def __init__(self):
        self.last = None

    def get_route(self, *, workspace):
        return self.ROUTE

    def set_route(self, *, workspace, provider_name, model_id, no_verify=False):
        self.last = (workspace, provider_name, model_id, no_verify)
        return self.ROUTE


class FakeAdminStub:
    # (workspace, sandbox, service) -> ServiceEndpointResponse-like
    SERVICES: Dict[Tuple[str, str, str], SimpleNamespace] = {}

    def ListProviders(self, request, timeout=None):
        prov = SimpleNamespace(metadata=SimpleNamespace(name="prov-x"),
                               type="openai",
                               config={"OPENAI_BASE_URL": "http://x/v1"})
        return SimpleNamespace(providers=[prov])

    def UpdateProvider(self, request, timeout=None):
        return SimpleNamespace()

    def CreateProvider(self, request, timeout=None):
        raise AssertionError("provider exists; must Update, not Create")

    @staticmethod
    def _svc_response(workspace, sandbox, service, target_port, domain):
        ep = SimpleNamespace(sandbox_id="sb-x", sandbox_name=sandbox,
                             service_name=service,
                             target_port=target_port, domain=domain)
        url = f"http://{workspace}--{sandbox}--{service}.gw.test:8080/"
        return SimpleNamespace(endpoint=ep, url=url)

    def ExposeService(self, request, timeout=None):
        key = (request.workspace, request.sandbox, request.service)
        self.SERVICES[key] = self._svc_response(
            request.workspace, request.sandbox, request.service,
            request.target_port, request.domain)
        return self.SERVICES[key]

    def ListServices(self, request, timeout=None):
        out = [resp for (ws, sb, _name), resp in sorted(self.SERVICES.items())
               if request.all_workspaces or (ws == request.workspace
                                             and sb == request.sandbox)]
        return SimpleNamespace(services=out)

    def DeleteService(self, request, timeout=None):
        key = (request.workspace, request.sandbox, request.service)
        return SimpleNamespace(deleted=self.SERVICES.pop(key, None) is not None)


FAKE = FakeSandboxClient()
ROUTES_FAKE = FakeRouteClient()


def make_app(token_env):
    """Fresh HTTP server + request helper bound to the fake SDK."""
    if token_env is None:
        os.environ.pop("OPENSHELL_MANAGER_TOKEN", None)
    else:
        os.environ["OPENSHELL_MANAGER_TOKEN"] = token_env
    # Isolate from the deploy-state config.json/.token: an empty config file
    # keeps "token_env=None" cases auth-disabled regardless of what the real
    # deployment configured via tokenFile (config falls back to it).
    cfg = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    cfg.write("{}")
    cfg.close()
    os.environ["OPENSHELL_MANAGER_CONFIG"] = cfg.name
    config._config_cache = None

    facade = GatewayFacade(client_factory=lambda: FAKE)
    facade._route_client = lambda: ROUTES_FAKE  # test seam
    gw.pb_grpc_stub = lambda client: FakeAdminStub()
    FakeAdminStub.SERVICES.clear()
    api.facade = facade  # ADR-174: FastAPI 架构（http_api 为旧实现参照）

    app = api.create_app()
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=0,
                                           log_level="warning", access_log=False))
    threading.Thread(target=server.run, daemon=True).start()
    while not server.started:
        time.sleep(0.02)
    port = server.servers[0].sockets[0].getsockname()[1]
    base = f"http://127.0.0.1:{port}"

    class _ServerShim:  # 兼容旧 ThreadingHTTPServer 句柄语义（server_address/shutdown）
        server_address = ("127.0.0.1", port)
        shutdown = staticmethod(lambda: setattr(server, "should_exit", True))
    server = _ServerShim()

    def request(method, path, body=None, token=None, raw=None, ctype=None):
        req = urllib.request.Request(base + path, method=method)
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        data = None
        if raw is not None:
            data = raw
            req.add_header("Content-Type", ctype or "application/json")
        elif body is not None:
            data = json.dumps(body).encode()
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, data=data, timeout=10) as resp:
                return resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                return exc.code, json.loads(raw)
            except Exception:  # noqa: BLE001
                return exc.code, raw

    return server, request


# ---------------------------------------------------------------------------
# cases
# ---------------------------------------------------------------------------

def test_healthz_open_no_auth():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET", "/healthz")
        assert status == 200 and payload["ok"] is True, payload
    finally:
        server.shutdown()


def test_route_and_providers_auth_disabled():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("PUT", "/api/v1/inference/route",
                              {"workspace": "default", "provider": "prov-x",
                               "model": "model-y", "no_verify": True})
        assert status == 200 and payload["provider"] == "prov-x", payload
        assert ROUTES_FAKE.last == ("default", "prov-x", "model-y", True)

        status, payload = req("GET",
                              "/api/v1/inference/route?workspace=default")
        assert status == 200 and payload == {
            "provider": "prov-x", "model": "model-y", "version": 4}, payload

        status, payload = req("GET",
                              "/api/v1/inference/providers?workspace=default")
        assert status == 200 and payload["providers"] == ["prov-x"], payload

        status, payload = req("PUT", "/api/v1/inference/providers",
                              {"workspace": "default", "name": "prov-x",
                               "type": "openai",
                               "credentials": {"OPENAI_API_KEY": "sk-test"},
                               "config": {"OPENAI_BASE_URL": "http://x/v1"}})
        assert status == 200 and payload == {"name": "prov-x",
                                             "created": False}, payload
    finally:
        server.shutdown()


def test_sandbox_lifecycle_and_exec():
    server, req = make_app(token_env=None)
    try:
        spec = {"providers": ["prov-x"], "environment": {"A": "B"}}
        status, payload = req("POST", "/api/v1/sandboxes",
                              {"workspace": "default", "name": "dsh-fake",
                               "spec": spec})
        assert status == 200 and payload["id"] == "sb-1", payload
        kind, ws, name, pb_spec = FAKE.calls[-1]
        assert (kind, ws, name) == ("create", "default", "dsh-fake")
        assert list(pb_spec.providers) == ["prov-x"], pb_spec

        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1",
                               "command": ["/bin/echo", "hi"],
                               "env": {"K": "V"},
                               "stdin_b64": base64.b64encode(
                                   b"in-bytes").decode(),
                               "timeout_seconds": 30})
        assert status == 200 and payload["stdout"] == "hello-out", payload
        call = FAKE.calls[-1]
        assert call[1] == "sb-1" and call[2] == ["/bin/echo", "hi"]
        assert call[5] == b"in-bytes" and call[6] == 30

        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake?workspace=default")
        assert status == 200 and payload["phase"] == 2, payload

        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/wait-ready",
                              {"workspace": "default", "timeout_seconds": 12.5})
        assert status == 200 and payload["name"] == "dsh-fake", payload
        assert FAKE.calls[-1][3] == 12.5

        status, payload = req("GET", "/api/v1/sandboxes?limit=50")
        assert status == 200 and len(payload["sandboxes"]) == 2, payload

        status, payload = req("DELETE",
                              "/api/v1/sandboxes/dsh-fake?workspace=default")
        assert status == 200 and payload["deleted"] is True, payload

        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake/logs"
                              "?workspace=default&lines=99")
        assert status == 200 and payload["logs"] == [], payload

        status, payload = req("POST",
                              "/api/v1/sandboxes/dsh-fake/update-config",
                              {"workspace": "default", "policy": {"version": 1}})
        assert status == 200 and payload == {"version": 3,
                                             "policy_hash": "abc123"}, payload
    finally:
        server.shutdown()


def test_error_mapping():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET", "/api/v1/nope")
        assert status == 404, payload
        status, payload = req("PUT", "/api/v1/inference/route",
                              {"workspace": "default"})
        assert status == 400 and "provider" in payload["error"], payload
        status, payload = req("GET", "/api/v1/sandboxes/x?workspace=")
        assert status == 400 and "workspace" in payload["error"], payload
    finally:
        server.shutdown()


def test_provider_detail_and_404():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET",
                              "/api/v1/inference/providers/prov-x"
                              "?workspace=default")
        assert status == 200 and payload["type"] == "openai", payload
        assert payload["config"]["OPENAI_BASE_URL"] == "http://x/v1", payload
        assert "credentials" not in payload, payload  # masked by omission
        status, payload = req("GET",
                              "/api/v1/inference/providers/nope"
                              "?workspace=default")
        assert status == 404, payload
    finally:
        server.shutdown()


def test_ref_projection_carries_phase_name_and_conditions():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake?workspace=default")
        assert status == 200, payload
        assert "phase_name" in payload and "conditions" in payload, payload
        assert payload["phase_name"].startswith("SANDBOX_PHASE_"), payload
        assert isinstance(payload["conditions"], list), payload
    finally:
        server.shutdown()


def test_token_auth_enforced():
    server, req = make_app(token_env="secret-token")
    try:
        status, _ = req("GET", "/api/v1/inference/route?workspace=default")
        assert status == 401, status
        status, payload = req("GET",
                              "/api/v1/inference/route?workspace=default",
                              token="wrong")
        assert status == 401, payload
        status, payload = req("GET",
                              "/api/v1/inference/route?workspace=default",
                              token="secret-token")
        assert status == 200 and payload["provider"] == "prov-x", payload
        status, payload = req("GET", "/healthz")  # healthz stays open
        assert status == 200, payload
    finally:
        server.shutdown()


class FailingClient(FakeSandboxClient):
    def get(self, name, workspace=None):
        raise LookupError(f"sandbox '{name}' not found")

    def health(self):
        raise RuntimeError("gateway unreachable")


def test_gateway_health_endpoint():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET", "/api/v1/gateway/health")
        assert status == 200 and payload["ok"] is True, payload
        assert payload["endpoint"], payload
    finally:
        server.shutdown()


def test_upstream_error_mapping():
    server, req = make_app(token_env=None)
    api.facade = GatewayFacade(client_factory=lambda: FailingClient())
    try:
        status, payload = req("GET", "/api/v1/sandboxes/nope?workspace=default")
        assert status == 404 and "not found" in payload["error"], payload
        status, payload = req("GET", "/api/v1/gateway/health")
        assert status == 502 and "RuntimeError" in payload["error"], payload
    finally:
        server.shutdown()


def test_body_parsing_errors():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("POST", "/api/v1/sandboxes", raw=b"{not-json")
        assert status == 400 and "invalid JSON" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes", raw=b"[1,2]")
        assert status == 400 and "object" in payload["error"], payload
        # oversized declared Content-Length must 413 before the body is read
        port = server.server_address[1]
        with socket.create_connection(("127.0.0.1", port), timeout=5) as s:
            s.sendall(b"POST /api/v1/sandboxes HTTP/1.1\r\nHost: t\r\n"
                      b"Content-Length: 99999999\r\n\r\n")
            first = s.recv(4096)
        assert first.startswith(b"HTTP/1.1 413"), first[:60]
    finally:
        server.shutdown()


def test_config_validate_bind_discipline():
    saved = {k: os.environ.get(k) for k in
             ("OPENSHELL_MANAGER_BIND", "OPENSHELL_MANAGER_TOKEN")}
    try:
        os.environ.pop("OPENSHELL_MANAGER_TOKEN", None)
        os.environ["OPENSHELL_MANAGER_BIND"] = "0.0.0.0"
        try:
            config.validate()
            raise AssertionError("non-loopback bind without token must refuse")
        except RuntimeError as exc:
            assert "refusing to bind" in str(exc), exc
        os.environ["OPENSHELL_MANAGER_TOKEN"] = "t"
        config.validate()  # non-loopback WITH token: allowed
        os.environ["OPENSHELL_MANAGER_BIND"] = "127.0.0.1"
        os.environ.pop("OPENSHELL_MANAGER_TOKEN", None)
        config.validate()  # loopback without token: allowed
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def test_token_priority_env_over_tokenfile():
    token_file = tempfile.NamedTemporaryFile("w", suffix=".token", delete=False)
    token_file.write("file-token")
    token_file.close()
    cfg = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    cfg.write(json.dumps({"tokenFile": token_file.name}))
    cfg.close()
    saved = {k: os.environ.get(k) for k in
             ("OPENSHELL_MANAGER_TOKEN", "OPENSHELL_MANAGER_CONFIG")}
    try:
        os.environ["OPENSHELL_MANAGER_CONFIG"] = cfg.name
        config._config_cache = None
        os.environ.pop("OPENSHELL_MANAGER_TOKEN", None)
        assert config.manager_token() == "file-token", config.manager_token()
        os.environ["OPENSHELL_MANAGER_TOKEN"] = "env-token"
        assert config.manager_token() == "env-token", config.manager_token()
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        config._config_cache = None


def test_service_expose_list_delete():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "demo",
                               "target_port": 8123, "domain": False})
        assert status == 200, payload
        assert payload["url"] == \
            "http://default--dsh-fake--demo.gw.test:8080/", payload
        assert payload["name"] == "demo", payload
        assert payload["target_port"] == 8123, payload
        assert payload["sandbox_name"] == "dsh-fake", payload

        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake/services"
                              "?workspace=default")
        assert status == 200, payload
        assert [s["name"] for s in payload["services"]] == ["demo"], payload

        # re-expose same name: update in place, not duplicated
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "demo",
                               "target_port": 9999})
        assert status == 200 and payload["target_port"] == 9999, payload
        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake/services"
                              "?workspace=default")
        assert len(payload["services"]) == 1, payload

        status, payload = req("DELETE",
                              "/api/v1/sandboxes/dsh-fake/services/demo"
                              "?workspace=default")
        assert status == 200 and payload["deleted"] is True, payload
        status, payload = req("DELETE",
                              "/api/v1/sandboxes/dsh-fake/services/demo"
                              "?workspace=default")
        assert status == 200 and payload["deleted"] is False, payload
        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake/services"
                              "?workspace=default")
        assert payload["services"] == [], payload
    finally:
        server.shutdown()


def test_service_list_requires_workspace():
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET", "/api/v1/sandboxes/dsh-fake/services")
        assert status == 400 and "workspace" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "demo"})
        assert status == 400 and "target_port" in payload["error"], payload
    finally:
        server.shutdown()


def test_sandbox_file_upload():
    server, req = make_app(token_env=None)
    try:
        content = bytes(range(256)) * 4  # 1KiB binary payload
        calls_base = len(FAKE.calls)  # FAKE.calls accumulates across cases
        body, ctype = multipart_body({"path": "/tmp/up/bin.dat", "mode": "0755"},
                                     file_content=content)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200, payload
        assert payload == {"path": "/tmp/up/bin.dat", "bytes": 1024,
                           "chunks": 1}, payload

        execs = [c for c in FAKE.calls[calls_base:] if c[0] == "exec"]
        # mkdir 父目录（先于分块写）+ 1 chunk write + finalize mv + chmod
        assert len(execs) == 4, FAKE.calls[-5:]
        mkdir_cmd, chunk, finalize, chmod_cmd = execs
        assert mkdir_cmd[2] == ["/bin/sh", "-c",
                                'mkdir -p "$(dirname /tmp/up/bin.dat)"'], mkdir_cmd
        assert chunk[1] == "sb-1" and chunk[5] == base64.b64encode(content), chunk
        assert "base64 -d > " in chunk[2][2] and ".part" in chunk[2][2]
        assert finalize[2] == ["/bin/sh", "-c",
                               "mv /tmp/up/bin.dat.part /tmp/up/bin.dat"], finalize
        assert chmod_cmd[2] == ["chmod", "0755", "/tmp/up/bin.dat"], chmod_cmd

        # error mapping
        body, ctype = multipart_body({"path": "relative/path"}, file_content=b"hi")
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 400 and "absolute" in payload["error"], payload
        body, ctype = multipart_body({"path": "/tmp/x", "mode": "rwx"},
                                     file_content=b"hi")
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 400 and "mode" in payload["error"], payload

        # JSON is no longer accepted on this endpoint
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              {"path": "/tmp/x", "content_b64": "aGk="})
        assert status == 415 and "multipart" in payload["error"], payload
    finally:
        server.shutdown()


def multipart_body(fields, file_name="file", file_filename="payload.bin",
                   file_content=b"", boundary="----openshelltest"):
    """Build a multipart/form-data body like curl -F / browser forms do.

    ``file_content=None`` omits the file part entirely (missing-file error).
    """
    parts = []
    for name, value in fields.items():
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"'
            f"\r\n\r\n{value}\r\n".encode())
    if file_content is not None:
        parts.append(
            (f'--{boundary}\r\nContent-Disposition: form-data; '
             f'name="{file_name}"; filename="{file_filename}"\r\n'
             f"Content-Type: application/octet-stream\r\n\r\n").encode()
            + file_content + b"\r\n")
    parts.append(f"--{boundary}--\r\n".encode())
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


def test_sandbox_file_upload_multipart():
    server, req = make_app(token_env=None)
    try:
        content = bytes(range(256)) * 8  # 2KiB binary incl. NUL/CR/LF bytes
        body, ctype = multipart_body(
            {"path": "/tmp/up/binary blob.dat", "mode": "0755"},
            file_content=content)
        calls_base = len(FAKE.calls)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200, payload
        assert payload == {"path": "/tmp/up/binary blob.dat",
                           "bytes": 2048, "chunks": 1}, payload
        chunk = [c for c in FAKE.calls[calls_base:]
                 if c[0] == "exec" and "base64 -d" in c[2][2]][0]
        assert chunk[5] == base64.b64encode(content), "binary payload must survive multipart"

        # empty file via multipart
        body, ctype = multipart_body({"path": "/tmp/empty"},
                                     file_content=b"")
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200 and payload["bytes"] == 0, payload

        # missing file part / missing path field
        body, ctype = multipart_body({"path": "/tmp/x"}, file_content=None)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 400 and "file" in payload["error"], payload
        body, ctype = multipart_body({}, file_content=b"hi")
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 400 and "absolute" in payload["error"], payload
    finally:
        server.shutdown()


def test_sandbox_file_upload_chunking():
    server, req = make_app(token_env=None)
    try:
        facade = api.facade
        facade.UPLOAD_CHUNK_BYTES = 4  # force many tiny chunks
        content = b"abcdefgh"  # 3 字节对齐切片（take=3）→ 3 chunks
        calls_base = len(FAKE.calls)
        body, ctype = multipart_body({"path": "/tmp/multi.bin"},
                                     file_content=content)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200 and payload["chunks"] == 3, payload
        execs = [c for c in FAKE.calls[calls_base:] if c[0] == "exec"]
        writes = [c for c in execs if "base64 -d" in c[2][2]]
        assert base64.b64decode(b"".join(w[5] for w in writes)) == content, writes
        assert ">" in writes[0][2][2] and ">>" in writes[1][2][2], writes
    finally:
        server.shutdown()


def test_multipart_rejected_on_other_endpoints():
    """Only /files speaks multipart; JSON endpoints keep original behavior."""
    server, req = make_app(token_env=None)
    try:
        body, ctype = multipart_body({"workspace": "default"},
                                     file_content=b"x")
        status, payload = req("POST", "/api/v1/sandboxes", raw=body, ctype=ctype)
        assert status == 400 and "invalid JSON" in payload["error"], payload
        status, payload = req("PUT", "/api/v1/inference/route",
                              raw=body, ctype=ctype)
        assert status == 400 and "invalid JSON" in payload["error"], payload
    finally:
        server.shutdown()


def test_upload_large_streaming_file():
    """A 32MiB upload — 4x the old 8MiB JSON cap — must stream through."""
    server, req = make_app(token_env=None)
    try:
        import random
        random.seed(42)
        content = random.randbytes(32 * 1024 * 1024)
        body, ctype = multipart_body({"path": "/tmp/big.bin"},
                                     file_content=content)
        calls_base = len(FAKE.calls)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200, payload
        assert payload == {"path": "/tmp/big.bin", "bytes": len(content),
                           "chunks": 46}, payload  # ceil(32MiB/720KiB)，720KiB 恰为 3 的倍数
        writes = [c for c in FAKE.calls[calls_base:]
                  if c[0] == "exec" and "base64 -d" in c[2][2]]
        assert base64.b64decode(b"".join(w[5] for w in writes)) == content, "content corrupted"
        # every write chunk respects the 1MiB gateway receive ceiling
        # (720KiB raw → 960KiB base64 text; 实测网关上限 1MiB, 2026-09-06)
        assert all(len(w[5]) <= 720 * 1024 // 3 * 4 for w in writes), \
            [len(w[5]) for w in writes]  # stdin 为 base64 文本，上限按编码后口径
    finally:
        server.shutdown()


def test_upload_size_limit_enforced():
    server, req = make_app(token_env=None)
    saved = {k: os.environ.get(k) for k in ("OPENSHELL_MANAGER_MAX_UPLOAD_BYTES",)}
    try:
        os.environ["OPENSHELL_MANAGER_MAX_UPLOAD_BYTES"] = "1024"
        body, ctype = multipart_body({"path": "/tmp/x"}, file_content=b"z" * 4096)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 413 and "max_upload_bytes" in payload["error"], payload

        # under the limit passes
        body, ctype = multipart_body({"path": "/tmp/x"}, file_content=b"z" * 512)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200 and payload["bytes"] == 512, payload
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        server.shutdown()


def test_upload_requires_content_length():
    """No Content-Length (would-be chunked upload) → 411, connection closed."""
    server, req = make_app(token_env=None)
    try:
        port = server.server_address[1]
        with socket.create_connection(("127.0.0.1", port), timeout=5) as s:
            s.sendall(b"POST /api/v1/sandboxes/sb-1/files HTTP/1.1\r\nHost: t\r\n"
                      b"Content-Type: multipart/form-data; boundary=b\r\n\r\n")
            first = s.recv(4096)
        assert first.startswith(b"HTTP/1.1 411"), first[:60]
    finally:
        server.shutdown()


def test_upload_embedded_boundary_bytes():
    """Payload containing near-miss boundary byte runs must pass through."""
    server, req = make_app(token_env=None)
    try:
        content = (b"--openshelltes\r\nx" + b"\r\n--openshelltest"
                   + b"\r\n--openshelltestz" + bytes(range(256)))
        body, ctype = multipart_body({"path": "/tmp/tricky.bin"},
                                     file_content=content)
        calls_base = len(FAKE.calls)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200 and payload["bytes"] == len(content), payload
        writes = [c for c in FAKE.calls[calls_base:]
                  if c[0] == "exec" and "base64 -d" in c[2][2]]
        assert base64.b64decode(b"".join(w[5] for w in writes)) == content, "boundary bytes lost"
    finally:
        server.shutdown()


def test_upload_spaced_parent_dir_quoted_and_created_first():
    """含空格的父目录：mkdir 必须先于分块写、$(dirname) 必须带引号。

    修复前两个缺陷：(a) mkdir 排在 finalize（分块写之后），父目录不存在时
    第一个分块就写盘失败，mkdir 形同虚设；(b) `$(dirname …)` 未加引号，
    含空格/通配符的目录名被词拆分，mkdir 造错目录、mv 失败。
    """
    server, req = make_app(token_env=None)
    try:
        body, ctype = multipart_body({"path": "/tmp/up/new dir/f.txt"},
                                     file_content=b"hi")
        calls_base = len(FAKE.calls)
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 200 and payload["bytes"] == 2, payload
        execs = [c for c in FAKE.calls[calls_base:] if c[0] == "exec"]
        mkdir_cmd, chunk, finalize = execs[0], execs[1], execs[2]
        assert mkdir_cmd[2] == ["/bin/sh", "-c",
                                'mkdir -p "$(dirname \'/tmp/up/new dir/f.txt\')"'], mkdir_cmd
        assert "base64 -d" in chunk[2][2], chunk
        assert finalize[2] == ["/bin/sh", "-c",
                               "mv '/tmp/up/new dir/f.txt'.part '/tmp/up/new dir/f.txt'"], finalize
    finally:
        server.shutdown()


def test_exec_rejects_non_list_command_and_bad_stdin():
    """command 裸字符串曾被 list() 拆成单字符数组静默执行垃圾命令；
    坏 stdin_b64 曾泄漏为 502。两者都必须 400 且不触达网关。"""
    server, req = make_app(token_env=None)
    try:
        calls_base = len(FAKE.calls)
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1", "command": "echo hi"})
        assert status == 400 and "list of strings" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1", "command": ["ok", 1]})
        assert status == 400 and "list of strings" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1", "command": ["x"],
                               "stdin_b64": "!!not-base64!!"})
        assert status == 400 and "stdin_b64" in payload["error"], payload
        assert [c for c in FAKE.calls[calls_base:] if c[0] == "exec"] == [], \
            "invalid exec input must never reach the gateway facade"
    finally:
        server.shutdown()


def test_malformed_numeric_params_map_400():
    """非法数值参数是客户端错误：曾一律泄漏为 502（上游失败口径）。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET", "/api/v1/sandboxes?limit=abc")
        assert status == 400 and "limit" in payload["error"], payload
        status, payload = req("GET",
                              "/api/v1/sandboxes/x/logs"
                              "?workspace=default&lines=abc")
        assert status == 400 and "lines" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/x/wait-ready",
                              {"workspace": "default",
                               "timeout_seconds": "abc"})
        assert status == 400 and "timeout_seconds" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/x/services",
                              {"workspace": "default", "service": "s",
                               "target_port": "abc"})
        assert status == 400 and "target_port" in payload["error"], payload
        status, payload = req("GET",
                              "/api/v1/sandboxes/x/services"
                              "?workspace=default&offset=abc")
        assert status == 400 and "offset" in payload["error"], payload
    finally:
        server.shutdown()


def test_invalid_spec_and_policy_map_400():
    """未知 spec/policy 字段（protobuf ParseError）必须是 400，曾为 502
    并把 protobuf 多行报文当上游错误暴露。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("POST", "/api/v1/sandboxes",
                              {"workspace": "default",
                               "spec": {"no_such_field": 1}})
        assert status == 400 and "invalid spec" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/x/update-config",
                              {"workspace": "default",
                               "policy": {"no_such_field": 1}})
        assert status == 400 and "invalid policy" in payload["error"], payload
    finally:
        server.shutdown()


if __name__ == "__main__":
    # Same resolution the service itself uses at runtime (env > config.json
    # > own libs tree > engine legacy path). Probing the engine checkout
    # directly went stale when the SDK moved into this repo's libs/.
    try:
        libs = config.openshell_lib_path()
    except RuntimeError as exc:
        print(f"SKIP: {exc} (libs optional discipline)")
        sys.exit(0)
    sys.path.insert(0, str(libs))
    print("== openshell-manager HTTP contract tests ==")
    for fn in list(globals().values()):
        if callable(fn) and getattr(fn, "__name__", "").startswith("test_"):
            run_case(fn)
    failed = [r for r in RESULTS if not r[1]]
    print(f"Manager contract: {len(RESULTS) - len(failed)}/{len(RESULTS)} passed")
    sys.exit(1 if failed else 0)

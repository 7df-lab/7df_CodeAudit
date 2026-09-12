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
import http.client
import json
import os
import socket
import sys
import tempfile
import threading
import time
import uvicorn
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from typing import Dict, Tuple

SERVICE_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(SERVICE_ROOT))

from openshell_manager import api  # noqa: E402 — 须先挂 SERVICE_ROOT（直跑模式）
from openshell_manager import config  # noqa: E402
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
    def __init__(self):
        self.last_logs_request = None

    def GetSandboxLogs(self, request, timeout=None):
        self.last_logs_request = request
        return SimpleNamespace(logs=[])

    def UpdateConfig(self, request, timeout=None):
        assert request.name == "dsh-fake"
        return SimpleNamespace(version=3, policy_hash="abc123")


class FakeSandboxClient:
    def __init__(self):
        self.calls = []
        self._stub = FakeInnerStub()
        # 可编程失败注入（.part 清理路径等负向用例）
        self.fail_exec_containing = None   # 子串：命中则该 exec 返回非零退出
        self.missing_names = set()         # get() 对这些名字抛 LookupError
        self.delete_result = True

    def health(self):
        return object()

    def create(self, *, workspace, name, spec):
        self.calls.append(("create", workspace, name, spec))
        return FakeRef(name=name or "dsh-fake")

    def get(self, name, workspace=None):
        self.calls.append(("get", name, workspace))
        if name in self.missing_names:
            raise LookupError(f"sandbox '{name}' not found")
        return FakeRef(name=name)

    def wait_ready(self, name, *, workspace, timeout_seconds=None):
        self.calls.append(("wait_ready", name, workspace, timeout_seconds))
        return FakeRef(name=name)

    def exec(self, sandbox_id, command, *, workdir=None, env=None, stdin=None,
             timeout_seconds=None):
        self.calls.append(("exec", sandbox_id, command, workdir, env, stdin,
                           timeout_seconds))
        if self.fail_exec_containing and \
                self.fail_exec_containing in " ".join(command):
            return SimpleNamespace(exit_code=1, stdout=b"", stderr=b"boom")
        return FakeExecResult()

    def delete(self, name, workspace=None):
        self.calls.append(("delete", name, workspace))
        return self.delete_result

    def list_for_all_workspaces(self, limit=None):
        return [FakeRef(name="dsh-a"), FakeRef(name="dsh-b")]


class FakeInferenceStub:
    """inference.v1.Inference 直连替身（gateway._inference_stub 缝）。
    SetInferenceRoute 回执带验证字段——回执透传是对外契约（3.16）。"""

    SET_RESPONSE = SimpleNamespace(
        provider_name="prov-x", model_id="model-y", version=5,
        validation_performed=True,
        validated_endpoints=[SimpleNamespace(url="https://gw/v1",
                                             protocol="https")])
    ROUTE = SimpleNamespace(provider_name="prov-x", model_id="model-y",
                            version=4)

    def __init__(self):
        self.last = None

    def GetInferenceRoute(self, request, timeout=None):
        self.last_get = request.workspace
        return self.ROUTE

    def SetInferenceRoute(self, request, timeout=None):
        self.last = (request.workspace, request.provider_name,
                     request.model_id, request.no_verify)
        return self.SET_RESPONSE


class FakeAdminStub:
    # (workspace, sandbox, service) -> ServiceEndpointResponse-like
    SERVICES: Dict[Tuple[str, str, str], SimpleNamespace] = {}
    # provider 名集合：让 upsert 的 Create/Update 双路径都可测
    PROVIDERS: set = {"prov-x"}

    def ListProviders(self, request, timeout=None):
        provs = [SimpleNamespace(metadata=SimpleNamespace(name=n),
                                 type="openai",
                                 config={"OPENAI_BASE_URL": "http://x/v1"})
                 for n in sorted(self.PROVIDERS)]
        return SimpleNamespace(providers=provs)

    def UpdateProvider(self, request, timeout=None):
        assert request.provider.metadata.name in self.PROVIDERS, \
            "provider missing; must Create, not Update"
        return SimpleNamespace()

    def CreateProvider(self, request, timeout=None):
        self.PROVIDERS.add(request.provider.metadata.name)
        return SimpleNamespace()

    def DeleteProvider(self, request, timeout=None):
        existed = request.name in self.PROVIDERS
        self.PROVIDERS.discard(request.name)
        return SimpleNamespace(deleted=existed)

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
INFERENCE_FAKE = FakeInferenceStub()


def make_app(token_env, client=None):
    """Fresh HTTP server + request helper bound to the fake SDK.

    ``client`` 可注入自定义假 SDK 客户端（缺省模块级 FAKE 单例）。
    每次装配复位可编程状态，避免用例间串扰。
    """
    client = client or FAKE
    client.fail_exec_containing = None
    client.missing_names = set()
    client.delete_result = True
    FakeAdminStub.PROVIDERS = {"prov-x"}
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
    config._token_cache = None  # B3-2：token 文件结果 5s 缓存逐用例复位

    facade = GatewayFacade(client_factory=lambda: client)
    facade._inference_stub = lambda: INFERENCE_FAKE  # test seam
    gw.pb_grpc_stub = lambda client: FakeAdminStub()
    FakeAdminStub.SERVICES.clear()
    api.facade = facade  # ADR-174: FastAPI 架构

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
        # SetInferenceRoute 回执的验证字段必须透传（连通性验证回执契约）
        assert payload["validation_performed"] is True, payload
        assert payload["validated_endpoints"] == [
            {"url": "https://gw/v1", "protocol": "https"}], payload
        assert INFERENCE_FAKE.last == ("default", "prov-x", "model-y", True)

        status, payload = req("GET",
                              "/api/v1/inference/route?workspace=default")
        assert status == 200 and payload == {
            "provider": "prov-x", "model": "model-y", "version": 4}, payload

        status, payload = req("GET",
                              "/api/v1/inference/providers?workspace=default")
        assert status == 200 and payload["providers"] == [
            {"name": "prov-x", "type": "openai",
             "config": {"OPENAI_BASE_URL": "http://x/v1"}}], payload

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
        # B3-3 同步收紧：南向未捕获异常走兜底处理器 → 500 + 通用文案
        # （原 502 让上游按"网关不可达"误重试/降级）
        status, payload = req("GET", "/api/v1/gateway/health")
        assert status == 500 and payload == {"error": "internal error"}, payload
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
        config._token_cache = None  # B3-2：token 文件结果缓存须逐断言复位
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
        config._token_cache = None


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
    """A 32MiB upload — beyond the 20MiB JSON cap — must stream through
    (the cap only bounds JSON endpoints; uploads are streamed)."""
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


def test_non_string_fields_map_400_not_502():
    """R10 字符串字段族（修复前为 502）：非字符串字段曾透传进 SDK/proto 层，
    proto 对 str 字段传非 str 抛 TypeError → 兜底 502（上游会按"网关不可达"
    重试/降级）。README 红线：客户端格式错误一律 400。"""
    server, req = make_app(token_env=None)
    try:
        cases = [
            ("POST", "/api/v1/sandboxes",
             {"workspace": 123}, "workspace"),
            ("POST", "/api/v1/sandboxes",
             {"workspace": "default", "name": 456}, "name"),
            ("POST", "/api/v1/sandboxes/x/wait-ready",
             {"workspace": 1}, "workspace"),
            ("POST", "/api/v1/sandboxes/x/update-config",
             {"workspace": 1, "policy": {"version": 1}}, "workspace"),
            ("POST", "/api/v1/sandboxes/x/services",
             {"workspace": 1, "service": "s", "target_port": 80}, "workspace"),
            ("POST", "/api/v1/sandboxes/x/services",
             {"workspace": "default", "service": 5, "target_port": 80}, "service"),
            ("PUT", "/api/v1/inference/route",
             {"workspace": "default", "provider": 7, "model": "m"}, "provider"),
            ("PUT", "/api/v1/inference/route",
             {"workspace": "default", "provider": "p", "model": 8}, "model"),
            ("PUT", "/api/v1/inference/providers",
             {"workspace": "default", "name": 9, "type": "openai"}, "name"),
            ("PUT", "/api/v1/inference/providers",
             {"workspace": "default", "name": "n", "type": 7}, "type"),
        ]
        for method, path, body, field in cases:
            status, payload = req(method, path, body)
            assert status == 400, (method, path, body, status, payload)
            assert field in payload["error"], (method, path, body, payload)
    finally:
        server.shutdown()


def test_exec_rejects_bad_env_workdir_and_timeout():
    """R10 同族：exec 的 env/workdir/timeout_seconds 类型不校验时透传 SDK
    （timeout 会进 gRPC 调用参数）→ 502 泄漏。必须 400 且不触达网关。"""
    server, req = make_app(token_env=None)
    try:
        calls_base = len(FAKE.calls)
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1", "command": ["x"],
                               "env": ["not-a-map"]})
        assert status == 400 and "env" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1", "command": ["x"],
                               "env": {"k": 1}})
        assert status == 400 and "env" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1", "command": ["x"],
                               "workdir": 17})
        assert status == 400 and "workdir" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": "sb-1", "command": ["x"],
                               "timeout_seconds": "abc"})
        assert status == 400 and "timeout_seconds" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/exec",
                              {"sandbox_id": ["not-str"], "command": ["x"]})
        assert status == 400 and "sandbox_id" in payload["error"], payload
        assert [c for c in FAKE.calls[calls_base:] if c[0] == "exec"] == [], \
            "invalid exec input must never reach the gateway facade"
    finally:
        server.shutdown()


def test_upload_to_missing_sandbox_maps_404():
    """上传端点 name→UUID 解析失败（沙箱不存在）= 客户端寻址错误 → 404。"""
    server, req = make_app(token_env=None)
    try:
        FAKE.missing_names.add("ghost")
        body, ctype = multipart_body({"path": "/tmp/x"}, file_content=b"hi")
        status, payload = req("POST", "/api/v1/sandboxes/ghost/files",
                              raw=body, ctype=ctype)
        assert status == 404 and "not found" in payload["error"], payload
    finally:
        server.shutdown()


def test_upload_failure_cleans_part_and_maps_5xx():
    """R1 族防线：chunk 写失败（远端非零退出）必须 rm -f .part 后上抛
    5xx——防失败上传留下 .part 残片/半写文件回归。（B3-3 同步收紧：兜底
    处理器 502 → 500 "internal error"。）"""
    server, req = make_app(token_env=None)
    try:
        FAKE.fail_exec_containing = "base64 -d"
        calls_base = len(FAKE.calls)
        body, ctype = multipart_body({"path": "/tmp/doomed/f.txt"},
                                     file_content=b"hi")
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=body, ctype=ctype)
        assert status == 500 and payload == {"error": "internal error"}, payload
        execs = [c for c in FAKE.calls[calls_base:] if c[0] == "exec"]
        assert execs[-1][2] == ["rm", "-f", "/tmp/doomed/f.txt.part"], execs[-1]
    finally:
        server.shutdown()


def test_upload_without_boundary_maps_400():
    """multipart Content-Type 缺 boundary 参数 = 客户端错误 → 400。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("POST", "/api/v1/sandboxes/sb-1/files",
                              raw=b"--x\r\ny\r\n--x--\r\n",
                              ctype="multipart/form-data")
        assert status == 400 and "boundary" in payload["error"], payload
    finally:
        server.shutdown()


def test_gateway_health_ok_false_when_sdk_answers_none():
    """SDK health() 返回 None = 网关应答异常（仍在线）→ 200 + ok:false。"""
    server, req = make_app(token_env=None, client=NoneHealthClient())
    try:
        status, payload = req("GET", "/api/v1/gateway/health")
        assert status == 200 and payload["ok"] is False, payload
    finally:
        server.shutdown()


class NoneHealthClient(FakeSandboxClient):
    def health(self):
        return None


def test_sandbox_delete_false_propagates():
    """网关返回删除失败必须如实透出 deleted:false（不许美化成 true）。"""
    server, req = make_app(token_env=None)
    try:
        FAKE.delete_result = False
        status, payload = req("DELETE",
                              "/api/v1/sandboxes/dsh-fake?workspace=default")
        assert status == 200 and payload["deleted"] is False, payload
    finally:
        server.shutdown()


def test_services_list_all_workspaces():
    """?all_workspaces=true 免 workspace 参数（跨工作区清单）。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "ws-a", "service": "demo",
                               "target_port": 8123})
        assert status == 200, payload
        status, payload = req(
            "GET", "/api/v1/sandboxes/dsh-fake/services?all_workspaces=true")
        assert status == 200, payload
        assert [s["name"] for s in payload["services"]] == ["demo"], payload
        # 大写变体同样接受（解析先 lower）
        status, payload = req(
            "GET", "/api/v1/sandboxes/dsh-fake/services?all_workspaces=TRUE")
        assert status == 200 and len(payload["services"]) == 1, payload
    finally:
        server.shutdown()


def test_provider_upsert_create_path():
    """upsert 不存在的 provider 必须走 CreateProvider（created:true）——
    此前假 stub 无 Create 路径，该分支零覆盖。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("PUT", "/api/v1/inference/providers",
                              {"workspace": "default", "name": "prov-new",
                               "type": "anthropic",
                               "credentials": {"API_KEY": "sk-x"},
                               "config": {"BASE_URL": "http://y/v1"}})
        assert status == 200 and payload == {"name": "prov-new",
                                             "created": True}, payload
        # 创建后可查详情，且凭据仍被屏蔽
        status, payload = req("GET",
                              "/api/v1/inference/providers/prov-new"
                              "?workspace=default")
        assert status == 200 and payload["name"] == "prov-new", payload
        assert "credentials" not in payload, payload
    finally:
        server.shutdown()


def test_provider_delete_roundtrip():
    """DELETE provider：删已存在 → {name, deleted:true} 且清单/详情立即可证
    消失；幂等口径与网关一致（不存在的名字 deleted:false）。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("PUT", "/api/v1/inference/providers",
                              {"workspace": "default", "name": "prov-del",
                               "type": "openai"})
        assert status == 200 and payload["created"] is True, payload

        status, payload = req("DELETE",
                              "/api/v1/inference/providers/prov-del"
                              "?workspace=default")
        assert status == 200 and payload == {"name": "prov-del",
                                             "deleted": True}, payload

        status, payload = req("GET",
                              "/api/v1/inference/providers?workspace=default")
        assert status == 200 and "prov-del" not in \
            [p["name"] for p in payload["providers"]], payload
        status, _ = req("GET",
                        "/api/v1/inference/providers/prov-del"
                        "?workspace=default")
        assert status == 404, "deleted provider must 404 on detail"

        status, payload = req("DELETE",
                              "/api/v1/inference/providers/prov-del"
                              "?workspace=default")
        assert status == 200 and payload["deleted"] is False, payload

        # 缺 workspace → 400；无 name 段（整表删）方法不允许 → 405
        status, payload = req("DELETE",
                              "/api/v1/inference/providers/prov-x")
        assert status == 400 and "workspace" in payload["error"], payload
        status, _ = req("DELETE",
                        "/api/v1/inference/providers?workspace=default")
        assert status == 405, "collection-level DELETE must not be routed"
    finally:
        server.shutdown()


def test_logs_request_params_passthrough():
    """lines/since_ms 必须透传到 GetSandboxLogsRequest。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake/logs"
                              "?workspace=default&lines=42&since_ms=77")
        assert status == 200 and payload["logs"] == [], payload
        sent = FAKE._stub.last_logs_request
        assert (sent.lines, sent.since_ms, sent.workspace) == (42, 77, "default"), sent
    finally:
        server.shutdown()


def test_logs_resolve_name_to_uuid():
    """B1-15：/logs 路由的沙箱 name 必须先解析成 UUID 再打 GetSandboxLogs。

    GetSandboxLogs 属 ExecSandbox 系 RPC，只认 sandbox_id=UUID（resolve_
    sandbox_id 文档口径）；原实现把路由 name 原样当 sandbox_id 传，网关侧
    必 NOT_FOUND。与 /files 同口径（ADR-173）：接口层收 name 自解析。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET",
                              "/api/v1/sandboxes/dsh-fake/logs"
                              "?workspace=default&lines=42&since_ms=77")
        assert status == 200 and payload["logs"] == [], payload
        sent = FAKE._stub.last_logs_request
        assert sent.sandbox_id == "sb-1", \
            f"logs 查询必须用解析后的 UUID，实际 sandbox_id={sent.sandbox_id!r}"
    finally:
        server.shutdown()


def test_chunked_json_body_not_dropped():
    """B1-13：Transfer-Encoding: chunked 的 JSON body 不得被
    Content-Length==0 短路分支丢弃（原实现静默当空对象 → 400 missing
    required field(s)，请求体凭空消失）。chunked 走流式读取。"""
    server, req = make_app(token_env=None)
    try:
        port = server.server_address[1]
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
        # body 为迭代器且未显式 Content-Length → http.client 自动走 chunked
        payload = iter([json.dumps({"sandbox_id": "sb-1",
                                    "command": ["/bin/echo", "hi"]}).encode()])
        conn.request("POST", "/api/v1/sandboxes/exec", body=payload,
                     headers={"Content-Type": "application/json"})
        resp = conn.getresponse()
        body = json.loads(resp.read())
        assert resp.status == 200, body
        assert body["stdout"] == "hello-out", body
        conn.close()
    finally:
        server.shutdown()


def test_boolean_string_fields_strict():
    """B1-12：布尔字段收 true/false 布尔或 "true"/"false" 字符串（字符串
    "false" 必须解析为 False——曾被 bool() 恒真打开），其余类型 400。"""
    server, req = make_app(token_env=None)
    try:
        # 字符串 "false" 合法且必须解析为 False（修复前 bool("false") 恒真）
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "bridge",
                               "target_port": 8080, "domain": "false"})
        assert status == 200, payload
        assert FakeAdminStub.SERVICES[("default", "dsh-fake", "bridge")] \
            .endpoint.domain is False, payload
        # 非布尔非字符串的垃圾类型 → 400（修复前恒真）
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "bridge",
                               "target_port": 8080, "domain": 1})
        assert status == 400 and "domain" in payload["error"], payload
        # 字符串 "true" 语义正确（domain 真被置真）
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "bridge",
                               "target_port": 8080, "domain": "true"})
        assert status == 200, payload
        assert FakeAdminStub.SERVICES[("default", "dsh-fake", "bridge")] \
            .endpoint.domain is True, payload
        # 布尔 false 原生通道不受影响
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "bridge",
                               "target_port": 8080, "domain": False})
        assert status == 200, payload
        assert FakeAdminStub.SERVICES[("default", "dsh-fake", "bridge")] \
            .endpoint.domain is False, payload
        # route_set 的 no_verify 同口径
        status, payload = req("PUT", "/api/v1/inference/route",
                              {"workspace": "default", "provider": "prov-x",
                               "model": "model-y", "no_verify": "false"})
        assert status == 200, payload
        assert INFERENCE_FAKE.last == ("default", "prov-x", "model-y", False), \
            INFERENCE_FAKE.last
        status, payload = req("PUT", "/api/v1/inference/route",
                              {"workspace": "default", "provider": "prov-x",
                               "model": "model-y", "no_verify": "true"})
        assert status == 200, payload
        assert INFERENCE_FAKE.last == ("default", "prov-x", "model-y", True), \
            INFERENCE_FAKE.last
        status, payload = req("PUT", "/api/v1/inference/route",
                              {"workspace": "default", "provider": "prov-x",
                               "model": "model-y", "no_verify": 0})
        assert status == 400 and "no_verify" in payload["error"], payload
    finally:
        server.shutdown()


def test_upload_spool_closed_on_stream_error():
    """B1-12：spool 文件全程 try/finally 关闭——收流中途异常（客户端断连，
    request.stream() 抛出）也必须关闭 spool，不得泄漏临时文件。"""
    import asyncio
    opened = []

    class FakeSpool:
        def __init__(self, *a, **k):
            opened.append(self)
            self.closed = False

        def write(self, chunk):
            pass

        def seek(self, *a):
            pass

        def close(self):
            self.closed = True

    async def broken_stream():
        raise RuntimeError("client disconnected")
        yield b""  # pragma: no cover — 仅为异步生成器形态

    orig = api.tempfile.SpooledTemporaryFile
    api.tempfile.SpooledTemporaryFile = FakeSpool
    server, _req = make_app(token_env=None)  # 借其隔离 config 环境
    try:
        from types import SimpleNamespace as NS
        request = NS(
            headers={"Content-Type": "multipart/form-data; boundary=xyz",
                     "Content-Length": "10"},
            stream=lambda: broken_stream(),
            url=NS(query=""),
        )
        try:
            asyncio.run(api._handle_upload("dsh-fake", request))
            raise AssertionError("stream error must propagate")
        except RuntimeError:
            pass
        assert opened and opened[-1].closed, \
            "spool file must be closed on stream error (try/finally)"
    finally:
        api.tempfile.SpooledTemporaryFile = orig
        server.shutdown()


def test_method_not_allowed_error_contract():
    """405 也是 {"error": …} 单键契约（不允许 FastAPI 默认 detail 形态泄漏）。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("DELETE", "/healthz")
        assert status == 405 and set(payload) == {"error"}, payload
    finally:
        server.shutdown()


def test_404_route_message_contract():
    """未知路由文案锁定 `no route for METHOD /path`；尾斜杠归一。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("GET", "/api/v1/nope")
        assert status == 404, payload
        assert payload["error"] == "no route for GET /api/v1/nope", payload
        status, payload = req("GET", "/api/v1/nope/")
        assert status == 404 and \
            payload["error"] == "no route for GET /api/v1/nope", payload
    finally:
        server.shutdown()


def test_auth_rejects_lowercase_bearer_scheme():
    """Bearer 前缀大小写敏感：小写 scheme 必须拒（严格前缀契约）。"""
    server, req = make_app(token_env="secret-token")
    try:
        port = server.server_address[1]
        r = urllib.request.Request(
            f"http://127.0.0.1:{port}/api/v1/inference/route?workspace=default")
        r.add_header("Authorization", "bearer secret-token")
        try:
            urllib.request.urlopen(r, timeout=5)
            raise AssertionError("lowercase bearer must be rejected")
        except urllib.error.HTTPError as exc:
            assert exc.code == 401, exc.code
    finally:
        server.shutdown()


# ---------------------------------------------------------------------------
# B3 审计修复批（fix-plan-2026-09-11 §5，先红后修）
# ---------------------------------------------------------------------------

class SlowExecClient(FakeSandboxClient):
    """模拟慢南向调用（gRPC 在途）：exec 阻塞 2 秒。"""

    def exec(self, sandbox_id, command, *, workdir=None, env=None, stdin=None,
             timeout_seconds=None):
        time.sleep(2)
        return FakeExecResult()


def test_slow_exec_does_not_block_healthz():
    """B3-1：async 端点必须在 event loop 外跑南向调用——慢 exec 在途时
    /healthz 仍 <0.5s 响应。修复前红：exec 裸调 facade（同步阻塞）劫持
    整个事件循环，探活/一切并发请求排队 2s。"""
    server, req = make_app(token_env=None, client=SlowExecClient())
    try:
        done = threading.Event()

        def do_exec():
            status, payload = req("POST", "/api/v1/sandboxes/exec",
                                  {"sandbox_id": "sb-1",
                                   "command": ["/bin/sleep", "2"]})
            assert status == 200, payload
            done.set()

        threading.Thread(target=do_exec, daemon=True).start()
        time.sleep(0.5)  # 等 exec 确认进入在途（南向慢调用占线）
        t0 = time.monotonic()
        status, payload = req("GET", "/healthz")
        dt = time.monotonic() - t0
        assert status == 200, payload
        assert dt < 0.5, \
            f"/healthz blocked {dt:.2f}s by in-flight slow exec " \
            "(async routes must offload facade calls via run_in_threadpool)"
        assert done.wait(timeout=10), "slow exec must still complete"
    finally:
        server.shutdown()


def test_wait_ready_timeout_server_side_cap():
    """B3-1：wait_ready timeout_seconds 服务端上限——缺省 300s、硬上限
    600s，超限 = 客户端错误 400（客户端曾可传 1e9 让南向连接无限占用）。"""
    server, req = make_app(token_env=None)
    try:
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/wait-ready",
                              {"workspace": "default",
                               "timeout_seconds": 1e9})
        assert status == 400 and "timeout_seconds" in payload["error"], payload
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/wait-ready",
                              {"workspace": "default", "timeout_seconds": 601})
        assert status == 400, payload
        # 边界含 600；缺省 300
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/wait-ready",
                              {"workspace": "default", "timeout_seconds": 600})
        assert status == 200, payload
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/wait-ready",
                              {"workspace": "default"})
        assert status == 200 and FAKE.calls[-1][3] == 300, payload
    finally:
        server.shutdown()


def test_token_file_auth_and_fail_closed():
    """B3-2：tokenFile 已配置但读失败（EACCES/EIO 等 OSError）=
    fail-closed——受保护路由 503，不再静默放行（修复前异常被吞 token 变
    空 → 鉴权失效）；/healthz 探活豁免不受影响；文件可读时鉴权回归不破。

    读失败复现：常规环境 chmod 000（EACCES）；root 开发机上权限位对
    root 无效，改用 tokenFile 指向目录（IsADirectoryError，同一 OSError
    → TokenFileError fail-closed 通道）。"""
    token_file = tempfile.NamedTemporaryFile("w", suffix=".token", delete=False)
    token_file.write("file-token")
    token_file.close()
    cfg = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    cfg.write(json.dumps({"tokenFile": token_file.name}))
    cfg.close()
    unreadable_dir: str | None = None
    cfg_broken: str | None = None
    saved = {k: os.environ.get(k) for k in
             ("OPENSHELL_MANAGER_TOKEN", "OPENSHELL_MANAGER_CONFIG")}
    server, req = make_app(token_env=None)
    try:
        def point_config_at(cfg_path: str):
            os.environ["OPENSHELL_MANAGER_CONFIG"] = cfg_path
            os.environ.pop("OPENSHELL_MANAGER_TOKEN", None)
            config._config_cache = None
            config._token_cache = None

        # (a) 可读 tokenFile：正常鉴权（错 token 401 / 对 token 200）
        point_config_at(cfg.name)
        status, _ = req("GET", "/api/v1/inference/route?workspace=default")
        assert status == 401, status
        status, payload = req("GET", "/api/v1/inference/route?workspace=default",
                              token="file-token")
        assert status == 200 and payload["provider"] == "prov-x", payload

        # (b) 读失败 → 503 fail-closed；healthz 不受影响
        if os.geteuid() == 0:
            unreadable_dir = tempfile.mkdtemp(suffix=".as-token-file")
            broken_source = unreadable_dir
        else:
            os.chmod(token_file.name, 0o000)
            broken_source = token_file.name
        cfg_broken = tempfile.NamedTemporaryFile("w", suffix=".json",
                                                 delete=False).name
        with open(cfg_broken, "w", encoding="utf-8") as fh:
            fh.write(json.dumps({"tokenFile": broken_source}))
        point_config_at(cfg_broken)  # 复位 token 缓存（含文件结果 5s 缓存）
        status, payload = req("GET", "/api/v1/inference/route?workspace=default",
                              token="file-token")
        assert status == 503, (status, payload)
        status, payload = req("GET", "/healthz")
        assert status == 200 and payload["ok"] is True, payload
    finally:
        os.chmod(token_file.name, 0o644)
        if unreadable_dir:
            os.rmdir(unreadable_dir)
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        config._config_cache = None
        config._token_cache = None
        server.shutdown()


def test_manager_token_file_result_cached():
    """B3-2：manager_token() 文件解析结果 5s 缓存——async 依赖里消除
    每请求阻塞磁盘 IO；env 优先级不受缓存影响（直读）。"""
    calls = {"n": 0}
    orig = config._token_from_file
    cfg = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    cfg.write(json.dumps({"tokenFile": "/tmp/fake.token"}))
    cfg.close()
    saved = {k: os.environ.get(k) for k in
             ("OPENSHELL_MANAGER_TOKEN", "OPENSHELL_MANAGER_CONFIG")}

    def counting(token_file):
        calls["n"] += 1
        return "cached-token"

    config._token_from_file = counting
    try:
        os.environ["OPENSHELL_MANAGER_CONFIG"] = cfg.name
        os.environ.pop("OPENSHELL_MANAGER_TOKEN", None)
        config._config_cache = None
        config._token_cache = None
        assert config.manager_token() == "cached-token"
        assert config.manager_token() == "cached-token"
        assert calls["n"] == 1, \
            f"token file must be read once per TTL window, read {calls['n']}x"
        # 缓存过期后重读
        config._token_cache = ("cached-token", time.monotonic() - 0.001)
        assert config.manager_token() == "cached-token"
        assert calls["n"] == 2, calls
        # env 直读不受缓存影响（优先级契约）
        os.environ["OPENSHELL_MANAGER_TOKEN"] = "env-token"
        assert config.manager_token() == "env-token"
    finally:
        config._token_from_file = orig
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        config._config_cache = None
        config._token_cache = None


def test_unhandled_exception_maps_500_generic():
    """B3-3：未捕获异常兜底 = 500 + 通用文案 "internal error"（细节进
    服务端 stderr 日志）。原 502 会让上游按"网关不可达"误重试/降级，
    且异常细节（类型+消息）直接泄漏给客户端。"""
    server, req = make_app(token_env=None)
    api.facade = GatewayFacade(client_factory=lambda: FailingClient())
    try:
        status, payload = req("GET", "/api/v1/gateway/health")
        assert status == 500, payload
        assert payload == {"error": "internal error"}, payload
    finally:
        server.shutdown()


def test_target_port_strict_integer():
    """B3-3：_int_field 严格化（_opt_bool 同款）——target_port 只收 int：
    bool（int 子类恒真陷阱）/float（含整值形式）/字符串数字一律 400。
    原 int() 强转放过 "8123"、8123.5、True 等垃圾类型。"""
    server, req = make_app(token_env=None)
    try:
        for bad in ("8123", 8123.5, 8080.0, True):
            status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                                  {"workspace": "default", "service": "s",
                                   "target_port": bad})
            assert status == 400 and "target_port" in payload["error"], \
                (bad, status, payload)
        status, payload = req("POST", "/api/v1/sandboxes/dsh-fake/services",
                              {"workspace": "default", "service": "s",
                               "target_port": 8123})
        assert status == 200 and payload["target_port"] == 8123, payload
    finally:
        server.shutdown()


def test_max_upload_bytes_defaults_to_2gib():
    """B3-3：maxUploadBytes 缺省 2GiB（2147483648）非零——原缺省 0（不限）
    让纯防误操作的上限形同虚设（有人误指 50GB 归档时无拦截）。"""
    saved = {k: os.environ.get(k) for k in
             ("OPENSHELL_MANAGER_MAX_UPLOAD_BYTES", "OPENSHELL_MANAGER_CONFIG")}
    try:
        os.environ.pop("OPENSHELL_MANAGER_MAX_UPLOAD_BYTES", None)
        cfg = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
        cfg.write("{}")
        cfg.close()
        os.environ["OPENSHELL_MANAGER_CONFIG"] = cfg.name
        config._config_cache = None
        assert config.max_upload_bytes() == 2147483648, \
            config.max_upload_bytes()
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        config._config_cache = None


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

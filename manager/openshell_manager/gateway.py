"""Gateway facade: the OpenShell SDK operations the manager exposes over HTTP.

Dict boundary mirrors the engine's ``GrpcSandboxClient`` (engine repo,
``openshell_runtime_real.py``): protobufs exist only on the service side of
the wire. All SDK imports are lazy so the module imports (and the HTTP
surface self-describes) even where the vendored SDK is absent.

``client_factory`` is the seam for tests: inject a stub returning a fake
SDK client instead of touching the real gateway.
"""
from __future__ import annotations

import base64
import sys
import threading
from pathlib import Path
from typing import Any, Dict, Iterator, List, Optional

from . import config

_sdk_path_done = False


def _ensure_sdk_path():
    """Put the vendored openshell SDK on sys.path exactly once. Called at
    every lazy protobuf/SDK import point (also with injected test factories)."""
    global _sdk_path_done
    if _sdk_path_done:
        return
    lib = config.openshell_lib_path()
    if str(lib) not in sys.path:
        sys.path.insert(0, str(lib))
    _sdk_path_done = True


class GatewayFacade:
    """All gateway operations the manager service exposes. Stateless."""

    def __init__(self, client_factory=None):
        self._client_factory = client_factory or self._default_client_factory
        self._client = None
        self._lock = threading.Lock()

    # -- SDK plumbing -------------------------------------------------------

    @staticmethod
    def _default_client_factory():
        _ensure_sdk_path()
        try:
            from openshell import SandboxClient  # noqa: F401
        except ImportError as exc:  # pragma: no cover - environment error
            raise RuntimeError(
                f"openshell SDK not importable from "
                f"{config.openshell_lib_path()}: {exc}") from exc
        return SandboxClient(config.gateway_endpoint(), timeout=60)

    def _sdk(self):
        with self._lock:
            if self._client is None:
                self._client = self._client_factory()
            return self._client

    @staticmethod
    def _pb():
        _ensure_sdk_path()
        from openshell._proto import openshell_pb2, sandbox_pb2  # noqa: F401
        from google.protobuf.json_format import MessageToDict, ParseDict
        return openshell_pb2, sandbox_pb2, MessageToDict, ParseDict

    def _ref(self, ref) -> Dict[str, Any]:
        """Full status projection: plain ref fields PLUS phase_name and the
        raw status.conditions list (gateway diagnostics must survive the
        transport — gateway_probe.py depends on them)."""
        pb, _sandbox, MessageToDict, _ParseDict = self._pb()
        try:
            phase_name = pb.SandboxPhase.Name(ref.status.phase)
        except Exception:  # noqa: BLE001 - unknown enum value
            phase_name = str(ref.status.phase)
        conditions = [
            MessageToDict(c, preserving_proto_field_name=True)
            for c in (getattr(ref.status, "conditions", None) or [])
        ]
        return {
            "id": ref.id,
            "name": ref.name,
            "workspace": ref.workspace,
            "phase": ref.status.phase,
            "phase_name": phase_name,
            "current_policy_version": ref.status.current_policy_version,
            "labels": dict(ref.labels),
            "conditions": conditions,
        }

    # -- sandbox lifecycle ---------------------------------------------------

    def health(self) -> Dict[str, Any]:
        client = self._sdk()
        ok = client.health() is not None
        return {"ok": ok, "endpoint": config.gateway_endpoint()}

    @staticmethod
    def _parse_dict(js: Dict[str, Any], message) -> Any:
        """dict→proto，边界统一：json_format.ParseError（protobuf 新版已
        非 ValueError 子类）转换为 ValueError，让 HTTP 层按 400 客户端错误
        映射而不是 502 上游失败。"""
        _pb, _sandbox, _m2d, ParseDict = GatewayFacade._pb()
        try:
            return ParseDict(js, message, ignore_unknown_fields=False)
        except Exception as exc:  # noqa: BLE001 - 仅 ParseDict 路径
            raise ValueError(str(exc)) from exc

    def create(self, *, workspace: str, name: str, spec: Dict[str, Any]) -> Dict[str, Any]:
        client = self._sdk()
        pb, _sandbox, _m2d, _ParseDict = self._pb()
        pb_spec = self._parse_dict(spec, pb.SandboxSpec())
        return self._ref(client.create(workspace=workspace, name=name or "", spec=pb_spec))

    def get(self, *, name: str, workspace: str) -> Dict[str, Any]:
        client = self._sdk()
        return self._ref(client.get(name, workspace=workspace))

    def wait_ready(self, *, name: str, workspace: str,
                   timeout_seconds: float = 300.0) -> Dict[str, Any]:
        client = self._sdk()
        return self._ref(client.wait_ready(
            name, workspace=workspace, timeout_seconds=timeout_seconds))

    def exec(self, *, sandbox_id: str, command: List[str],
             workdir: Optional[str] = None,
             environment: Optional[Dict[str, str]] = None,
             stdin: Optional[bytes] = None,
             timeout_seconds: Optional[int] = None) -> Dict[str, Any]:
        client = self._sdk()
        result = client.exec(
            sandbox_id, list(command), workdir=workdir, env=environment or {},
            stdin=stdin, timeout_seconds=timeout_seconds)
        return {
            "exit_code": result.exit_code,
            "stdout": result.stdout.decode("utf-8", errors="replace")
            if isinstance(result.stdout, (bytes, bytearray)) else result.stdout,
            "stderr": result.stderr.decode("utf-8", errors="replace")
            if isinstance(result.stderr, (bytes, bytearray)) else result.stderr,
        }

    # -- file upload ----------------------------------------------------------

    def resolve_sandbox_id(self, *, name: str, workspace: str) -> str:
        """人类指令 2026-09-01：/files 接口层对外收 name，内部自行换 UUID 走后续
        上传（网关 ExecSandbox 系 RPC 只认 UUID；其余 name 端点由此统一收口）。"""
        return self._sdk().get(name, workspace=workspace).id

    # ExecSandbox carries stdin as a single proto bytes field. The gateway
    # REJECTS request messages over 1MiB (measured live 2026-09-06:
    # OUT_OF_RANGE "decoded message length too large … limit is 1048576" —
    # NOT the 4MiB gRPC default the original comment assumed). 720KiB raw →
    # 960KiB base64 text, leaving ~64KiB headroom for command/framing.
    UPLOAD_CHUNK_BYTES = 720 * 1024

    def upload_file(self, *, sandbox_id: str, path: str, content: bytes,
                    mode: Optional[str] = None) -> Dict[str, Any]:
        """Write ``content`` to ``path`` (thin wrapper over the stream API)."""
        return self.write_file_stream(sandbox_id=sandbox_id, path=path,
                                      chunks=iter([content]), mode=mode)

    def write_file_stream(self, *, sandbox_id: str, path: str,
                          chunks: "Iterator[bytes]",
                          mode: Optional[str] = None) -> Dict[str, Any]:
        """Write a byte-stream to ``path`` in the sandbox, chunk by chunk.

        Consumes ``chunks`` lazily: each accumulated UPLOAD_CHUNK_BYTES slice
        is base64-ENCODED and fed to ``base64 -d`` through ExecSandbox stdin
        (the supervisor pipes stdin bytes straight to the process, so the
        decoder must receive base64 text); chunk 0 truncates, later chunks
        append. The target's parent directory is created (quoted
        ``mkdir -p "$(dirname …)"``) BEFORE the first chunk lands — the
        ``.part`` staging file lives inside that directory, so a missing
        parent would fail the very first chunk write. Memory use is bounded
        by the chunk size regardless of size. On any failure the ``.part``
        staging file is removed and the original path left untouched.
        """
        import shlex

        if not path.startswith("/"):
            raise ValueError("path must be absolute")
        escaped = shlex.quote(path)
        total = 0
        chunk_count = 0
        buffer = b""
        try:
            # 父目录先于第一个分块建好；命令替换必须套引号，否则含空格/通配符
            # 的目录名会被词拆分（mkdir -p a b）或弹 glob，mkdir 造错目录。
            self._exec_or_fail(
                sandbox_id,
                ["/bin/sh", "-c", f'mkdir -p "$(dirname {escaped})"'],
                "parent dir creation")
            # 原始切片按 3 字节对齐后再编码：2MiB 非 3 的倍数，若按整块编码，
            # 每段 base64 各带 padding，沙箱内单条 `base64 -d` 流式解码会在段中
            # 遇到 padding 而中断/截断（人类指令 2026-09-01 name 口径联调时实测暴露）。
            take = self.UPLOAD_CHUNK_BYTES - (self.UPLOAD_CHUNK_BYTES % 3)
            for piece in chunks:
                buffer += piece
                while len(buffer) >= take:
                    self._exec_upload_chunk(
                        sandbox_id, escaped,
                        buffer[:take],
                        append=chunk_count > 0)
                    total += take
                    chunk_count += 1
                    buffer = buffer[take:]
            # Flush the tail; skip only when the stream ended exactly on a
            # chunk boundary (an empty stream still creates the file).
            if buffer or chunk_count == 0:
                self._exec_upload_chunk(sandbox_id, escaped, buffer,
                                        append=chunk_count > 0)
                total += len(buffer)
                chunk_count += 1
        except Exception:
            # Best-effort cleanup of the staging file so a failed upload
            # leaves no debris; the target path was never touched.
            try:
                self.exec(sandbox_id=sandbox_id,
                          command=["rm", "-f", f"{path}.part"])
            except Exception:  # noqa: BLE001 - cleanup is advisory
                pass
            raise
        # Rename into place only after every chunk landed, so a failed upload
        # never leaves a truncated file at the target path.
        self._exec_or_fail(
            sandbox_id,
            ["/bin/sh", "-c", f"mv {escaped}.part {escaped}"],
            "finalize mv")
        if mode:
            self._exec_or_fail(
                sandbox_id, command=["chmod", mode, path], what=f"chmod {mode}")
        return {"path": path, "bytes": total, "chunks": chunk_count}

    def _exec_or_fail(self, sandbox_id: str, command: List[str],
                      what: str, stdin: Optional[bytes] = None) -> Dict[str, Any]:
        """Run one sandbox exec and turn a non-zero exit into RuntimeError
        (which triggers the caller's .part cleanup)."""
        result = self.exec(sandbox_id=sandbox_id, command=command, stdin=stdin)
        if result["exit_code"] != 0:
            raise RuntimeError(
                f"upload {what} failed with exit code "
                f"{result['exit_code']}: {result['stderr'][:500]}")
        return result

    def _exec_upload_chunk(self, sandbox_id: str, escaped_path: str,
                           data: bytes, *, append: bool) -> None:
        operator = ">>" if append else ">"
        self._exec_or_fail(
            sandbox_id,
            ["/bin/sh", "-c", f"base64 -d {operator} {escaped_path}.part"],
            "chunk write",
            stdin=base64.b64encode(data))

    def get_logs(self, *, sandbox_id: str, workspace: str, lines: int = 2000,
                 since_ms: int = 0) -> List[Dict[str, Any]]:
        client = self._sdk()
        pb, _sandbox, MessageToDict, _ParseDict = self._pb()
        request = pb.GetSandboxLogsRequest(
            sandbox_id=sandbox_id, lines=lines, since_ms=since_ms,
            workspace=workspace)
        response = client._stub.GetSandboxLogs(request, timeout=60)
        return [MessageToDict(line, preserving_proto_field_name=True)
                for line in response.logs]

    def update_config(self, *, name: str, workspace: str,
                      policy: Dict[str, Any]) -> Dict[str, Any]:
        client = self._sdk()
        pb, sandbox_pb, _m2d, _ParseDict = self._pb()
        pb_policy = self._parse_dict(policy, sandbox_pb.SandboxPolicy())
        response = client._stub.UpdateConfig(
            pb.UpdateConfigRequest(name=name, workspace=workspace,
                                   policy=pb_policy), timeout=60)
        return {"version": response.version, "policy_hash": response.policy_hash}

    def delete(self, *, name: str, workspace: str) -> bool:
        client = self._sdk()
        return bool(client.delete(name, workspace=workspace))

    def list_all(self, *, limit: int = 500) -> List[Dict[str, Any]]:
        client = self._sdk()
        return [self._ref(ref) for ref in
                client.list_for_all_workspaces(limit=limit)]

    # -- sandbox service exposure (ExposeService) -----------------------------

    @staticmethod
    def _service_projection(resp) -> Dict[str, Any]:
        ep = resp.endpoint
        return {"name": ep.service_name, "sandbox_id": ep.sandbox_id,
                "sandbox_name": ep.sandbox_name,
                "target_port": ep.target_port, "domain": bool(ep.domain),
                "url": resp.url}

    def expose_service(self, *, sandbox: str, service: str,
                       target_port: int, workspace: str,
                       domain: bool = False) -> Dict[str, Any]:
        stub = pb_grpc_stub(self._sdk())
        pb, _sandbox, _m2d, _ParseDict = self._pb()
        resp = stub.ExposeService(pb.ExposeServiceRequest(
            sandbox=sandbox, service=service, target_port=int(target_port),
            domain=bool(domain), workspace=workspace), timeout=60)
        return self._service_projection(resp)

    def list_services(self, *, sandbox: str, workspace: str = "",
                      limit: int = 100, offset: int = 0,
                      all_workspaces: bool = False) -> List[Dict[str, Any]]:
        stub = pb_grpc_stub(self._sdk())
        pb, _sandbox, _m2d, _ParseDict = self._pb()
        resp = stub.ListServices(pb.ListServicesRequest(
            sandbox=sandbox, workspace=workspace, limit=limit, offset=offset,
            all_workspaces=all_workspaces), timeout=30)
        return [self._service_projection(s) for s in resp.services]

    def delete_service(self, *, sandbox: str, service: str,
                       workspace: str) -> Dict[str, Any]:
        stub = pb_grpc_stub(self._sdk())
        pb, _sandbox, _m2d, _ParseDict = self._pb()
        resp = stub.DeleteService(pb.DeleteServiceRequest(
            sandbox=sandbox, service=service, workspace=workspace), timeout=30)
        return {"deleted": bool(resp.deleted)}

    # -- inference route / provider admin ------------------------------------

    def _route_client(self):
        _ensure_sdk_path()
        from openshell.sandbox import InferenceRouteClient
        return InferenceRouteClient.from_sandbox_client(self._sdk())

    def get_route(self, *, workspace: str) -> Dict[str, Any]:
        cfg = self._route_client().get_route(workspace=workspace)
        return {"provider": cfg.provider_name, "model": cfg.model_id,
                "version": cfg.version}

    def set_route(self, *, workspace: str, provider: str, model: str,
                  no_verify: bool = False) -> Dict[str, Any]:
        cfg = self._route_client().set_route(
            workspace=workspace, provider_name=provider, model_id=model,
            no_verify=no_verify)
        return {"provider": cfg.provider_name, "model": cfg.model_id,
                "version": cfg.version}

    def list_providers(self, *, workspace: str) -> List[str]:
        _ensure_sdk_path()
        client = self._sdk()
        from openshell._proto import openshell_pb2 as pb
        stub = pb_grpc_stub(client)
        providers = stub.ListProviders(
            pb.ListProvidersRequest(workspace=workspace), timeout=30)
        return [p.metadata.name for p in providers.providers]

    def get_provider(self, *, workspace: str, name: str) -> Dict[str, Any]:
        """Provider detail WITHOUT credentials (secret masking by omission:
        credentials live only in the gateway's encrypted store)."""
        _ensure_sdk_path()
        client = self._sdk()
        from openshell._proto import openshell_pb2 as pb
        stub = pb_grpc_stub(client)
        providers = stub.ListProviders(
            pb.ListProvidersRequest(workspace=workspace), timeout=30)
        for p in providers.providers:
            if p.metadata.name == name:
                return {"name": p.metadata.name, "type": p.type,
                        "config": dict(p.config)}
        raise LookupError(f"provider '{name}' not found in workspace "
                          f"'{workspace}'")

    def upsert_provider(self, *, workspace: str, name: str, type_: str,
                        credentials: Dict[str, str],
                        conf: Dict[str, str]) -> Dict[str, Any]:
        _ensure_sdk_path()
        client = self._sdk()
        from openshell._proto import openshell_pb2 as pb
        from openshell._proto import datamodel_pb2 as dm
        stub = pb_grpc_stub(client)
        existing = stub.ListProviders(
            pb.ListProvidersRequest(workspace=workspace), timeout=30)
        exists = any(p.metadata.name == name for p in existing.providers)
        provider = dm.Provider(
            metadata=dm.ObjectMeta(name=name),
            type=type_,
            credentials=dict(credentials),
            config=dict(conf),
        )
        if exists:
            stub.UpdateProvider(pb.UpdateProviderRequest(
                provider=provider, workspace=workspace), timeout=30)
        else:
            stub.CreateProvider(pb.CreateProviderRequest(
                provider=provider, workspace=workspace), timeout=30)
        return {"name": name, "created": not exists}


def pb_grpc_stub(client):
    """OpenShell gRPC admin stub on the sandbox client's channel."""
    _ensure_sdk_path()
    from openshell._proto import openshell_pb2_grpc as pb_grpc
    return pb_grpc.OpenShellStub(client._channel)

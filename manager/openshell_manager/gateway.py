"""Gateway facade: the OpenShell SDK operations the manager exposes over HTTP.

Dict boundary mirrors the engine's ``GrpcSandboxClient`` (engine repo,
``openshell_runtime_real.py``): protobufs exist only on the service side of
the wire. The ``openshell`` SDK is a top-level package of this repo
(2026-09-15 解绑: 原 libs/OpenShell/python/openshell 并入仓根), imported
at module top like any normal dependency.

``client_factory`` is the seam for tests: inject a stub returning a fake
SDK client instead of touching the real gateway.
"""
from __future__ import annotations

import base64
import contextlib
import threading
from typing import Any, Dict, Iterator, List, Optional

from google.protobuf.json_format import MessageToDict, ParseDict
from openshell import SandboxClient, WorkspaceClient
from openshell._proto import (
    datamodel_pb2,
    inference_pb2,
    inference_pb2_grpc,
    openshell_pb2,
    openshell_pb2_grpc,
    sandbox_pb2,
)

from . import config


def _rpc_code_name(exc) -> "str | None":
    """grpc.RpcError（须同时实现 grpc.Call）才有 code()；返回其 name（如
    'NOT_FOUND'）。鸭子探测而非 isinstance 判型——测试替身无需构造真
    RpcError 即可覆盖各 code 分支。"""
    code = getattr(exc, "code", None)
    if not callable(code):
        return None
    return getattr(code(), "name", None)


@contextlib.contextmanager
def _map_not_found(what: str):
    """南向 gRPC NOT_FOUND → LookupError（api 层既有 404 通道）。

    R22 审计修复：真实 SDK 对网关 NOT_FOUND 裸抛 grpc.RpcError（非
    LookupError 子类，MRO 实测），沙箱不存在曾全落兜底 500，文档承诺的
    404 从未生效。其余 code（UNAVAILABLE/UNKNOWN 等）原样上抛维持 500
    兜底——health→500 是 api-external 已接受口径，不扩大映射面。"""
    try:
        yield
    except Exception as exc:
        if _rpc_code_name(exc) == "NOT_FOUND":
            raise LookupError(f"{what} not found") from exc
        raise


@contextlib.contextmanager
def _map_verification_failed():
    """R30：南向 FAILED_PRECONDITION（路由实核失败）→
    InferenceVerificationFailed（api 层 400）。其余 code 原样上抛维持兜底。"""
    try:
        yield
    except Exception as exc:
        if _rpc_code_name(exc) == "FAILED_PRECONDITION":
            details = getattr(exc, "details", None)
            msg = details() if callable(details) else str(exc)
            raise InferenceVerificationFailed(msg) from exc
        raise


@contextlib.contextmanager
def _map_invalid_argument():
    """南向 INVALID_ARGUMENT（管理面：类型不支持/类型不可改等，details 携带
    可行动指引）→ InferenceAdminRejected（api 层 400）。其余 code 原样上抛维持
    兜底。2026-09-17 实证：unsupported type（details 含 supported 类型表）与
    "type cannot be changed" 曾裸 500 internal error，客户端拿不到指引。"""
    try:
        yield
    except Exception as exc:
        if _rpc_code_name(exc) == "INVALID_ARGUMENT":
            details = getattr(exc, "details", None)
            msg = details() if callable(details) else str(exc)
            raise InferenceAdminRejected(msg) from exc
        raise


class ExecOutputTooLarge(RuntimeError):
    """R27 审计修复：exec 输出超过 MAX_EXEC_OUTPUT_BYTES。api 层映射 413
    （确定性拒绝——5xx 会被上游按"服务端故障"误重试/降级）。"""


class InferenceVerificationFailed(RuntimeError):
    """R30：set_route 南向 FAILED_PRECONDITION（endpoint 连通性实核失败）。
    api 层映射 400——客户端可修正（换 provider/修 base_url/改 no_verify），
    500 会被上游按"服务端故障"误重试/降级（同 ExecOutputTooLarge 413 理由）。"""


class StateConflict(RuntimeError):
    """南向资源状态冲突（ALREADY_EXISTS / FAILED_PRECONDITION）→ api 层 409。
    2026-09-19 切片3b：workspace 建区重名/删区含资源（上游资源守卫）——
    客户端可修正条件，5xx 会被误当服务端故障。"""


@contextlib.contextmanager
def _map_state_conflict():
    """南向 ALREADY_EXISTS / FAILED_PRECONDITION → StateConflict（details
    透传）。其余 code 原样上抛维持兜底。"""
    try:
        yield
    except Exception as exc:
        code = _rpc_code_name(exc)
        if code in ("ALREADY_EXISTS", "FAILED_PRECONDITION"):
            details = getattr(exc, "details", None)
            msg = details() if callable(details) else str(exc)
            raise StateConflict(msg) from exc
        raise


class InferenceAdminRejected(RuntimeError):
    """管理面南向 INVALID_ARGUMENT（provider 类型不支持/类型不可改等，
    details 携带 supported 类型表等可行动指引）。api 层映射 400 透传 details
    ——同 InferenceVerificationFailed 的"客户端可修正条件不占 5xx"口径
    （2026-09-17：107 实测裸 500 internal error，指引全丢）。"""


class GatewayFacade:
    """All gateway operations the manager service exposes. Stateless."""

    def __init__(self, client_factory=None):
        self._client_factory = client_factory or self._default_client_factory
        self._client = None
        self._lock = threading.Lock()

    # -- SDK plumbing -------------------------------------------------------

    @staticmethod
    def _default_client_factory():
        return SandboxClient(config.gateway_endpoint(), timeout=60)

    def _sdk(self):
        with self._lock:
            if self._client is None:
                self._client = self._client_factory()
            return self._client

    @staticmethod
    def _pb():
        return openshell_pb2, sandbox_pb2, MessageToDict, ParseDict

    def _admin_stub(self):
        """OpenShell admin stub + openshell pb 模块——ExposeService 与
        Provider 系 RPC 的统一取用点（原六处重复的 stub/pb 取用样板）。"""
        pb, _sandbox, _m2d, _ParseDict = self._pb()
        return pb_grpc_stub(self._sdk()), pb

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
        # 切片3b 缺陷②修复：未知工作区=网关 resolve_workspace NOT_FOUND（沙箱
        # 建不出来，2026-09-19 审计实测曾裸 500 internal error 掩盖根因）→ 404。
        with _map_not_found(f"workspace '{workspace}' (sandbox create rejected)"):
            client = self._sdk()
            pb, _sandbox, _m2d, _ParseDict = self._pb()
            pb_spec = self._parse_dict(spec, pb.SandboxSpec())
            return self._ref(client.create(workspace=workspace, name=name or "", spec=pb_spec))

    def get(self, *, name: str, workspace: str) -> Dict[str, Any]:
        with _map_not_found(f"sandbox '{name}' in workspace '{workspace}'"):
            client = self._sdk()
            return self._ref(client.get(name, workspace=workspace))

    def wait_ready(self, *, name: str, workspace: str,
                   timeout_seconds: float = 300.0) -> Dict[str, Any]:
        with _map_not_found(f"sandbox '{name}' in workspace '{workspace}'"):
            client = self._sdk()
            return self._ref(client.wait_ready(
                name, workspace=workspace, timeout_seconds=timeout_seconds))

    # R27 审计修复：exec 输出累计上限。SDK exec 曾无界攒 stdout/stderr，
    # `cat /dev/zero` 类命令以线速耗尽管理面内存（殃及全部租户）；facade
    # 改走 exec_stream 流式消费并在此钳制（vendored 上游树零改动；生成器
    # 惰性拉取保证 SDK 侧同步受界，放弃消费即释放）。
    MAX_EXEC_OUTPUT_BYTES = 64 * 1024 * 1024

    def exec(self, *, sandbox_id: str, command: List[str],
             workdir: Optional[str] = None,
             environment: Optional[Dict[str, str]] = None,
             stdin: Optional[bytes] = None,
             timeout_seconds: Optional[int] = None) -> Dict[str, Any]:
        client = self._sdk()
        stdout_parts: List[bytes] = []
        stderr_parts: List[bytes] = []
        received = 0
        exit_code: Optional[int] = None
        # 流式路径同样暴露寻址类 NOT_FOUND（R22）：错 UUID/沙箱中途消失——
        # 与 get_logs/update_config 同一 _map_not_found 通道（LookupError
        # 文案 "sandbox {id} not found" 逐字节不变）。
        with _map_not_found(f"sandbox {sandbox_id}"):
            for event in client.exec_stream(
                    sandbox_id, list(command), workdir=workdir,
                    env=environment or {}, stdin=stdin,
                    timeout_seconds=timeout_seconds):
                stream = getattr(event, "stream", None)
                if stream not in ("stdout", "stderr"):
                    # ExecResult 收尾事件：输出以逐 chunk 累计为准（受上限
                    # 约束），此处只取退出码。
                    exit_code = event.exit_code
                    continue
                data = event.data
                received += len(data)
                if received > self.MAX_EXEC_OUTPUT_BYTES:
                    raise ExecOutputTooLarge(
                        f"exec output exceeds {self.MAX_EXEC_OUTPUT_BYTES} "
                        "byte cap")
                (stdout_parts if stream == "stdout"
                 else stderr_parts).append(data)
        if exit_code is None:
            raise RuntimeError("exec stream ended without an exit event")
        return {
            "exit_code": exit_code,
            "stdout": b"".join(stdout_parts).decode("utf-8", errors="replace"),
            "stderr": b"".join(stderr_parts).decode("utf-8", errors="replace"),
        }

    # -- file upload ----------------------------------------------------------

    def resolve_sandbox_id(self, *, name: str, workspace: str) -> str:
        """/files 接口层对外收 name，内部自行换 UUID 走后续
        上传（网关 ExecSandbox 系 RPC 只认 UUID；其余 name 端点由此统一收口）。"""
        with _map_not_found(f"sandbox '{name}' in workspace '{workspace}'"):
            return self._sdk().get(name, workspace=workspace).id

    # ExecSandbox carries stdin as a single proto bytes field. The gateway
    # REJECTS request messages over 1MiB (measured live 2026-09-06:
    # OUT_OF_RANGE "decoded message length too large … limit is 1048576" —
    # NOT the 4MiB gRPC default the original comment assumed). 720KiB raw →
    # 960KiB base64 text, leaving ~64KiB headroom for command/framing.
    UPLOAD_CHUNK_BYTES = 720 * 1024

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
            # 遇到 padding 而中断/截断（name 口径联调时实测暴露）。
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
            # Rename into place only after every chunk landed, so a failed
            # upload never leaves a truncated file at the target path.
            # R26d 审计修复：finalize mv/chmod 纳入同一 try——mv 因磁盘满/
            # 权限失败曾绕过下方 .part 清理（违背本 docstring 承诺）；mv 已
            # 成功后 chmod 失败时 rm 是无害空操作。
            self._exec_or_fail(
                sandbox_id,
                ["/bin/sh", "-c", f"mv {escaped}.part {escaped}"],
                "finalize mv")
            if mode:
                self._exec_or_fail(
                    sandbox_id, command=["chmod", mode, path],
                    what=f"chmod {mode}")
        except Exception:
            # Best-effort cleanup of the staging file so a failed upload
            # leaves no debris; the target path was never touched.
            try:
                self.exec(sandbox_id=sandbox_id,
                          command=["rm", "-f", f"{path}.part"])
            except Exception:  # noqa: BLE001 - cleanup is advisory
                pass
            raise
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
        with _map_not_found(f"sandbox {sandbox_id}"):
            response = client._stub.GetSandboxLogs(request, timeout=60)
        return [MessageToDict(line, preserving_proto_field_name=True)
                for line in response.logs]

    def update_config(self, *, name: str, workspace: str,
                      policy: Dict[str, Any]) -> Dict[str, Any]:
        client = self._sdk()
        pb, sandbox_pb, _m2d, _ParseDict = self._pb()
        pb_policy = self._parse_dict(policy, sandbox_pb.SandboxPolicy())
        with _map_not_found(f"sandbox '{name}'"):
            # R22 审查补全：对不存在沙箱的热更新同属寻址错误 → 404（曾落
            # 兜底 500，与错误码总表的泛化 404 承诺不一致）
            response = client._stub.UpdateConfig(
                pb.UpdateConfigRequest(name=name, workspace=workspace,
                                       policy=pb_policy), timeout=60)
        return {"version": response.version, "policy_hash": response.policy_hash}

    def delete(self, *, name: str, workspace: str) -> bool:
        with _map_not_found(f"sandbox '{name}' in workspace '{workspace}'"):
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
        stub, pb = self._admin_stub()
        with _map_not_found(f"sandbox '{sandbox}'"):
            resp = stub.ExposeService(pb.ExposeServiceRequest(
                sandbox=sandbox, service=service, target_port=int(target_port),
                domain=bool(domain), workspace=workspace), timeout=60)
        return self._service_projection(resp)

    def list_services(self, *, sandbox: str, workspace: str = "",
                      limit: int = 100, offset: int = 0,
                      all_workspaces: bool = False) -> List[Dict[str, Any]]:
        stub, pb = self._admin_stub()
        with _map_not_found(f"sandbox '{sandbox}'"):
            resp = stub.ListServices(pb.ListServicesRequest(
                sandbox=sandbox, workspace=workspace, limit=limit,
                offset=offset, all_workspaces=all_workspaces), timeout=30)
        return [self._service_projection(s) for s in resp.services]

    def delete_service(self, *, sandbox: str, service: str,
                       workspace: str) -> Dict[str, Any]:
        stub, pb = self._admin_stub()
        with _map_not_found(f"sandbox '{sandbox}'"):
            resp = stub.DeleteService(pb.DeleteServiceRequest(
                sandbox=sandbox, service=service, workspace=workspace),
                timeout=30)
        return {"deleted": bool(resp.deleted)}

    # -- workspace lifecycle（切片3b team 上车；manager 纯透传不执法）-------

    def _workspace_client(self):
        """WorkspaceClient 缝（测试替身注入点，_inference_stub 同型）。"""
        return WorkspaceClient.from_sandbox_client(self._sdk())

    def list_workspaces(self, *, limit: int = 100, offset: int = 0) -> Dict[str, Any]:
        wc = self._workspace_client()
        return {"workspaces": [
            {"name": w.name, "phase": w.phase, "labels": dict(w.labels)}
            for w in wc.list(limit=limit, offset=offset)
        ]}

    def create_workspace(self, *, name: str,
                         labels: Optional[Dict[str, str]] = None) -> Dict[str, Any]:
        # 名字非法=INVALID_ARGUMENT（DNS-1123 校验在网关）→ 400 指引；
        # 重名=ALREADY_EXISTS → 409
        with _map_invalid_argument(), _map_state_conflict():
            wc = self._workspace_client()
            ref = wc.create(name, labels=labels or {})
            return {"name": ref.name, "phase": ref.phase, "labels": dict(ref.labels)}

    def delete_workspace(self, *, name: str) -> Dict[str, Any]:
        # 未知区=NOT_FOUND→404；区内含资源=FAILED_PRECONDITION（上游资源守卫，
        # 2026-09-19 e2e 实证错误原文透传）→ 409
        with _map_not_found(f"workspace '{name}'"), _map_state_conflict():
            wc = self._workspace_client()
            deleted = wc.delete(name)
            return {"name": name, "deleted": bool(deleted)}

    # -- inference route / provider admin ------------------------------------

    def _inference_stub(self):
        """Direct stub on the inference service (same channel as the SDK
        SandboxClient). 直连而非 SDK InferenceRouteClient：SDK 客户端丢回执
        字段（set_route 丢 validation_performed/validated_endpoints，
        InferenceRouteConfig 丢 route_name/timeout_secs；delete_route 无
        封装）——回执透传是对外契约（docs/api-external.md 3.16）。Get/Set/
        Delete 三 RPC 透传；GetInferenceBundle 不透传（R32/3.22：沙箱主体
        专享，bearer 必被 PERMISSION_DENIED 拒）。"""
        return inference_pb2_grpc.InferenceStub(self._sdk()._channel)

    @staticmethod
    def _route_desc(workspace: str, route_name: str) -> str:
        # route_name 合法值域=网关 effective_route_name（d51a653f
        # inference.rs）：""（缺省）| "inference.local"（显式实名，与空串
        # 等价）| "sandbox-system"（前后空白被网关 trim），其余名网关
        # INVALID_ARGUMENT→400；默认路由 404 文案保持 R30 既有字面，
        # 命名路由才带名字——默认面零契约漂移
        if route_name:
            return f"inference route '{route_name}' in workspace '{workspace}'"
        return f"inference route in workspace '{workspace}'"

    @staticmethod
    def _route_projection(resp) -> Dict[str, Any]:
        """Get/SetInferenceRouteResponse 共用投影（五公共键；validation_*
        两键仅 Set 回执带，在 set_route 处单独补）。"""
        return {"provider": resp.provider_name, "model": resp.model_id,
                "version": resp.version, "route_name": resp.route_name,
                "timeout_secs": resp.timeout_secs}

    def get_route(self, *, workspace: str, route_name: str = "") -> Dict[str, Any]:
        stub = self._inference_stub()
        # R30：全新部署路由未配置，南向 NOT_FOUND 曾裸 500（dind 七战 e2e 实证；
        # 源码同证 get_inference_route 对缺失路由 Status::not_found
        # "…is not configured in workspace…"）——与 R22 沙箱面同款映射。
        # INVALID_ARGUMENT→400：route_name 进 GET 面后网关 effective_route_name
        # 的拒绝路径在 GET 上可达
        with _map_invalid_argument(), _map_not_found(self._route_desc(workspace, route_name)):
            resp = stub.GetInferenceRoute(
                inference_pb2.GetInferenceRouteRequest(
                    workspace=workspace, route_name=route_name),
                timeout=30)
        return self._route_projection(resp)

    def set_route(self, *, workspace: str, provider: str, model: str,
                  no_verify: bool = False, route_name: str = "",
                  timeout_secs: int = 0) -> Dict[str, Any]:
        stub = self._inference_stub()
        # R30：endpoint 实核失败是客户端可修正条件（换 provider/修 base_url/改
        # no_verify），南向 FAILED_PRECONDITION 曾裸 500——确定性 400（同
        # ExecOutputTooLarge 413 的"5xx 被上游误重试"理由）；INVALID_ARGUMENT
        # 400 透传（2026-09-17 实证：类型不支持 details 含 supported 表；
        # 源码同证：unknown route_name 亦走此通道，文案
        # "unknown route_name '…'; expected 'inference.local' or 'sandbox-system'"）。
        # timeout_secs 是 proto uint64：网关对值原样落库、无校验拒绝路径；
        # 0=缺省 60s 由 router 层施加（d51a653f supervisor-network
        # inference_routes.rs 0→DEFAULT_ROUTE_TIMEOUT 映射，带单测）
        with _map_invalid_argument(), _map_verification_failed():
            resp = stub.SetInferenceRoute(
                inference_pb2.SetInferenceRouteRequest(
                    workspace=workspace, provider_name=provider, model_id=model,
                    route_name=route_name, no_verify=no_verify,
                    timeout_secs=timeout_secs), timeout=30)
        result = self._route_projection(resp)
        result["validation_performed"] = bool(resp.validation_performed)
        result["validated_endpoints"] = [
            {"url": e.url, "protocol": e.protocol}
            for e in resp.validated_endpoints]
        return result

    def delete_route(self, *, workspace: str, route_name: str = "") -> Dict[str, Any]:
        stub = self._inference_stub()
        # 幂等删除：缺失路由回 deleted:false（源码 delete_by_name→bool，
        # 上游测试 delete_route_returns_false_when_not_found 同证），不抛
        # NOT_FOUND——不挂 404 映射（挂了=死代码+误导读者）；未知 route_name
        # 的 INVALID_ARGUMENT 400 透传保留（2026-09-17 管理面先例 c4649ab，
        # 源码实有此路径）
        with _map_invalid_argument():
            resp = stub.DeleteInferenceRoute(
                inference_pb2.DeleteInferenceRouteRequest(
                    workspace=workspace, route_name=route_name),
                timeout=30)
        return {"deleted": bool(resp.deleted)}

    def list_providers(self, *, workspace: str) -> List[Dict[str, Any]]:
        """Provider 概要清单（name/type/config），凭据按省略屏蔽——同
        get_provider 的脱敏纪律，秘密只在网关加密存储。"""
        stub, pb = self._admin_stub()
        # R34/P2-2：上游 resolve_workspace 对未知区回 NOT_FOUND——裸 RpcError 曾
        # 落兜底 500；同一输入在建沙箱/读路由/删区三面均 404，唯本面分裂。
        with _map_not_found(f"workspace '{workspace}' (providers)"):
            providers = stub.ListProviders(
                pb.ListProvidersRequest(workspace=workspace), timeout=30)
        return [{"name": p.metadata.name, "type": p.type,
                 "config": dict(p.config)} for p in providers.providers]

    def get_provider(self, *, workspace: str, name: str) -> Dict[str, Any]:
        """Provider detail WITHOUT credentials (secret masking by omission:
        credentials live only in the gateway's encrypted store)."""
        for p in self.list_providers(workspace=workspace):
            if p["name"] == name:
                return p
        raise LookupError(f"provider '{name}' not found in workspace "
                          f"'{workspace}'")

    def delete_provider(self, *, workspace: str, name: str) -> Dict[str, Any]:
        stub, pb = self._admin_stub()
        # R34/P2-2/P2-3：未知区 NOT_FOUND→404；"provider 仍被沙箱引用"的
        # FAILED_PRECONDITION→409（details 携带阻塞沙箱清单——可行动指引曾
        # 被兜底 500 吞掉）。
        with _map_not_found(f"provider '{name}' in workspace '{workspace}'"), \
                _map_state_conflict():
            resp = stub.DeleteProvider(
                pb.DeleteProviderRequest(name=name, workspace=workspace),
                timeout=30)
        return {"name": name, "deleted": bool(resp.deleted)}

    def upsert_provider(self, *, workspace: str, name: str, type_: str,
                        credentials: Dict[str, str],
                        conf: Dict[str, str]) -> Dict[str, Any]:
        stub, pb = self._admin_stub()
        with _map_not_found(f"workspace '{workspace}' (providers)"):  # R34/P2-2 同面
            existing = stub.ListProviders(
                pb.ListProvidersRequest(workspace=workspace), timeout=30)
        exists = any(p.metadata.name == name for p in existing.providers)
        provider = datamodel_pb2.Provider(
            metadata=datamodel_pb2.ObjectMeta(name=name),
            type=type_,
            credentials=dict(credentials),
            config=dict(conf),
        )
        # INVALID_ARGUMENT 400 通道：类型不可改
        # （"provider type cannot be changed; delete and recreate"）等
        # 客户端可修正条件——裸 500 会吞掉网关指引。
        with _map_invalid_argument():
            if exists:
                stub.UpdateProvider(pb.UpdateProviderRequest(
                    provider=provider, workspace=workspace), timeout=30)
            else:
                try:
                    stub.CreateProvider(pb.CreateProviderRequest(
                        provider=provider, workspace=workspace), timeout=30)
                except Exception as exc:
                    # R26c 审计修复：check-then-act 竞态收敛——并发双 Create 的
                    # 败者收 ALREADY_EXISTS 时落一次 Update，维持 upsert 语义
                    # （曾炸 500）。其余 RpcError 原样上抛维持兜底。
                    if _rpc_code_name(exc) != "ALREADY_EXISTS":
                        raise
                    stub.UpdateProvider(pb.UpdateProviderRequest(
                        provider=provider, workspace=workspace), timeout=30)
                    exists = True
        return {"name": name, "created": not exists}


def pb_grpc_stub(client):
    """OpenShell gRPC admin stub on the sandbox client's channel."""
    return openshell_pb2_grpc.OpenShellStub(client._channel)

#!/usr/bin/env python3
"""
依据: ADR-109（TP03-T2 契约夹具服务器）
依据: 03 §2 幂等规范（三态行为表）
依据: 09 §3 Redis DB=5, TTL 24h

契约夹具服务器 —— 通用幂等 Echo Servicer。
接收任何 gRPC 请求 → 按 03 §2 三态规则响应：
  1. 无 RequestMetadata → INVALID_ARGUMENT(3)
  2. 首次请求 → 记录并 echo 回响应
  3. 同键同体 → 返回首次记录的响应（幂等重放）
  4. 同键异体 → ALREADY_EXISTS(9)

启动：python3 fixture_server.py --port 50051
"""

import argparse
import hashlib
import os
import sys
import time
import threading
import signal
from concurrent import futures
from typing import Dict, Tuple, Optional

import grpc

# proto-gen/python 路径
_project_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
_proto_gen = os.path.join(_project_root, 'libs', 'proto-gen', 'python')
if _proto_gen not in sys.path:
    sys.path.insert(0, _proto_gen)

import codeaudit_common_pb2 as pb2
import codeaudit_common_pb2_grpc as pb2_grpc
from google.protobuf.empty_pb2 import Empty


# ============================================================
# 幂等去重存储（依据: 03 §2, 09 §3）
# ============================================================

class IdempotencyStore:
    """幂等去重存储接口"""
    def get(self, key: str) -> Optional[Tuple[bytes, bytes]]:
        """返回 (response_bytes, request_hash_bytes) 或 None"""
        raise NotImplementedError
    def put(self, key: str, response_bytes: bytes, request_hash_bytes: bytes):
        raise NotImplementedError


class MemoryIdempotencyStore(IdempotencyStore):
    """内存字典。TTL 24h（依据: 03 §2 去重窗口）"""
    def __init__(self):
        self._store: Dict[str, Tuple[bytes, bytes, float]] = {}
        self._lock = threading.Lock()
        self._ttl = 86400  # 24h
        self._running = True
        self._cleanup = threading.Thread(target=self._cleanup_loop, daemon=True)
        self._cleanup.start()

    def get(self, key: str) -> Optional[Tuple[bytes, bytes]]:
        with self._lock:
            entry = self._store.get(key)
            if entry is None:
                return None
            resp_bytes, hash_bytes, ts = entry
            if time.time() - ts > self._ttl:
                del self._store[key]
                return None
            return resp_bytes, hash_bytes

    def put(self, key: str, response_bytes: bytes, request_hash_bytes: bytes):
        with self._lock:
            self._store[key] = (response_bytes, request_hash_bytes, time.time())

    def _cleanup_loop(self):
        while self._running:
            time.sleep(3600)
            now = time.time()
            with self._lock:
                expired = [k for k, v in self._store.items() if now - v[2] > self._ttl]
                for k in expired:
                    del self._store[k]

    def stop(self):
        self._running = False


class RedisIdempotencyStore(IdempotencyStore):
    """Redis DB=5。依据: 09 §3"""
    def __init__(self, redis_url: str = "redis://localhost:6379/5"):
        import redis
        self._client = redis.from_url(redis_url, decode_responses=False)
        self._client.ping()

    def get(self, key: str) -> Optional[Tuple[bytes, bytes]]:
        data = self._client.get(f"idempotency:{key}")
        if data is None:
            return None
        import json
        entry = json.loads(data)
        return bytes.fromhex(entry['r']), bytes.fromhex(entry['h'])

    def put(self, key: str, response_bytes: bytes, request_hash_bytes: bytes):
        import json
        data = json.dumps({'r': response_bytes.hex(), 'h': request_hash_bytes.hex()})
        self._client.setex(f"idempotency:{key}", 86400, data)


def create_store() -> IdempotencyStore:
    t = os.environ.get('CODEAUDIT_IDEMPOTENCY_STORE', 'memory').lower()
    if t == 'redis':
        try:
            return RedisIdempotencyStore()
        except Exception as e:
            print(f"Redis 不可用({e})，回退内存")
    return MemoryIdempotencyStore()


# ============================================================
# 幂等处理核心（依据: 03 §2 三态行为表）
# ============================================================

def check_idempotency(store: IdempotencyStore, request, context,
                      default_response) -> Tuple[Optional[object], bool]:
    """
    通用幂等检查。返回 (response, handled)。
    handled=True: 已处理（成功返回或已 abort），调用方直接 return response。
    handled=False: 首次请求，调用方应构造响应并存储。

    依据: 03 §2 三态行为表
    """
    # 1. 无 metadata → INVALID_ARGUMENT(3)
    if not hasattr(request, 'metadata') or request.metadata.ByteSize() == 0:
        context.abort(grpc.StatusCode.INVALID_ARGUMENT,
                      "RequestMetadata required (03 §2)")
        return None, True

    meta = request.metadata
    if not meta.request_id:
        context.abort(grpc.StatusCode.INVALID_ARGUMENT,
                      "request_id required in RequestMetadata (03 §2)")
        return None, True

    # 幂等键: (caller_service, request_id)
    key = f"{meta.caller_service}:{meta.request_id}"
    req_hash = hashlib.sha256(request.SerializeToString()).digest()

    stored = store.get(key)

    if stored is not None:
        stored_resp_bytes, stored_req_hash = stored
        if req_hash == stored_req_hash:
            # 3. 同键同体 → 返回首次响应
            default_response.ParseFromString(stored_resp_bytes)
            return default_response, True
        else:
            # 4. 同键异体 → ALREADY_EXISTS(9)
            context.abort(grpc.StatusCode.ALREADY_EXISTS,
                          f"Idempotency conflict: key ({meta.caller_service},{meta.request_id})")
            return None, True

    # 2. 首次 → 需要调用方构造响应
    return None, False


def store_response(store: IdempotencyStore, request, response):
    """首次请求后存储响应"""
    meta = request.metadata
    key = f"{meta.caller_service}:{meta.request_id}"
    req_hash = hashlib.sha256(request.SerializeToString()).digest()
    store.put(key, response.SerializeToString(), req_hash)


# ============================================================
# 14 个 Service 的 Servicer
# ============================================================

class TaskServiceServicer(pb2_grpc.TaskServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def CreateScanTask(self, request, context):
        return self._write_rpc(request, context, pb2.ScanTask())
    def CancelScanTask(self, request, context):
        return self._write_rpc(request, context, pb2.ScanTask())
    def RetryScanTask(self, request, context):
        return self._write_rpc(request, context, pb2.ScanTask())
    # ADR-171 审批流废除：Submit/Approve/Reject RPC 已从 proto 删除
    def StartTask(self, request, context):
        return self._write_rpc(request, context, pb2.ScanTask())
    def CompleteTask(self, request, context):
        return self._write_rpc(request, context, pb2.ScanTask())
    def FailTask(self, request, context):
        return self._write_rpc(request, context, pb2.ScanTask())
    def UpdateStageStatus(self, request, context):
        return self._write_rpc(request, context, pb2.ScanTask())
    def ReportStageComplete(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()
    def ReportStageFailed(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()

    # Read RPCs
    def GetScanTask(self, request, context):
        return pb2.ScanTask()
    def ListScanTasks(self, request, context):
        return pb2.ListScanTasksResponse()
    def GetTaskProgress(self, request, context):
        return pb2.TaskProgress()
    def WatchTaskProgress(self, request, context):
        return iter([])
    def GetTaskContext(self, request, context):
        return pb2.TaskContext()


class ProjectServiceServicer(pb2_grpc.ProjectServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def CreateProject(self, request, context):
        return self._write_rpc(request, context, pb2.Project())
    def UpdateProject(self, request, context):
        return self._write_rpc(request, context, pb2.Project())
    def DeleteProject(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()
    def UpdateProjectConfig(self, request, context):
        return self._write_rpc(request, context, pb2.ProjectConfig())
    def AddProjectMember(self, request, context):
        return self._write_rpc(request, context, pb2.ProjectMember())
    def RemoveProjectMember(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()

    def GetProject(self, request, context):
        return pb2.Project()
    def ListProjects(self, request, context):
        return pb2.ListProjectsResponse()
    def GetProjectConfig(self, request, context):
        return pb2.ProjectConfig()
    def GetProjectStats(self, request, context):
        return pb2.ProjectStats()
    def ListProjectMembers(self, request, context):
        return pb2.ListProjectMembersResponse()


class UserServiceServicer(pb2_grpc.UserServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def Login(self, request, context):
        return self._write_rpc(request, context, pb2.LoginResponse())
    def Logout(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()
    def RefreshToken(self, request, context):
        return self._write_rpc(request, context, pb2.RefreshTokenResponse())
    def UpdateUser(self, request, context):
        return self._write_rpc(request, context, pb2.User())

    def GetCurrentUser(self, request, context):
        return pb2.User()
    def GetUser(self, request, context):
        return pb2.User()
    def ValidatePermission(self, request, context):
        return pb2.ValidatePermissionResponse()
    def GetUserPermissions(self, request, context):
        return pb2.UserPermissions()

    # ---- V2.1 用户生命周期 (ADR-205) ----
    def RegisterUser(self, request, context):
        return self._write_rpc(request, context, pb2.LoginResponse())
    def ListUsers(self, request, context):
        return pb2.ListUsersResponse()
    def CreateUser(self, request, context):
        return self._write_rpc(request, context, pb2.User())
    def ChangePassword(self, request, context):
        return self._write_rpc(request, context, Empty())
    def ResetPassword(self, request, context):
        return self._write_rpc(request, context, pb2.ResetPasswordResponse())


class ResultServiceServicer(pb2_grpc.ResultServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def CreateFinding(self, request, context):
        return self._write_rpc(request, context, pb2.AuditFinding())
    def UpdateFinding(self, request, context):
        return self._write_rpc(request, context, pb2.AuditFinding())
    def DeleteFinding(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()
    def BatchCreateFindings(self, request, context):
        return self._write_rpc(request, context, pb2.BatchCreateFindingsResponse())
    def BatchUpdateFindings(self, request, context):
        return self._write_rpc(request, context, pb2.BatchUpdateFindingsResponse())
    def BatchUpdateVerdict(self, request, context):
        return self._write_rpc(request, context, pb2.BatchUpdateVerdictResponse())
    def UpdateVerdict(self, request, context):
        return self._write_rpc(request, context, pb2.AuditFinding())
    def SubmitFindingFeedback(self, request, context):
        return self._write_rpc(request, context, pb2.SubmitFindingFeedbackResponse())

    def GetFinding(self, request, context):
        return pb2.AuditFinding()
    def ListFindings(self, request, context):
        return pb2.ListFindingsResponse()
    def GetFindingsByVerdict(self, request, context):
        return pb2.ListFindingsResponse()
    def GetTaskResultStats(self, request, context):
        return pb2.ResultStats()
    def ExportFindings(self, request, context):
        return pb2.ExportFindingsResponse()


class ReportServiceServicer(pb2_grpc.ReportServiceServicer):
    def __init__(self, store):
        self._s = store

    def GenerateReport(self, request, context):
        r, handled = check_idempotency(self._s, request, context, pb2.GenerateReportResponse())
        if handled:
            return r if r is not None else pb2.GenerateReportResponse()
        resp = pb2.GenerateReportResponse()
        store_response(self._s, request, resp)
        return resp

    def GetReport(self, request, context):
        return pb2.Report()
    def ListReports(self, request, context):
        return pb2.ListReportsResponse()
    def ListTemplates(self, request, context):
        return pb2.ListTemplatesResponse()
    def GetTemplate(self, request, context):
        return pb2.ReportTemplate()
    def DownloadReport(self, request, context):
        return iter([])


class DSHRuntimeServiceServicer(pb2_grpc.DSHRuntimeServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def RunAIAnalysis(self, request, context):
        return self._write_rpc(request, context, pb2.RunAIAnalysisResponse())
    def VerifySASTResults(self, request, context):
        return self._write_rpc(request, context, pb2.VerifySASTResultsResponse())
    def SearchMissedVulns(self, request, context):
        return self._write_rpc(request, context, pb2.SearchMissedVulnsResponse())
    def ReviewSASTResults(self, request, context):
        return self._write_rpc(request, context, pb2.ReviewSASTResultsResponse())
    def CancelAnalysis(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()

    def GetAnalysisProgress(self, request, context):
        return pb2.AnalysisProgress()
    def WatchAnalysisProgress(self, request, context):
        return iter([])
    def GetSessionStatus(self, request, context):
        return pb2.SessionStatus()


class CodeAnalysisServiceServicer(pb2_grpc.CodeAnalysisServiceServicer):
    def __init__(self, store):
        self._s = store

    def AnalyzeCode(self, request, context):
        r, handled = check_idempotency(self._s, request, context, pb2.AnalyzeCodeResponse())
        if handled:
            return r if r is not None else pb2.AnalyzeCodeResponse()
        resp = pb2.AnalyzeCodeResponse()
        store_response(self._s, request, resp)
        return resp

    def QueryCPG(self, request, context):
        return pb2.QueryCPGResponse()
    def GetCallGraph(self, request, context):
        return pb2.CallGraph()
    def GetDataFlow(self, request, context):
        return pb2.DataFlowGraph()
    def GetAnalysisProgress(self, request, context):
        return pb2.CodeAnalysisProgress()


# ADR-175: AIInferenceService 已从 proto 删除（服务随删）

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def Chat(self, request, context):
        return self._write_rpc(request, context, pb2.ChatResponse())
    def RuleScan(self, request, context):
        return self._write_rpc(request, context, pb2.RuleScanResponse())

    def ChatStream(self, request, context):
        return iter([])
    def ListModels(self, request, context):
        return pb2.ListModelsResponse()
    def GetModelInfo(self, request, context):
        return pb2.ModelInfo()
    def HealthCheck(self, request, context):
        return pb2.HealthCheckResponse()
    def ListRules(self, request, context):
        return pb2.ListRulesResponse()


class SASTAdapterServiceServicer(pb2_grpc.SASTAdapterServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def RunSASTScan(self, request, context):
        return self._write_rpc(request, context, pb2.RunSASTScanResponse())
    def RunMultipleScans(self, request, context):
        return self._write_rpc(request, context, pb2.RunMultipleScansResponse())

    def ListAvailableTools(self, request, context):
        return pb2.ListAvailableToolsResponse()
    def GetToolInfo(self, request, context):
        return pb2.SASTToolInfo()
    def ValidateToolConfig(self, request, context):
        return pb2.ValidateToolConfigResponse()
    def GetScanProgress(self, request, context):
        return pb2.ScanProgress()


class SASTFusionServiceServicer(pb2_grpc.SASTFusionServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc(self, request, context, resp):
        r, handled = check_idempotency(self._s, request, context, type(resp)())
        if handled:
            return r if r is not None else resp
        store_response(self._s, request, resp)
        return resp

    def FuseResults(self, request, context):
        return self._write_rpc(request, context, pb2.FuseResultsResponse())
    def UpdateFusionConfig(self, request, context):
        return self._write_rpc(request, context, pb2.FusionConfig())

    def AlignLocations(self, request, context):
        return pb2.AlignLocationsResponse()
    def ClusterFindings(self, request, context):
        return pb2.ClusterFindingsResponse()
    def ResolveConflicts(self, request, context):
        return pb2.ResolveConflictsResponse()
    def GetFusionConfig(self, request, context):
        return pb2.FusionConfig()
    def CompareResults(self, request, context):
        return pb2.CompareResultsResponse()
    def CalculateMetrics(self, request, context):
        return pb2.ComparisonMetrics()
    def GenerateComparisonReport(self, request, context):
        return pb2.ComparisonReport()


class StorageServiceServicer(pb2_grpc.StorageServiceServicer):
    def __init__(self, store):
        self._s = store

    def UploadFile(self, request_iterator, context):
        return pb2.StoredFile()

    def DeleteFile(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()

    def DownloadFile(self, request, context):
        return iter([])
    def GetPresignedUrl(self, request, context):
        return pb2.GetPresignedUrlResponse()
    def GetFileInfo(self, request, context):
        return pb2.StoredFile()
    def ListFiles(self, request, context):
        return pb2.ListFilesResponse()


class NotificationServiceServicer(pb2_grpc.NotificationServiceServicer):
    def __init__(self, store):
        self._s = store

    def _write_rpc_empty(self, request, context):
        r, handled = check_idempotency(self._s, request, context, Empty())
        if handled:
            return r if r is not None else Empty()
        store_response(self._s, request, Empty())
        return Empty()

    def SendNotification(self, request, context):
        return self._write_rpc_empty(request, context)
    def SendBatchNotification(self, request, context):
        return self._write_rpc_empty(request, context)
    def MarkNotificationRead(self, request, context):
        return self._write_rpc_empty(request, context)

    def ListNotifications(self, request, context):
        return pb2.ListNotificationsResponse()


# ============================================================
# 服务器注册与启动
# ============================================================

ALL_SERVICERS = {
    'TaskService': TaskServiceServicer,
    'ProjectService': ProjectServiceServicer,
    'UserService': UserServiceServicer,
    'ResultService': ResultServiceServicer,
    'ReportService': ReportServiceServicer,
    'DSHRuntimeService': DSHRuntimeServiceServicer,
    'CodeAnalysisService': CodeAnalysisServiceServicer,
    'SASTAdapterService': SASTAdapterServiceServicer,
    'SASTFusionService': SASTFusionServiceServicer,
    'StorageService': StorageServiceServicer,
    'NotificationService': NotificationServiceServicer,
}


def serve(port: int):
    store = create_store()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))

    for name, servicer_cls in ALL_SERVICERS.items():
        servicer = servicer_cls(store)
        register_fn = getattr(pb2_grpc, f'add_{name}Servicer_to_server', None)
        if register_fn:
            register_fn(servicer, server)
            print(f"  注册: {name}")

    server.add_insecure_port(f'[::]:{port}')
    server.start()
    print(f"契约夹具服务器已启动，端口: {port}")

    stop_event = threading.Event()
    def _handler(sig, frame):
        stop_event.set()
    signal.signal(signal.SIGINT, _handler)
    signal.signal(signal.SIGTERM, _handler)
    stop_event.wait()
    server.stop(grace=5)
    if isinstance(store, MemoryIdempotencyStore):
        store.stop()
    print("夹具服务器已停止")


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='CodeAudit 契约夹具服务器')
    parser.add_argument('--port', type=int, default=50051)
    args = parser.parse_args()
    serve(args.port)

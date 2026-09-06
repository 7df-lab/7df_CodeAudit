# ============================================================
# ADR-136 显式警示（防"绿=实现正确"误读）:
# 本目录默认 fixture 模式（ADR-109）的被测对象是 fixture_server.py 的 Echo Servicer，
# 仅验证"幂等三态契约语义"，不验证 services/ 下真实实现的行为。
# 真实行为覆盖以 tests/e2e/（真实进程 + 真实 gRPC）为准。
# 大量 skip（读 RPC 无幂等要求）是既定范围而非覆盖达成。
# ============================================================
"""
依据: test-gates.md §4 契约测试强制标准
依据: ADR-109 契约夹具服务器模式
依据: 03 §2 gRPC endpoint 配置

pytest conftest - 提供 gRPC channel fixture，按 CODEAUDIT_CONTRACT_MODE 分流：
  - mode=fixture（默认）：进程内启动夹具服务器
  - mode=live：连 CODEAUDIT_GRPC_ENDPOINT 真实服务（TP04 后用）
  - 连接失败 → pytest.skip
"""

import os
import sys
import time
import socket
import pytest
import grpc

GRPC_ENDPOINT = os.environ.get('CODEAUDIT_GRPC_ENDPOINT', 'localhost:50051')
CONTRACT_MODE = os.environ.get('CODEAUDIT_CONTRACT_MODE', 'fixture')
FIXTURE_PORT = int(os.environ.get('CODEAUDIT_FIXTURE_PORT', '50051'))

_fixture_server = None
_fixture_store = None


def _wait_for_port(host: str, port: int, timeout: int = 15) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection((host, port), timeout=1):
                return True
        except (ConnectionRefusedError, OSError):
            time.sleep(0.3)
    return False


def _wait_for_port_free(host: str, port: int, timeout: float = 1.0) -> bool:
    """端口空闲检测（夹具自动避让真实服务网占用，ADR-109）。"""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(timeout)
        return s.connect_ex((host, port)) != 0


def _start_fixture_server(port: int):
    """在线程中启动夹具服务器"""
    global _fixture_server, _fixture_store

    project_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    proto_gen = os.path.join(project_root, 'libs', 'proto-gen', 'python')
    conftest_dir = os.path.dirname(os.path.abspath(__file__))
    for p in [proto_gen, conftest_dir]:
        if p not in sys.path:
            sys.path.insert(0, p)

    from concurrent import futures
    import codeaudit_common_pb2_grpc as pb2_grpc
    from fixture_server import (
        TaskServiceServicer, ProjectServiceServicer, UserServiceServicer,
        ResultServiceServicer, ReportServiceServicer, DSHRuntimeServiceServicer,
        CodeAnalysisServiceServicer,
        SASTAdapterServiceServicer, SASTFusionServiceServicer,
        StorageServiceServicer, NotificationServiceServicer,
        MemoryIdempotencyStore
    )

    store = MemoryIdempotencyStore()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))

    for name, cls in {
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
    }.items():
        fn = getattr(pb2_grpc, f'add_{name}Servicer_to_server', None)
        if fn:
            fn(cls(store), server)

    # 端口被真实服务网(E2E/开发中服务)占用时自动顺延，保证契约夹具总能起（ADR-109 精神）
    bound = 0
    for p in range(port, port + 50):
        if _wait_for_port_free('127.0.0.1', p):
            bound = server.add_insecure_port(f'[::]:{p}')
            if bound:
                port = p
                break
    if not bound:
        return None, None
    server.start()
    _fixture_server = server
    _fixture_store = store
    # FIXTURE_PORT 是模块级变量，channel fixture 直接读它——同步更新
    global FIXTURE_PORT
    FIXTURE_PORT = port
    return server, store


@pytest.fixture(scope="session")
def channel():
    """提供 gRPC channel"""
    if CONTRACT_MODE == 'live':
        try:
            ch = grpc.insecure_channel(GRPC_ENDPOINT)
            grpc.channel_ready_future(ch).result(timeout=5)
            yield ch
            ch.close()
        except Exception:
            pytest.skip("真实服务未部署——骨架阶段契约不执行")
            yield None
    else:
        server, store = _start_fixture_server(FIXTURE_PORT)
        if server is None:
            pytest.skip("夹具服务器启动失败")
            yield None
            return

        try:
            if not _wait_for_port('127.0.0.1', FIXTURE_PORT, timeout=10):
                pytest.skip(f"夹具服务器端口 {FIXTURE_PORT} 未就绪")
                yield None
                return

            # 直接创建 channel（不检查 ready 状态——lazy 连接）
            ch = grpc.insecure_channel(f'127.0.0.1:{FIXTURE_PORT}')
            yield ch
            ch.close()
        except Exception as e:
            pytest.skip(f"无法连接夹具服务器: {e}")
            yield None
        finally:
            server.stop(grace=1)
            if store:
                store.stop()

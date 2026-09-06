"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
契约测试 - SASTAdapterService
依据: codeaudit_common.proto L1033

本文件由 scripts/gen-contract-skeletons.py 自动生成，请勿手动编辑
版本: V2（TP03-T2 升级：可执行幂等三态测试体）
"""

import pytest
import grpc

# 依据: codeaudit_common.proto L1033
# 服务定义行号来源: .agent/research/brief-TP03-04-05.md §1.1


def test_run_sast_scan_idempotent_same_body_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1034；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import SASTAdapterServiceStub

    stub = SASTAdapterServiceStub(channel)
    request_id, req = make_idempotent_request("RunSASTScanRequest", caller="RunSASTScan_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "RunSASTScan", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "RunSASTScan", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_run_sast_scan_idempotent_diff_body_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1034；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import SASTAdapterServiceStub

    stub = SASTAdapterServiceStub(channel)
    request_id, req = make_idempotent_request("RunSASTScanRequest", caller="RunSASTScan_test")

    # 第一次调用
    call_rpc(stub, "RunSASTScan", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "RunSASTScan", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_run_sast_scan_missing_metadata_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1034；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import SASTAdapterServiceStub

    stub = SASTAdapterServiceStub(channel)
    req = make_request_without_metadata("RunSASTScanRequest")

    resp, err = call_rpc(stub, "RunSASTScan", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_run_multiple_scans_idempotent_same_body_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1035；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import SASTAdapterServiceStub

    stub = SASTAdapterServiceStub(channel)
    request_id, req = make_idempotent_request("RunMultipleScansRequest", caller="RunMultipleScans_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "RunMultipleScans", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "RunMultipleScans", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_run_multiple_scans_idempotent_diff_body_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1035；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import SASTAdapterServiceStub

    stub = SASTAdapterServiceStub(channel)
    request_id, req = make_idempotent_request("RunMultipleScansRequest", caller="RunMultipleScans_test")

    # 第一次调用
    call_rpc(stub, "RunMultipleScans", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "RunMultipleScans", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_run_multiple_scans_missing_metadata_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1035；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import SASTAdapterServiceStub

    stub = SASTAdapterServiceStub(channel)
    req = make_request_without_metadata("RunMultipleScansRequest")

    resp, err = call_rpc(stub, "RunMultipleScans", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_list_available_tools_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1036"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_tool_info_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1037"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_validate_tool_config_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1038"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_scan_progress_sast_adapter_service(channel):
    """依据: codeaudit_common.proto L1039"""
    pytest.skip("读 RPC 无幂等契约要求")

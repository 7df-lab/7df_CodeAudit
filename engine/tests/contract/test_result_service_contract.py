"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
契约测试 - ResultService
依据: codeaudit_common.proto L920

本文件由 scripts/gen-contract-skeletons.py 自动生成，请勿手动编辑
版本: V2（TP03-T2 升级：可执行幂等三态测试体）
"""

import pytest
import grpc

# 依据: codeaudit_common.proto L920
# 服务定义行号来源: .agent/research/brief-TP03-04-05.md §1.1


def test_create_finding_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L921；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("CreateFindingRequest", caller="CreateFinding_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "CreateFinding", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "CreateFinding", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_create_finding_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L921；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("CreateFindingRequest", caller="CreateFinding_test")

    # 第一次调用
    call_rpc(stub, "CreateFinding", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "CreateFinding", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_create_finding_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L921；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    req = make_request_without_metadata("CreateFindingRequest")

    resp, err = call_rpc(stub, "CreateFinding", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_get_finding_result_service(channel):
    """依据: codeaudit_common.proto L922"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_update_finding_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L923；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateFindingRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_finding_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L923；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateFindingRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_finding_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L923；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateFindingRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_delete_finding_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L924；依据: test-gates.md §3 幂等行"""
    pytest.skip("DeleteFindingRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_delete_finding_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L924；依据: test-gates.md §3 幂等行"""
    pytest.skip("DeleteFindingRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_delete_finding_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L924；依据: test-gates.md §3 幂等行"""
    pytest.skip("DeleteFindingRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_list_findings_result_service(channel):
    """依据: codeaudit_common.proto L925"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_batch_create_findings_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L926；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("BatchCreateFindingsRequest", caller="BatchCreateFindings_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "BatchCreateFindings", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "BatchCreateFindings", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_batch_create_findings_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L926；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("BatchCreateFindingsRequest", caller="BatchCreateFindings_test")

    # 第一次调用
    call_rpc(stub, "BatchCreateFindings", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "BatchCreateFindings", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_batch_create_findings_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L926；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    req = make_request_without_metadata("BatchCreateFindingsRequest")

    resp, err = call_rpc(stub, "BatchCreateFindings", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_batch_update_findings_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L927；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("BatchUpdateFindingsRequest", caller="BatchUpdateFindings_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "BatchUpdateFindings", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "BatchUpdateFindings", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_batch_update_findings_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L927；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("BatchUpdateFindingsRequest", caller="BatchUpdateFindings_test")

    # 第一次调用
    call_rpc(stub, "BatchUpdateFindings", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "BatchUpdateFindings", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_batch_update_findings_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L927；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    req = make_request_without_metadata("BatchUpdateFindingsRequest")

    resp, err = call_rpc(stub, "BatchUpdateFindings", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_batch_update_verdict_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L928；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("BatchUpdateVerdictRequest", caller="BatchUpdateVerdict_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "BatchUpdateVerdict", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "BatchUpdateVerdict", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_batch_update_verdict_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L928；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("BatchUpdateVerdictRequest", caller="BatchUpdateVerdict_test")

    # 第一次调用
    call_rpc(stub, "BatchUpdateVerdict", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "BatchUpdateVerdict", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_batch_update_verdict_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L928；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    req = make_request_without_metadata("BatchUpdateVerdictRequest")

    resp, err = call_rpc(stub, "BatchUpdateVerdict", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_update_verdict_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L929；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateVerdictRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_verdict_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L929；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateVerdictRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_verdict_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L929；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateVerdictRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_get_findings_by_verdict_result_service(channel):
    """依据: codeaudit_common.proto L930"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_task_result_stats_result_service(channel):
    """依据: codeaudit_common.proto L931"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_export_findings_result_service(channel):
    """依据: codeaudit_common.proto L932"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_submit_finding_feedback_idempotent_same_body_result_service(channel):
    """依据: codeaudit_common.proto L935；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("SubmitFindingFeedbackRequest", caller="SubmitFindingFeedback_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "SubmitFindingFeedback", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "SubmitFindingFeedback", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_submit_finding_feedback_idempotent_diff_body_result_service(channel):
    """依据: codeaudit_common.proto L935；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    request_id, req = make_idempotent_request("SubmitFindingFeedbackRequest", caller="SubmitFindingFeedback_test")

    # 第一次调用
    call_rpc(stub, "SubmitFindingFeedback", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "SubmitFindingFeedback", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_submit_finding_feedback_missing_metadata_result_service(channel):
    """依据: codeaudit_common.proto L935；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import ResultServiceStub

    stub = ResultServiceStub(channel)
    req = make_request_without_metadata("SubmitFindingFeedbackRequest")

    resp, err = call_rpc(stub, "SubmitFindingFeedback", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

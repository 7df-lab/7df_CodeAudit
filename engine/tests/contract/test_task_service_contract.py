"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
契约测试 - TaskService
依据: codeaudit_common.proto L858

本文件由 scripts/gen-contract-skeletons.py 自动生成，请勿手动编辑
版本: V2（TP03-T2 升级：可执行幂等三态测试体）
"""

import pytest
import grpc

# 依据: codeaudit_common.proto L858
# 服务定义行号来源: .agent/research/brief-TP03-04-05.md §1.1


def test_create_scan_task_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L860；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    request_id, req = make_idempotent_request("CreateScanTaskRequest", caller="CreateScanTask_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "CreateScanTask", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "CreateScanTask", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_create_scan_task_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L860；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    request_id, req = make_idempotent_request("CreateScanTaskRequest", caller="CreateScanTask_test")

    # 第一次调用
    call_rpc(stub, "CreateScanTask", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "CreateScanTask", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_create_scan_task_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L860；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    req = make_request_without_metadata("CreateScanTaskRequest")

    resp, err = call_rpc(stub, "CreateScanTask", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_get_scan_task_task_service(channel):
    """依据: codeaudit_common.proto L861"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_cancel_scan_task_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L862；依据: test-gates.md §3 幂等行"""
    pytest.skip("CancelScanTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_cancel_scan_task_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L862；依据: test-gates.md §3 幂等行"""
    pytest.skip("CancelScanTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_cancel_scan_task_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L862；依据: test-gates.md §3 幂等行"""
    pytest.skip("CancelScanTaskRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_retry_scan_task_task_service(channel):
    """依据: codeaudit_common.proto L863"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_list_scan_tasks_task_service(channel):
    """依据: codeaudit_common.proto L864"""
    pytest.skip("读 RPC 无幂等契约要求")

# ADR-171 审批流废除：SubmitTask/ApproveTask/RejectTask RPC 已从 proto 删除，
# 对应幂等契约用例随之移除（2026-09-01 人类裁定：有创建权限即有启动权限）。

def test_start_task_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L872；依据: test-gates.md §3 幂等行"""
    pytest.skip("StartTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_start_task_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L872；依据: test-gates.md §3 幂等行"""
    pytest.skip("StartTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_start_task_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L872；依据: test-gates.md §3 幂等行"""
    pytest.skip("StartTaskRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_complete_task_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L873；依据: test-gates.md §3 幂等行"""
    pytest.skip("CompleteTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_complete_task_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L873；依据: test-gates.md §3 幂等行"""
    pytest.skip("CompleteTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_complete_task_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L873；依据: test-gates.md §3 幂等行"""
    pytest.skip("CompleteTaskRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_fail_task_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L874；依据: test-gates.md §3 幂等行"""
    pytest.skip("FailTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_fail_task_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L874；依据: test-gates.md §3 幂等行"""
    pytest.skip("FailTaskRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_fail_task_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L874；依据: test-gates.md §3 幂等行"""
    pytest.skip("FailTaskRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_update_stage_status_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L875；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateStageStatusRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_stage_status_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L875；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateStageStatusRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_stage_status_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L875；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateStageStatusRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_get_task_progress_task_service(channel):
    """依据: codeaudit_common.proto L878"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_watch_task_progress_task_service(channel):
    """依据: codeaudit_common.proto L879"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_report_stage_complete_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L880；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    request_id, req = make_idempotent_request("ReportStageCompleteRequest", caller="ReportStageComplete_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "ReportStageComplete", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "ReportStageComplete", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_report_stage_complete_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L880；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    request_id, req = make_idempotent_request("ReportStageCompleteRequest", caller="ReportStageComplete_test")

    # 第一次调用
    call_rpc(stub, "ReportStageComplete", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "ReportStageComplete", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_report_stage_complete_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L880；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    req = make_request_without_metadata("ReportStageCompleteRequest")

    resp, err = call_rpc(stub, "ReportStageComplete", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_report_stage_failed_idempotent_same_body_task_service(channel):
    """依据: codeaudit_common.proto L881；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    request_id, req = make_idempotent_request("ReportStageFailedRequest", caller="ReportStageFailed_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "ReportStageFailed", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "ReportStageFailed", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"

def test_report_stage_failed_idempotent_diff_body_task_service(channel):
    """依据: codeaudit_common.proto L881；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    request_id, req = make_idempotent_request("ReportStageFailedRequest", caller="ReportStageFailed_test")

    # 第一次调用
    call_rpc(stub, "ReportStageFailed", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "ReportStageFailed", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"

def test_report_stage_failed_missing_metadata_task_service(channel):
    """依据: codeaudit_common.proto L881；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import TaskServiceStub

    stub = TaskServiceStub(channel)
    req = make_request_without_metadata("ReportStageFailedRequest")

    resp, err = call_rpc(stub, "ReportStageFailed", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"

def test_get_task_context_task_service(channel):
    """依据: codeaudit_common.proto L882"""
    pytest.skip("读 RPC 无幂等契约要求")

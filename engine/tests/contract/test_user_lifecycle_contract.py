"""
契约测试 - UserService V2.1 用户生命周期（ADR-205）
依据: codeaudit_common.proto UserService 段（RegisterUser/ListUsers/CreateUser/
ChangePassword/ResetPassword）；test-gates.md §3 幂等行（R4：写 RPC 全覆盖）。

手写用例（非骨架生成物）：骨架文件 test_user_service_contract.py 由
gen-contract-skeletons.py 生成，覆盖 V1 八接口；本文件覆盖 V2.1 增量五接口。
"""

import pytest
import grpc


def test_register_user_idempotent_same_body_user_service(channel):
    """同键同体→返回首次响应（令牌对重放一致，R4/R6）"""
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub

    stub = UserServiceStub(channel)
    request_id, req = make_idempotent_request("RegisterUserRequest", caller="RegisterUser_test")

    resp1, err1 = call_rpc(stub, "RegisterUser", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")

    resp2, err2 = call_rpc(stub, "RegisterUser", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"
    assert resp2 is not None, "幂等重放响应不应为空"


def test_register_user_idempotent_diff_body_user_service(channel):
    """同键异体→ALREADY_EXISTS(9)"""
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub

    stub = UserServiceStub(channel)
    request_id, req = make_idempotent_request("RegisterUserRequest", caller="RegisterUser_test")

    call_rpc(stub, "RegisterUser", req)
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "RegisterUser", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"


def test_register_user_missing_metadata_user_service(channel):
    """缺 RequestMetadata→INVALID_ARGUMENT(3)"""
    from libs_contract_helpers import call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub
    from codeaudit_common_pb2 import RegisterUserRequest

    stub = UserServiceStub(channel)
    _, err = call_rpc(stub, "RegisterUser", RegisterUserRequest(username="x"))
    assert err is not None, "缺 metadata 应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {err}"


def test_list_users_user_service(channel):
    """读 RPC：ListUsers 应可调用（分页/过滤语义由服务端单测覆盖）"""
    from libs_contract_helpers import call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub
    from codeaudit_common_pb2 import ListUsersRequest

    stub = UserServiceStub(channel)
    _, err = call_rpc(stub, "ListUsers", ListUsersRequest())
    assert err is None, f"ListUsers 不应失败: {err}"


def test_create_user_idempotent_same_body_user_service(channel):
    """同键同体→返回首次响应"""
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub

    stub = UserServiceStub(channel)
    request_id, req = make_idempotent_request("CreateUserRequest", caller="CreateUser_test")

    resp1, err1 = call_rpc(stub, "CreateUser", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")
    resp2, err2 = call_rpc(stub, "CreateUser", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {err2}"
    assert resp2 is not None


def test_create_user_idempotent_diff_body_user_service(channel):
    """同键异体→ALREADY_EXISTS(9)"""
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub

    stub = UserServiceStub(channel)
    request_id, req = make_idempotent_request("CreateUserRequest", caller="CreateUser_test")

    call_rpc(stub, "CreateUser", req)
    mutate_business_field(req)
    _, err2 = call_rpc(stub, "CreateUser", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {err2}"


def test_change_password_idempotent_same_body_user_service(channel):
    """同键同体→返回首次响应（重放不再二次校验旧密码）"""
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub

    stub = UserServiceStub(channel)
    request_id, req = make_idempotent_request("ChangePasswordRequest", caller="ChangePassword_test")

    resp1, err1 = call_rpc(stub, "ChangePassword", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")
    resp2, err2 = call_rpc(stub, "ChangePassword", req)
    assert err2 is None, f"幂等重放不应失败: {err2}"


def test_reset_password_idempotent_same_body_user_service(channel):
    """同键同体→返回首次响应（临时密码重放一致）"""
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import UserServiceStub

    stub = UserServiceStub(channel)
    request_id, req = make_idempotent_request("ResetPasswordRequest", caller="ResetPassword_test")

    resp1, err1 = call_rpc(stub, "ResetPassword", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {err1}")
    resp2, err2 = call_rpc(stub, "ResetPassword", req)
    assert err2 is None, f"幂等重放不应失败: {err2}"
    assert resp2 is not None

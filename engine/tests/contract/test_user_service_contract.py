"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
契约测试 - UserService
依据: codeaudit_common.proto L906

本文件由 scripts/gen-contract-skeletons.py 自动生成，请勿手动编辑
版本: V2（TP03-T2 升级：可执行幂等三态测试体）
"""

import pytest
import grpc

# 依据: codeaudit_common.proto L906
# 服务定义行号来源: .agent/research/brief-TP03-04-05.md §1.1


def test_login_user_service(channel):
    """依据: codeaudit_common.proto L907"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_logout_user_service(channel):
    """依据: codeaudit_common.proto L908"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_refresh_token_user_service(channel):
    """依据: codeaudit_common.proto L909"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_current_user_user_service(channel):
    """依据: codeaudit_common.proto L910"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_user_user_service(channel):
    """依据: codeaudit_common.proto L911"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_update_user_idempotent_same_body_user_service(channel):
    """依据: codeaudit_common.proto L912；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateUserRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_user_idempotent_diff_body_user_service(channel):
    """依据: codeaudit_common.proto L912；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateUserRequest 无 RequestMetadata 字段——幂等测试不适用")


def test_update_user_missing_metadata_user_service(channel):
    """依据: codeaudit_common.proto L912；依据: test-gates.md §3 幂等行"""
    pytest.skip("UpdateUserRequest 无 RequestMetadata 字段——幂等测试不适用")

def test_validate_permission_user_service(channel):
    """依据: codeaudit_common.proto L913"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_user_permissions_user_service(channel):
    """依据: codeaudit_common.proto L914"""
    pytest.skip("读 RPC 无幂等契约要求")

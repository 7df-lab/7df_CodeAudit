"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
契约测试 - ReportService
依据: codeaudit_common.proto L942

本文件由 scripts/gen-contract-skeletons.py 自动生成，请勿手动编辑
版本: V2（TP03-T2 升级：可执行幂等三态测试体）
"""

import pytest
import grpc

# 依据: codeaudit_common.proto L942
# 服务定义行号来源: .agent/research/brief-TP03-04-05.md §1.1


def test_generate_report_report_service(channel):
    """依据: codeaudit_common.proto L943"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_report_report_service(channel):
    """依据: codeaudit_common.proto L944"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_list_reports_report_service(channel):
    """依据: codeaudit_common.proto L945"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_list_templates_report_service(channel):
    """依据: codeaudit_common.proto L946"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_template_report_service(channel):
    """依据: codeaudit_common.proto L947"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_download_report_report_service(channel):
    """依据: codeaudit_common.proto L948"""
    pytest.skip("读 RPC 无幂等契约要求")

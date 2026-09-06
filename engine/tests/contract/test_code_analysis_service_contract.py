"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
契约测试 - CodeAnalysisService
依据: codeaudit_common.proto L979

本文件由 scripts/gen-contract-skeletons.py 自动生成，请勿手动编辑
版本: V2（TP03-T2 升级：可执行幂等三态测试体）
"""

import pytest
import grpc

# 依据: codeaudit_common.proto L979
# 服务定义行号来源: .agent/research/brief-TP03-04-05.md §1.1


def test_analyze_code_code_analysis_service(channel):
    """依据: codeaudit_common.proto L980"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_query_cpg_code_analysis_service(channel):
    """依据: codeaudit_common.proto L981"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_call_graph_code_analysis_service(channel):
    """依据: codeaudit_common.proto L982"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_data_flow_code_analysis_service(channel):
    """依据: codeaudit_common.proto L983"""
    pytest.skip("读 RPC 无幂等契约要求")

def test_get_analysis_progress_code_analysis_service(channel):
    """依据: codeaudit_common.proto L984"""
    pytest.skip("读 RPC 无幂等契约要求")

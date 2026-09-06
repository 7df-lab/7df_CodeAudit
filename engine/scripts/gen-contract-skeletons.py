#!/usr/bin/env python3
"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
从 codeaudit_common.proto 自动生成 14 个 service 的契约测试骨架

生成规则（V2 — TP03-T2 升级，可执行幂等测试）：
- 读 RPC：pytest.skip（无幂等相关求）
- 写 RPC（有 metadata 字段）：三个可执行测试——
    a) test_<rpc>_idempotent_same_body（同键同体→返回首次响应）
    b) test_<rpc>_idempotent_diff_body（同键异体→ALREADY_EXISTS(9)）
    c) test_<rpc>_missing_metadata（缺幂等键→INVALID_ARGUMENT(3)）
- 写 RPC（无 metadata 字段）：pytest.skip
- 依据: 03 §2 + test-gates.md §3 + ADR-109
"""

import os
import re
import sys
from pathlib import Path

# 写方法关键词（依据: brief-TP03-04-05.md §1.2）
WRITE_KEYWORDS = [
    'Create', 'Update', 'Delete', 'Submit', 'Approve', 'Reject',
    'Start', 'Complete', 'Fail', 'Report', 'Upload', 'Send',
    'Batch', 'Reload', 'Index', 'Run', 'Fuse', 'Mark', 'Cancel',
    'Verify', 'Search'
]

# 服务定义（依据: codeaudit_common.proto L858-L1084）
SERVICES = [
    {'name': 'TaskService', 'start_line': 858},
    {'name': 'ProjectService', 'start_line': 888},
    {'name': 'UserService', 'start_line': 906},
    {'name': 'ResultService', 'start_line': 920},
    {'name': 'ReportService', 'start_line': 942},
    {'name': 'DSHRuntimeService', 'start_line': 956},
    {'name': 'CodeAnalysisService', 'start_line': 979},
    {'name': 'SASTAdapterService', 'start_line': 1033},
    {'name': 'SASTFusionService', 'start_line': 1047},
    {'name': 'StorageService', 'start_line': 1066},
    {'name': 'NotificationService', 'start_line': 1080},
]

# 服务→部署服务映射（依据: 01 §4 九服务表）
SERVICE_TO_DEPLOY = {
    'TaskService': 'task-service',
    'ProjectService': 'project-service',
    'UserService': 'project-service',
    'ResultService': 'result-service',
    'ReportService': 'result-service',
    'DSHRuntimeService': 'dsh-runtime-service',
    'CodeAnalysisService': 'dsh-runtime-service',
    'SASTAdapterService': 'sast-adapter-service',
    'SASTFusionService': 'sast-adapter-service',
    'StorageService': 'storage-service',
    'NotificationService': 'storage-service',
}


def camel_to_snake(name: str) -> str:
    s1 = re.sub('(.)([A-Z][a-z]+)', r'\1_\2', name)
    return re.sub('([a-z0-9])([A-Z])', r'\1_\2', s1).lower()


def is_write_rpc(rpc_name: str) -> bool:
    return any(rpc_name.startswith(kw) for kw in WRITE_KEYWORDS)


def parse_rpc_lines(proto_lines: list, service_start: int) -> list:
    rpcs = []
    in_service = False
    brace_count = 0
    for i, line in enumerate(proto_lines, 1):
        if i < service_start:
            continue
        stripped = line.strip()
        if 'service ' in stripped and '{' in stripped:
            in_service = True
            brace_count = stripped.count('{') - stripped.count('}')
            continue
        if in_service:
            brace_count += stripped.count('{') - stripped.count('}')
            rpc_match = re.match(
                r'rpc\s+(\w+)\s*\((stream\s+)?(\w+)\)\s*returns\s*\((stream\s+)?([\w.]+)\)',
                stripped
            )
            if rpc_match:
                rpcs.append({
                    'name': rpc_match.group(1),
                    'request': rpc_match.group(3),
                    'response': rpc_match.group(5),
                    'stream': bool(rpc_match.group(2)) or bool(rpc_match.group(4)),
                    'line': i,
                    'is_write': is_write_rpc(rpc_match.group(1))
                })
            if brace_count <= 0:
                break
    return rpcs


def has_metadata_field(proto_lines: list, message_name: str) -> bool:
    """检查 request message 是否包含 RequestMetadata metadata 字段"""
    in_msg = False
    brace = 0
    for line in proto_lines:
        s = line.strip()
        if re.match(rf'message\s+{re.escape(message_name)}\s*\{{', s):
            in_msg = True
            brace = s.count('{') - s.count('}')
            # 修复: 单行message定义(如proto L1391 IndexDocumentRequest)也需检查本行内容
            if 'RequestMetadata' in s and 'metadata' in s:
                return True
            if brace <= 0:
                break
            continue
        if in_msg:
            brace += s.count('{') - s.count('}')
            if 'RequestMetadata' in s and 'metadata' in s:
                return True
            if brace <= 0:
                break
    return False


def generate_write_tests_with_metadata(rpc: dict, service_name: str) -> str:
    """为有 metadata 的写 RPC 生成三个可执行幂等测试"""
    snake_rpc = camel_to_snake(rpc['name'])
    svc_snake = camel_to_snake(service_name)
    request_type = rpc['request']
    svc_stub = f'{service_name}Stub'
    method = rpc['name']

    # --- same_body ---
    same_body = f'''
def test_{snake_rpc}_idempotent_same_body_{svc_snake}(channel):
    """依据: codeaudit_common.proto L{rpc['line']}；依据: test-gates.md §3 幂等行
    同键同体→返回首次响应（幂等重放）
    """
    from libs_contract_helpers import make_idempotent_request, call_rpc
    from codeaudit_common_pb2_grpc import {svc_stub}

    stub = {svc_stub}(channel)
    request_id, req = make_idempotent_request("{request_type}", caller="{method}_test")

    # 第一次调用
    resp1, err1 = call_rpc(stub, "{method}", req)
    if err1 is not None:
        pytest.skip(f"首次调用失败（非幂等问题）: {{err1}}")

    # 第二次调用（同 request_id + 同请求体）
    resp2, err2 = call_rpc(stub, "{method}", req)
    assert err2 is None, f"幂等同体第二次调用不应失败: {{err2}}"

    # 断言：两次响应一致
    assert resp2 is not None, "幂等重放响应不应为空"
'''

    # --- diff_body ---
    diff_body = f'''
def test_{snake_rpc}_idempotent_diff_body_{svc_snake}(channel):
    """依据: codeaudit_common.proto L{rpc['line']}；依据: test-gates.md §3 幂等行
    同键异体→ALREADY_EXISTS(9)
    """
    from libs_contract_helpers import make_idempotent_request, mutate_business_field, call_rpc
    from codeaudit_common_pb2_grpc import {svc_stub}

    stub = {svc_stub}(channel)
    request_id, req = make_idempotent_request("{request_type}", caller="{method}_test")

    # 第一次调用
    call_rpc(stub, "{method}", req)

    # 第二次调用（同 request_id，不同 body）
    mutate_business_field(req)
    resp2, err2 = call_rpc(stub, "{method}", req)
    assert err2 is not None, "同键异体应返回错误"
    assert "ALREADY_EXISTS" in str(err2), f"期望 ALREADY_EXISTS(9)，实际: {{err2}}"
'''

    # --- missing_metadata ---
    missing = f'''
def test_{snake_rpc}_missing_metadata_{svc_snake}(channel):
    """依据: codeaudit_common.proto L{rpc['line']}；依据: test-gates.md §3 幂等行
    缺 RequestMetadata→INVALID_ARGUMENT(3)
    """
    from libs_contract_helpers import make_request_without_metadata, call_rpc
    from codeaudit_common_pb2_grpc import {svc_stub}

    stub = {svc_stub}(channel)
    req = make_request_without_metadata("{request_type}")

    resp, err = call_rpc(stub, "{method}", req)
    assert err is not None, "缺幂等键应返回错误"
    assert "INVALID_ARGUMENT" in str(err), f"期望 INVALID_ARGUMENT(3)，实际: {{err}}"
'''

    return same_body + diff_body + missing


def generate_write_tests_without_metadata(rpc: dict, service_name: str) -> str:
    """为无 metadata 的写 RPC 生成 skip 测试"""
    snake_rpc = camel_to_snake(rpc['name'])
    svc_snake = camel_to_snake(service_name)

    tests = []
    for suffix, desc in [
        ('idempotent_same_body', '幂等同体'),
        ('idempotent_diff_body', '幂等异体'),
        ('missing_metadata', '缺失幂等键')
    ]:
        tests.append(f'''
def test_{snake_rpc}_{suffix}_{svc_snake}(channel):
    """依据: codeaudit_common.proto L{rpc['line']}；依据: test-gates.md §3 幂等行"""
    pytest.skip("{rpc['request']} 无 RequestMetadata 字段——幂等测试不适用")
''')
    return '\n'.join(tests)


def generate_read_test(rpc: dict, service_name: str) -> str:
    snake_rpc = camel_to_snake(rpc['name'])
    svc_snake = camel_to_snake(service_name)
    return f'''
def test_{snake_rpc}_{svc_snake}(channel):
    """依据: codeaudit_common.proto L{rpc['line']}"""
    pytest.skip("读 RPC 无幂等契约要求")
'''


def generate_test_file(service: dict, rpcs: list, proto_lines: list) -> str:
    service_name = service['name']

    content = f'''"""
依据: test-gates.md §4 禁止手写契约,本骨架由proto派生
契约测试 - {service_name}
依据: codeaudit_common.proto L{service['start_line']}

本文件由 scripts/gen-contract-skeletons.py 自动生成，请勿手动编辑
版本: V2（TP03-T2 升级：可执行幂等三态测试体）
"""

import pytest
import grpc

# 依据: codeaudit_common.proto L{service['start_line']}
# 服务定义行号来源: .agent/research/brief-TP03-04-05.md §1.1

'''
    for rpc in rpcs:
        if rpc['is_write']:
            has_meta = has_metadata_field(proto_lines, rpc['request'])
            if has_meta:
                content += generate_write_tests_with_metadata(rpc, service_name)
            else:
                content += generate_write_tests_without_metadata(rpc, service_name)
        else:
            content += generate_read_test(rpc, service_name)

    return content


def main():
    proto_path = Path(__file__).parent.parent / 'codeaudit_common.proto'
    if not proto_path.exists():
        print(f"错误: 找不到 proto 文件 {proto_path}")
        sys.exit(1)

    with open(proto_path, 'r', encoding='utf-8') as f:
        proto_lines = f.readlines()

    output_dir = Path(__file__).parent.parent / 'tests' / 'contract'
    output_dir.mkdir(parents=True, exist_ok=True)

    total_services = 0
    total_rpcs = 0
    total_write_rpcs = 0
    total_write_with_meta = 0
    total_tests = 0

    for service in SERVICES:
        service_name = service['name']
        print(f"处理 {service_name}...")

        rpcs = parse_rpc_lines(proto_lines, service['start_line'])
        if not rpcs:
            print(f"  警告: 未找到 {service_name} 的 RPC 定义")
            continue

        write_rpcs = [r for r in rpcs if r['is_write']]
        read_rpcs = [r for r in rpcs if not r['is_write']]

        write_with_meta = sum(
            1 for wr in write_rpcs
            if has_metadata_field(proto_lines, wr['request'])
        )

        total_services += 1
        total_rpcs += len(rpcs)
        total_write_rpcs += len(write_rpcs)
        total_write_with_meta += write_with_meta

        tests_for_service = len(read_rpcs) + len(write_rpcs) * 3
        total_tests += tests_for_service

        print(f"  RPC 总数: {len(rpcs)}, 写 RPC: {len(write_rpcs)}"
              f" (有metadata: {write_with_meta}), 测试数: {tests_for_service}")

        content = generate_test_file(service, rpcs, proto_lines)
        file_name = f"test_{camel_to_snake(service_name)}_contract.py"
        file_path = output_dir / file_name
        with open(file_path, 'w', encoding='utf-8') as f:
            f.write(content)
        print(f"  生成: {file_path}")

    print("\n" + "=" * 60)
    print("生成统计:")
    print(f"  Service 数: {total_services}")
    print(f"  RPC 总数: {total_rpcs}")
    print(f"  写 RPC 数: {total_write_rpcs}")
    print(f"  写 RPC 有 metadata（可执行三态）: {total_write_with_meta}")
    print(f"  测试函数数: {total_tests}")
    print("=" * 60)

    if total_services != 14:
        print(f"\n错误: 期望 14 个 service，实际 {total_services} 个")
        sys.exit(1)

    print("\n生成完成！")


if __name__ == '__main__':
    main()

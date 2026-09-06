"""
依据: 03 §2 幂等规范 + test-gates.md §3 幂等行
契约测试辅助模块 —— 请求构造 / RPC 调用 / 字段变异

由 scripts/gen-contract-skeletons.py 生成的测试导入使用。
非手工契约测试代码（依据: test-gates.md §4 禁止手写契约）。
"""

import sys
import os
from uuid import uuid4

# 确保 proto-gen/python 在路径中（依据: TP02-T2 生成物布局）
_project_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
_proto_gen = os.path.join(_project_root, 'libs', 'proto-gen', 'python')
if _proto_gen not in sys.path:
    sys.path.insert(0, _proto_gen)

import codeaudit_common_pb2 as pb2
import grpc

try:
    from google.protobuf.timestamp_pb2 import Timestamp
except ImportError:
    # 环境降级：protobuf 较旧版本
    from google.protobuf import timestamp_pb2
    Timestamp = timestamp_pb2.Timestamp


def make_idempotent_request(request_type: str, caller: str = "contract-test"):
    """
    构造带 RequestMetadata 的请求。
    依据: codeaudit_common.proto L213-L219（RequestMetadata 字段）
    依据: 03 §2 幂等键 = (caller_service, request_id)，UUID v4

    返回: (request_id, request_message)
    """
    request_id = str(uuid4())
    metadata = pb2.RequestMetadata(
        request_id=request_id,
        caller_service=caller,
        timestamp=Timestamp()
    )

    msg_class = getattr(pb2, request_type)
    req = msg_class()

    # 设置 metadata 字段（如果存在）
    if hasattr(req, 'metadata'):
        req.metadata.CopyFrom(metadata)

    # 填充必要业务字段的最小值
    _fill_minimal_fields(req, request_id)

    return request_id, req


def make_request_without_metadata(request_type: str):
    """
    构造不带 RequestMetadata 的请求。
    依据: 03 §2 违例——未携带幂等键的写请求→INVALID_ARGUMENT(3)

    返回: request_message（metadata 字段保持零值）
    """
    msg_class = getattr(pb2, request_type)
    req = msg_class()

    # 明确不设置 metadata——让 protobuf 默认零值
    # 填充业务字段最小值，但跳过 metadata
    _fill_minimal_fields(req, skip_metadata=True)

    return req


def _fill_minimal_fields(msg, request_id: str = "", skip_metadata: bool = False):
    """
    递归填充 protobuf 消息中的字段最小值。
    策略：string → test-{name}，int → 0，bool → false，enum → 0，嵌套 → 递归
    使用 try/except 包裹 setattr 兼容 upb/pure-Python protobuf 和 repeated/map 字段。
    """
    for field_desc in msg.DESCRIPTOR.fields:
        fname = field_desc.name

        # 跳过 metadata
        if skip_metadata and fname == 'metadata':
            continue
        if fname == 'metadata' and not skip_metadata:
            continue

        ftype = field_desc.type

        try:
            if ftype == field_desc.TYPE_STRING:
                if fname == 'task_id':
                    setattr(msg, fname, f"task-{request_id[:8]}" if request_id else "task-test-001")
                elif fname == 'project_id':
                    setattr(msg, fname, "proj-test-001")
                elif 'id' in fname:
                    setattr(msg, fname, f"test-{fname}-001")
                elif 'path' in fname:
                    setattr(msg, fname, "/test/path")
                elif 'content' in fname:
                    setattr(msg, fname, "test-content")
                elif 'reason' in fname or 'message' in fname or 'error' in fname:
                    setattr(msg, fname, "test-reason")
                else:
                    setattr(msg, fname, f"test-{fname}")
            elif ftype in (field_desc.TYPE_INT32, field_desc.TYPE_INT64,
                           field_desc.TYPE_UINT32, field_desc.TYPE_UINT64):
                setattr(msg, fname, 1)
            elif ftype in (field_desc.TYPE_FLOAT, field_desc.TYPE_DOUBLE):
                setattr(msg, fname, 0.8)
            elif ftype == field_desc.TYPE_BOOL:
                setattr(msg, fname, False)
            elif ftype == field_desc.TYPE_ENUM:
                setattr(msg, fname, 1)
            elif ftype == field_desc.TYPE_MESSAGE:
                # 尝试递归子消息；如果失败（repeated/map），跳过
                try:
                    sub_msg = getattr(msg, fname)
                    if hasattr(sub_msg, 'DESCRIPTOR') and hasattr(sub_msg, 'ByteSize'):
                        if sub_msg.ByteSize() == 0:
                            _fill_minimal_fields(sub_msg, request_id, skip_metadata=False)
                except (AttributeError, TypeError):
                    pass
        except (AttributeError, TypeError):
            # repeated/map 字段无法 setattr，安全跳过
            pass


def mutate_business_field(msg):
    """
    修改 protobuf 消息中第一个非 metadata 的可修改字段值。
    用于 diff_body 测试：使第二次请求的业务体与首次不同。
    兼容 upb 和 pure-Python protobuf。
    """
    for field_desc in msg.DESCRIPTOR.fields:
        if field_desc.name == 'metadata':
            continue
        ftype = field_desc.type
        fname = field_desc.name
        try:
            if ftype == field_desc.TYPE_STRING:
                try:
                    current = getattr(msg, fname)
                    setattr(msg, fname, current + "-mutated" if current else "mutated-value")
                    return
                except (AttributeError, TypeError):
                    # repeated string — 尝试 append
                    try:
                        getattr(msg, fname).append("mutation-marker")
                        return
                    except Exception:
                        continue
            elif ftype in (field_desc.TYPE_INT32, field_desc.TYPE_INT64,
                           field_desc.TYPE_UINT32, field_desc.TYPE_UINT64):
                try:
                    current = getattr(msg, fname)
                    setattr(msg, fname, current + 999)
                    return
                except (AttributeError, TypeError):
                    continue
            elif ftype in (field_desc.TYPE_FLOAT, field_desc.TYPE_DOUBLE):
                try:
                    current = getattr(msg, fname)
                    setattr(msg, fname, current + 999.0)
                    return
                except (AttributeError, TypeError):
                    continue
            elif ftype == field_desc.TYPE_ENUM:
                try:
                    current = getattr(msg, fname)
                    setattr(msg, fname, current + 1 if current < 100 else current - 1)
                    return
                except (AttributeError, TypeError):
                    continue
            elif ftype == field_desc.TYPE_BOOL:
                try:
                    current = getattr(msg, fname)
                    setattr(msg, fname, not current)
                    return
                except (AttributeError, TypeError):
                    continue
            elif ftype == field_desc.TYPE_MESSAGE:
                try:
                    sub_msg = getattr(msg, fname)
                    # 检查是否为 repeated field container
                    if hasattr(sub_msg, 'add'):
                        # repeated message — 添加一个新元素
                        new_elem = sub_msg.add()
                        # 设置第一个可用的 string 字段
                        for sf in new_elem.DESCRIPTOR.fields:
                            if sf.type == sf.TYPE_STRING:
                                setattr(new_elem, sf.name, "mutation-marker")
                                return
                        # 如果没有 string 字段，设置第一个 int 字段
                        for sf in new_elem.DESCRIPTOR.fields:
                            if sf.type in (sf.TYPE_INT32, sf.TYPE_INT64):
                                setattr(new_elem, sf.name, 999999)
                                return
                    elif hasattr(sub_msg, 'ByteSize') and sub_msg.ByteSize() > 0:
                        for sub_field in sub_msg.DESCRIPTOR.fields:
                            if sub_field.type == sub_field.TYPE_STRING:
                                current = getattr(sub_msg, sub_field.name)
                                try:
                                    setattr(sub_msg, sub_field.name, current + "-mutated" if current else "mutated-value")
                                    return
                                except (AttributeError, TypeError):
                                    continue
                except (AttributeError, TypeError):
                    continue
        except (AttributeError, TypeError):
            continue

    # 如果所有字段都无法修改（理论上不应该发生），抛出异常
    raise RuntimeError("mutate_business_field: 无法找到可修改的业务字段")


def call_rpc(stub, method_name: str, request, timeout: int = 10):
    """
    调用 gRPC 方法，捕获错误。
    返回: (response, error_string) — 成功时 error=None，失败时 response=None
    """
    method = getattr(stub, method_name)
    try:
        resp = method(request, timeout=timeout)
        return resp, None
    except grpc.RpcError as e:
        code = e.code()
        details = e.details()
        return None, f"{code.name}({code.value[0]}): {details}"

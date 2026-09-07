#!/usr/bin/env python3
"""结构守门测试 —— 防回归机制的第二道防线（行为契约由 test_contract.py 锁定）。

锁定的是**结构不变量**，每条对应一类曾经/可能复发的事故模式：

1. 鉴权覆盖：/api/* 每条路由必须挂 require_token —— 防"新增路由忘挂鉴权"
   （本服务能在沙箱内执行命令，裸露路由 = 远程命令执行面，README 硬红线）；
2. 路由面快照：method+path 集合硬编码比对 —— 路由意外增删/改名即红，
   强制开发者显式更新快照与文档（接口漂移不再静默）；
3. 文档实测化（U7）：README「API 面」表格必须与实际路由一致 —— 文档漂移即红；
4. 分块上限：UPLOAD_CHUNK_BYTES 必须 3 字节对齐且编码后 < 网关 1MiB 收包上限
   （2026-09-06 实测 OUT_OF_RANGE；调大分块 = 上传全链 502）；
5. 静态红线：token 比较必须走 hmac.compare_digest（时序侧信道）；
6. JSON body 上限 8 MiB 是对外契约（网关/引擎按此预算请求体）；
7. vendored SDK 子树在库（fresh clone 构建输入，1e02c20 教训）；
8. REGRESSIONS.md 引用的锁定测试必须真实存在（档案不腐烂）。

本文件不依赖网络/SDK，纯离线。
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

SERVICE_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(SERVICE_ROOT))

from fastapi.routing import APIRoute  # noqa: E402

from openshell_manager import api  # noqa: E402
from openshell_manager.api import create_app, require_token  # noqa: E402

README_ROW = re.compile(r"^\|\s*`([A-Z]+)\s+(/[^\s`]+)`\s*\|", re.M)

# 路由面快照（2026-09-07，d662aeb 基线 + 字符串字段收口；同日增 provider
# DELETE 路由）。改动这里 = 接口面变化：必须同 commit 同步 README「API 面」表
# + docs/api-external.md。
EXPECTED_ROUTES = {
    ("GET", "/healthz"),
    ("GET", "/api/v1/gateway/health"),
    ("POST", "/api/v1/sandboxes"),
    ("GET", "/api/v1/sandboxes"),
    ("GET", "/api/v1/sandboxes/{name}"),
    ("DELETE", "/api/v1/sandboxes/{name}"),
    ("POST", "/api/v1/sandboxes/{name}/wait-ready"),
    ("POST", "/api/v1/sandboxes/exec"),
    ("GET", "/api/v1/sandboxes/{name}/logs"),
    ("POST", "/api/v1/sandboxes/{name}/update-config"),
    ("POST", "/api/v1/sandboxes/{name}/files"),
    ("POST", "/api/v1/sandboxes/{name}/services"),
    ("GET", "/api/v1/sandboxes/{name}/services"),
    ("DELETE", "/api/v1/sandboxes/{name}/services/{service}"),
    ("GET", "/api/v1/inference/route"),
    ("PUT", "/api/v1/inference/route"),
    ("GET", "/api/v1/inference/providers"),
    ("GET", "/api/v1/inference/providers/{name}"),
    ("PUT", "/api/v1/inference/providers"),
    ("DELETE", "/api/v1/inference/providers/{name}"),
}


def _routes() -> set:
    out = set()
    for route in create_app().routes:
        if isinstance(route, APIRoute):
            for method in route.methods:
                out.add((method, route.path))
    return out


def test_every_api_route_requires_token():
    """/api/* 全部挂 require_token；/healthz 必须豁免（探活口径）。

    程序化遍历而非逐条断言——新增路由忘挂鉴权时这里自动变红。"""
    seen_api = 0
    for route in create_app().routes:
        if not isinstance(route, APIRoute):
            continue
        deps = {d.dependency for d in route.dependencies}
        if route.path == "/healthz":
            assert require_token not in deps, \
                "/healthz must stay auth-free (probe contract)"
            continue
        assert route.path.startswith("/api/"), \
            f"non-api route escaped the convention: {route.methods} {route.path}"
        assert require_token in deps, \
            f"route missing auth guard: {route.methods} {route.path}"
        seen_api += 1
    assert seen_api == len(EXPECTED_ROUTES) - 1, seen_api


def test_route_surface_snapshot():
    actual = _routes()
    assert actual == EXPECTED_ROUTES, {
        "missing (route deleted?)": sorted(EXPECTED_ROUTES - actual),
        "unexpected (new route?)": sorted(actual - EXPECTED_ROUTES),
    }


def test_readme_api_table_matches_routes():
    """U7 实测化：README「API 面」表 = 真实路由面。改路由必须同 commit 改文档。
    README 行可带查询参数示例（`?workspace=`），比对前剥离查询串。"""
    readme = (SERVICE_ROOT / "README.md").read_text(encoding="utf-8")
    section = readme.split("## API 面", 1)[1].split("\n## ", 1)[0]
    documented = {(m, p.split("?")[0]) for m, p in README_ROW.findall(section)}
    assert documented == _routes(), {
        "in README but not in code": sorted(documented - _routes()),
        "in code but not in README": sorted(_routes() - documented),
    }


def test_upload_chunk_within_gateway_receive_ceiling():
    """网关拒收 >1MiB 的 ExecSandbox 消息（实测 1048576，非 gRPC 默认 4MiB）。
    分块必须 3 字节对齐（流式 base64 -d 无 padding 截断）且编码后留框架余量。"""
    from openshell_manager.gateway import GatewayFacade

    chunk = GatewayFacade.UPLOAD_CHUNK_BYTES
    assert chunk % 3 == 0, "chunk must be 3-byte aligned (base64 padding truncates)"
    encoded = chunk // 3 * 4
    assert encoded <= 1024 * 1024 - 64 * 1024, \
        f"base64 chunk {encoded} leaves no headroom under the 1MiB gateway ceiling"


def test_token_comparison_is_constant_time():
    api_src = (SERVICE_ROOT / "openshell_manager" / "api.py").read_text(encoding="utf-8")
    fn = api_src.split("async def require_token", 1)[1].split("\nasync def", 1)[0]
    assert "hmac.compare_digest" in fn, \
        "token check must stay constant-time (timing side channel)"


def test_json_body_cap_is_8mib():
    assert api.MAX_BODY_BYTES == 8 * 1024 * 1024, \
        "8 MiB JSON cap is a published contract (docs/api-external.md §2)"


def test_vendored_sdk_tree_present():
    """Dockerfile COPY 输入：python 子树必须是真实入库文件而非嵌套仓残影
    （fresh clone 无此目录 = 镜像必败，1e02c20 教训）。"""
    pkg = SERVICE_ROOT / "libs" / "OpenShell" / "python" / "openshell"
    assert (pkg / "__init__.py").is_file(), \
        f"vendored SDK missing at {pkg} — fresh-clone image build would break"
    assert (pkg / "_proto").is_dir(), "SDK proto modules missing (dict→proto boundary)"


def test_regressions_index_points_at_real_tests():
    """REGRESSIONS.md 引用的 `test_*` 必须真实存在于 tests/ —— 档案不腐烂，
    删除/改名锁定测试必须同 commit 更新档案。"""
    reg = (SERVICE_ROOT / "REGRESSIONS.md").read_text(encoding="utf-8")
    names = set(re.findall(r"`(test_[a-z0-9_]+)`", reg))
    assert names, "REGRESSIONS.md must reference locking tests by name"
    all_tests = "\n".join(
        p.read_text(encoding="utf-8") for p in (SERVICE_ROOT / "tests").glob("test_*.py"))
    missing = sorted(n for n in names if f"def {n}(" not in all_tests)
    assert not missing, f"REGRESSIONS.md references missing tests: {missing}"

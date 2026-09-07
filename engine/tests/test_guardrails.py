"""tests/test_guardrails.py — 守门测试（结构级不变量）。

反回归机制第二层（REGRESSIONS.md）：契约文档 ↔ 代码 的结构级双向比对，
加上台账/变异注册表的自完整性。与行为级锁定测试（Go *_test.go）互补：
行为锁钉"单条缺陷"，守门锁钉"结构不变量"（路由面、proto 覆盖、端口表、
台账不腐烂、已知漂移表不消失）。

覆盖项：
  1. 路由表：docs/api-external.md §2 ↔ transcode.go/main.go 字面量双向比对；
  2. 鉴权链：main.go 中间件序与文档口径（JWT 外置、/v1/auth/* 免 JWT）；
  3. 错误映射：grpcToHTTP switch ↔ 文档映射表；
  4. proto 覆盖：11 service 的每个声明 RPC 必须有实现函数；显式 Unimplemented
     集合与文档一致（有人把实现悄悄改回 Unimplemented → 红）；
  5. 台账：REGRESSIONS.md 引用的锁定测试真实存在；变异 id 双向对账；
  6. 端口表：docs/data-flows.md ↔ configs/codeaudit.yaml；
  7. 已知漂移表：api-external.md §7 的 D1-D4 行不得被删（删=掩盖已知问题）；
  8. 变异锚点静态检查（等价于 run_mutations.py --check，无 go 也能跑）。
"""

import importlib.util
import os
import re

import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))

# ---------------------------------------------------------------------------
# 助手
# ---------------------------------------------------------------------------


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8") as f:
        return f.read()


def service_tree(service):
    base = os.path.join(ROOT, "services", service)
    out = []
    for dirpath, _dirs, files in os.walk(base):
        for fn in files:
            if fn.endswith(".go"):
                out.append(os.path.join(dirpath, fn))
    return out


# ---------------------------------------------------------------------------
# 1. 路由表 ↔ 代码
# ---------------------------------------------------------------------------

def docs_routes():
    """解析 api-external.md §2 路由总表 → {(METHOD, path)}。"""
    md = read("docs/api-external.md")
    section = md.split("## 2. 路由总表", 1)[1].split("## 3.", 1)[0]
    routes = set()
    for m in re.finditer(r"^\| (GET|POST|PUT|DELETE|PATCH) (\S+) \|", section, re.M):
        routes.add((m.group(1), m.group(2)))
    return routes


def code_routes():
    """从 transcode.go + main.go 的字面量抽取路由面 → {(METHOD, path)}。"""
    src = read("services/gateway-service/internal/handler/transcode.go")
    main_src = read("services/gateway-service/cmd/main.go")

    routes = set()
    # serveHTTP：一级域（1 个 tab 的 case）与 auth 子端点（2 个 tab 的 case）
    serve_body = src.split("func (t *Transcoder) serveHTTP", 1)[1].split("\nfunc ", 1)[0]
    domains = re.findall(r'^\tcase "([a-z]+)":', serve_body, re.M)
    auth_eps = re.findall(r'^\t\tcase "([a-z]+)":', serve_body, re.M)
    for ep in auth_eps:
        routes.add(("POST", f"/v1/auth/{ep}"))

    method_map = {"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH"}

    def line_routes(chunk, domain):
        out = set()
        for line in chunk.splitlines():
            lm = re.search(r"r\.Method == http\.Method(\w+)", line)
            if not lm:
                continue
            method = method_map[lm.group(1)]
            sub = re.search(r'rest\[1\] == "([^"]+)"', line)
            sub0 = re.search(r'rest\[0\] == "([^"]+)"', line)
            n = re.search(r"len\(rest\) == (\d)", line)
            if sub:
                out.add((method, f"/v1/{domain}/{{id}}/{sub.group(1)}"))
            elif sub0 and n and n.group(1) == "1":
                out.add((method, f"/v1/{domain}/{sub0.group(1)}"))
            elif sub0 and n and n.group(1) == "2":
                # 深度 2 字面子路径 + 变量段（如 /v1/inference/providers/{name}）
                out.add((method, f"/v1/{domain}/{sub0.group(1)}/{{id}}"))
            elif n:
                depth = int(n.group(1))
                if depth == 0:
                    out.add((method, f"/v1/{domain}"))
                elif depth == 1:
                    out.add((method, f"/v1/{domain}/{{id}}"))
        return out

    # serveHTTP 内各域 case 分支的分发行（如 findings 的 verdict:batch 特判）：
    # 按一级 case 切段，行只归本段域
    segs = re.split(r'^\tcase "([a-z]+)":', serve_body, flags=re.M)
    for i in range(1, len(segs), 2):
        routes |= line_routes(segs[i + 1], segs[i])

    # 各域函数体 → 路由
    funcs = re.split(r"(?=^func \(t \*Transcoder\) )", src, flags=re.M)
    for chunk in funcs:
        fm = re.match(r"func \(t \*Transcoder\) (\w+)\(", chunk)
        if not fm:
            continue
        fname = fm.group(1)
        if fname not in domains:
            continue
        routes |= line_routes(chunk, fname)
        # tools 域为否定式守卫（len(rest)!=0 || r.Method != GET → 404）
        if fname == "tools" and re.search(r"r\.Method != http\.MethodGet", chunk):
            routes.add(("GET", "/v1/tools"))

    # callByName 任务动作（POST）
    for action in re.findall(r'^\t\t\t"([a-z]+)": func\(ctx context\.Context\)', src, re.M):
        routes.add(("POST", f"/v1/tasks/{{id}}/{action}"))

    # main.go 本地注册
    if 'publicMux.HandleFunc("/health"' in main_src:
        routes.add(("GET", "/health"))
    if 'apiMux.HandleFunc("/v1/uploads/archive"' in main_src:
        routes.add(("POST", "/v1/uploads/archive"))
    return routes


def test_route_table_matches_code():
    """文档路由表与代码字面量双向一致：新增/删除路由必须同步 docs/api-external.md。"""
    docs, code = docs_routes(), code_routes()
    missing_in_docs = code - docs
    missing_in_code = docs - code
    assert not missing_in_docs, f"代码中存在但文档未登记的路由（改代码必须同步 docs/api-external.md §2）: {sorted(missing_in_docs)}"
    assert not missing_in_code, f"文档登记但代码已不存在/改名的路由（文档腐烂）: {sorted(missing_in_code)}"


def test_route_table_nonempty():
    """路由表解析自检：抽不到任何行 = 解析器或文档结构坏了，静默空集会假绿。"""
    assert len(docs_routes()) >= 40, f"路由表解析异常（仅 {len(docs_routes())} 条，应 40+）"
    assert len(code_routes()) >= 40, f"代码路由抽取异常（仅 {len(code_routes())} 条）"


# ---------------------------------------------------------------------------
# 2. 鉴权链结构
# ---------------------------------------------------------------------------

def test_gateway_auth_chain_order():
    """保护链必须 JWT 外置包 RateLimit（限流键读 sub，ADR-212⑨）；/v1/auth/* 免 JWT。"""
    main_src = read("services/gateway-service/cmd/main.go")
    assert "strings.HasPrefix(r.URL.Path, \"/v1/auth/\")" in main_src, "/v1/auth/* 免 JWT 分支缺失"
    # protected 链：JWTMiddleware(secret, RateLimitMiddleware(...))
    m = re.search(r"protected := middleware\.JWTMiddleware\(cfg\.JWTSecret,\s*\n\s*middleware\.RateLimitMiddleware\(", main_src)
    assert m, "保护链中间件序漂移：应为 JWT(RateLimit(handler))——限流键依赖 JWT sub（ADR-212⑨）"


# ---------------------------------------------------------------------------
# 3. 错误映射表
# ---------------------------------------------------------------------------

def test_grpc_to_http_mapping_documented():
    """grpcToHTTP 的 switch 与文档映射表逐对一致。"""
    src = read("services/gateway-service/internal/handler/transcode.go")
    body = src.split("func grpcToHTTP", 1)[1].split("\n}", 1)[0]
    code_map = {}
    current = []
    for line in body.splitlines():
        cm = re.match(r"\tcase ((?:[A-Za-z.]+, )*[A-Za-z.]+):", line)
        if cm:
            current += [c.strip().split(".")[-1] for c in cm.group(1).split(",")]
            continue
        hm = re.search(r"return http\.Status(\w+)", line)
        if hm and current:
            for c in current:
                code_map[c] = int(
                    {"BadRequest": 400, "NotFound": 404, "Conflict": 409, "Forbidden": 403,
                     "Unauthorized": 401, "NotImplemented": 501, "ServiceUnavailable": 503,
                     "GatewayTimeout": 504, "InternalServerError": 500}[hm.group(1)])
            current = []
    md = read("docs/api-external.md")
    table = md.split("### 错误契约", 1)[1].split("## 2.", 1)[0]
    doc_map = {}
    for m in re.finditer(r"^\|\s*([A-Za-z/ ·]+?)\s*\|\s*\*{0,2}(\d{3})\*{0,2}\s*\|", table, re.M):
        for code in [c.strip() for c in m.group(1).split("/") if re.fullmatch(r"[A-Za-z]+", c.strip())]:
            doc_map[code] = int(m.group(2))
    # 文档口径中 FailedPrecondition 与 Aborted 同 409；代码亦然
    assert code_map == doc_map, f"gRPC→HTTP 映射漂移: 代码={code_map} 文档={doc_map}"


# ---------------------------------------------------------------------------
# 4. proto 覆盖
# ---------------------------------------------------------------------------

# proto service → (部署服务目录, 实现结构体文件提示)
SERVICE_DIRS = {
    "TaskService": "task-service",
    "ProjectService": "project-service",
    "UserService": "project-service",
    "ResultService": "result-service",
    "ReportService": "result-service",
    "DSHRuntimeService": "dsh-runtime-service",
    "CodeAnalysisService": "dsh-runtime-service",
    "SASTAdapterService": "sast-adapter-service",
    "SASTFusionService": "sast-adapter-service",
    "StorageService": "storage-service",
    "NotificationService": "storage-service",
}

# 文档记载的显式 Unimplemented 集合（api-internal.md §0；有人悄悄改回 Unimplemented → 红）
DOCUMENTED_UNIMPLEMENTED = {
    "TaskService": {"WatchTaskProgress"},
    "DSHRuntimeService": {"WatchAnalysisProgress"},
    "CodeAnalysisService": {"GetCallGraph", "GetDataFlow", "GetAnalysisProgress"},
}


def proto_services():
    src = read("proto/codeaudit_common.proto")
    out = {}
    for sm in re.finditer(r"^service (\w+) \{(.*?)^\}", src, re.M | re.S):
        rpcs = re.findall(r"^\s*rpc (\w+)\(", sm.group(2), re.M)
        out[sm.group(1)] = rpcs
    return out


def test_proto_rpc_counts():
    """proto 11 service / 110 RPC 口径（文档与实际声明的结构不变量；
    2026-09-07 ADR-217 起 104→110：DSHRuntimeService +6 推理管理面 RPC）。"""
    services = proto_services()
    assert set(services) == set(SERVICE_DIRS), f"proto service 集合漂移: {set(services) ^ set(SERVICE_DIRS)}"
    total = sum(len(v) for v in services.values())
    assert total == 110, f"proto RPC 总数 {total} != 110（文档/api-internal.md 需同步）"


def test_every_proto_rpc_has_impl():
    """每个声明的 RPC 必须有显式实现函数（不允许只靠 Unimplemented 嵌入静默兜底）。"""
    services = proto_services()
    missing = []
    for svc, rpcs in services.items():
        tree = "\n".join(read(os.path.relpath(p, ROOT)) for p in service_tree(SERVICE_DIRS[svc]))
        for rpc in rpcs:
            if not re.search(rf"func \([a-zA-Z]+ \*?\w+\) {rpc}\(", tree):
                missing.append(f"{svc}/{rpc}")
    assert not missing, f"proto 声明但无实现函数的 RPC: {missing}"


def _impl_stubs(svc_dir, rpc_index):
    """服务目录内的 (proto service, 方法) 裸桩集合。

    归位规则：接收者结构体名 `<Service>Impl` 优先（同目录多 service 共存时同名
    RPC 靠它消歧，如 dsh-runtime 的 GetAnalysisProgress 双声明双实现）；
    无 Impl 后缀时退回"仅一个 proto service 声明该方法"的单候选判定。
    """
    stubs = set()
    for p in service_tree(svc_dir):
        body = read(os.path.relpath(p, ROOT))
        for chunk in re.split(r"(?=^func )", body, flags=re.M):
            fm = re.match(r"func \((\w+) \*(\w+)\) (\w+)\(", chunk)
            if not fm:
                continue
            _recv_var, recv_type, method = fm.groups()
            fn_body = chunk.split("\nfunc ", 1)[0]
            if not re.search(r"\n\treturn (nil, )?status\.Errorf?\(codes\.Unimplemented", fn_body):
                continue
            if recv_type.endswith("Impl") and recv_type[:-4] in rpc_index:
                owner = recv_type[:-4]
            else:
                owners = [s for s, r in rpc_index.items() if method in r]
                owner = owners[0] if len(owners) == 1 else None
            if owner is not None and method in rpc_index[owner]:
                stubs.add((owner, method))
    return stubs


def test_explicit_unimplemented_set_matches_docs():
    """显式 Unimplemented（无条件裸桩）集合与 api-internal.md §0 一致（悄悄退化 → 红）。

    判定=实现函数的第一条语句即无条件 return codes.Unimplemented；
    条件分支内的 Unimplemented（如 storage memory 模式诚实降级）不算裸桩。
    """
    services = proto_services()
    found = {}
    seen_dirs = set()
    for svc in services:
        d = SERVICE_DIRS[svc]
        if d in seen_dirs:  # 同目录多 service 只扫一次，桩经接收者结构体归位
            continue
        seen_dirs.add(d)
        for owner, method in _impl_stubs(d, services):
            found.setdefault(owner, set()).add(method)
    documented = {k: set(v) for k, v in DOCUMENTED_UNIMPLEMENTED.items()}
    assert found == documented, (
        f"显式 Unimplemented 集合漂移: 实际={found} 文档={documented}；"
        f"新增退化必须先在 api-internal.md §0 登记（并评估调用方影响）")


# ---------------------------------------------------------------------------
# 5. 台账不腐烂
# ---------------------------------------------------------------------------

def test_regression_ledger_tests_exist():
    """REGRESSIONS.md 引用的锁定测试必须真实存在（改名/删除未同步台账 → 红）。"""
    md = read("REGRESSIONS.md")
    names = set(re.findall(r"`(Test[A-Za-z0-9_]+)`", md))
    assert names, "台账解析不到任何测试名（表结构坏了？）"
    all_go = set()
    scan_dirs = set(SERVICE_DIRS.values()) | {"gateway-service"}
    for svc_dir in scan_dirs:
        for p in service_tree(svc_dir):
            all_go |= set(re.findall(r"^func (Test\w+)\(", read(os.path.relpath(p, ROOT)), re.M))
    for libs_dir in ("libs/common-go", "libs/go-config", "libs/proto-gen"):
        base = os.path.join(ROOT, libs_dir)
        for dirpath, _dirs, files in os.walk(base):
            for fn in files:
                if fn.endswith("_test.go"):
                    all_go |= set(re.findall(r"^func (Test\w+)\(", open(os.path.join(dirpath, fn), encoding="utf-8").read(), re.M))
    missing = names - all_go
    assert not missing, f"台账引用的锁定测试不存在（services/ 与 libs/ 均未找到）: {sorted(missing)}"


def test_regression_ledger_mutant_ids_consistent():
    """台账的变异 id 与 run_mutations.py 注册表双向对账。"""
    md = read("REGRESSIONS.md")
    ledger_ids = set(re.findall(r"\b(M\d+)\b", md.split("## 缺陷档案", 1)[1]))
    spec = importlib.util.spec_from_file_location(
        "run_mutations", os.path.join(ROOT, "tests", "mutation", "run_mutations.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    registry_ids = {m["id"] for m in mod.MUTANTS}
    assert ledger_ids == registry_ids, (
        f"变异 id 不对账: 台账独有={sorted(ledger_ids - registry_ids)} "
        f"注册表独有={sorted(registry_ids - ledger_ids)}")


def test_mutation_anchors_statically():
    """变异锚点静态恰配（无 go 环境也能跑的牙齿检查）。"""
    spec = importlib.util.spec_from_file_location(
        "run_mutations", os.path.join(ROOT, "tests", "mutation", "run_mutations.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    cache = {}
    for m in mod.MUTANTS:
        problems = mod.anchor_check(m, cache)
        assert not problems, f"变异 {m['id']} 锚点失配（源码重构后必须同步 MUTANTS）: {problems}"


# ---------------------------------------------------------------------------
# 6. 端口表
# ---------------------------------------------------------------------------

def test_ports_table_matches_config():
    """docs/data-flows.md 端口总表的服务端口与 configs/codeaudit.yaml 一致。"""
    yaml_src = read("configs/codeaudit.yaml")
    cfg_ports = dict(re.findall(r"^\s+(\w+): (\d+)$", yaml_src.split("ports:", 1)[1].split("addresses:", 1)[0], re.M))
    md = read("docs/data-flows.md")
    table = md.split("### 端口总表", 1)[1]
    doc_ports = {key: port for port, key in re.findall(r"^\| [^|]+\(gRPC\) \| (\d+) \| ports\.(\w+) \|", table, re.M)}
    doc_ports.update({key: port for port, key in re.findall(r"^\| gateway（HTTP\+WS） \| (\d+) \| ports\.(\w+) \|", table, re.M)})
    assert doc_ports, "端口表解析为空（文档结构坏？）"
    for cfg_key, port in doc_ports.items():
        assert cfg_ports.get(cfg_key) == port, (
            f"端口漂移: ports.{cfg_key} 配置={cfg_ports.get(cfg_key)} 文档={port}")


# ---------------------------------------------------------------------------
# 7. 已知漂移表可见性
# ---------------------------------------------------------------------------

def test_known_divergence_table_rows():
    """api-external.md §7 的已知漂移行不得被删（删表=掩盖已知问题）。"""
    md = read("docs/api-external.md")
    section = md.split("## 7. 已知文档/配置与代码漂移", 1)[1].split("## 8.", 1)[0]
    for row in ("| D1 |", "| D2 |", "| D3 |", "| D4 |"):
        assert row in section, f"漂移表 {row} 行被删除——已知问题必须保持可见（修复后改写为已解决，而非删行）"


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__, "-q"]))

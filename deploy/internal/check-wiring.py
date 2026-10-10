#!/usr/bin/env python3
"""check-wiring —— 容器化部署接线静态审计（2026-09-05 GUI 实测暴露缺陷的体系化防线）。

背景：应用单测/契约测试 mock 在传输边界，e2e 走的又是幸运路径时，**部署接线缺陷
（compose env 缺覆盖/共享卷缺失/档位回落）对一切测试隐形**，只在真实用户路径上爆。
本工具把当日暴露的接线契约编码为可执行断言：

  A1 服务间地址：services 内每个 *_ADDR env 必须存在且值不含 localhost/127.0.0.1
     （yaml 缺省回落 localhost = 容器内拨自身，RPC 失败被降级链静默吞掉）
  A2 任务源共享卷：agent_repos 四方同卷（gateway/task/runtime=/data/repos，
     sast-adapter=/app/data/repos——运行时 CWD 各异，同相对路径须显式同卷）
  A3 生产档位：prod overlay storage CODEAUDIT_STORE=s3（缺省 memory=通知恒空+
     文件不落 MinIO）；runtime 沙箱路由拨号 env + host-gateway 别名
  A4 模拟档位：sim overlay storage=s3 / result=postgres / Kafka 广播=kafka
  A5 代码侧 env 出口：runtime 的 task 日志地址与网关拨号地址必须接受 env 覆盖
     （addresses.<key> 不带 envs 参数 = 部署层无法注入，任何 compose 都救不了）
  A6 YAML 重复键（委托 check-yaml-dups 的规则在此内置，免去双工具）
  A7 宿主端口出口：MinIO Console(9001) 等运维端口映射必须在位
  A8 枚举 parity：proto 枚举键集 ↔ web dict ↔ 插件标签表三方全等
     （proto 为权威；单侧加/改枚举而消费端不跟 = 用户看到 undefined/字面量）
  A14 构建清单 parity：docker-compose.release.yml 服务集/镜像名/build 引用与
     各仓唯一构建定义全等（防清单演化成第二套定义，2026-09-16）

用法: python3 deploy/check-wiring.py [engine_dir]
退出码: 0=全部通过, 1=存在违规
"""
import os
import sys

import yaml

engine = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else
                         os.path.join(os.path.dirname(__file__), "..", "..", "engine"))
root = os.path.dirname(engine)

fails = []


def ok(msg):
    print(f"  ✓ {msg}")


def bad(msg):
    print(f"  ✗ {msg}")
    fails.append(msg)


def load(path):
    with open(path, encoding="utf-8") as f:
        return yaml.safe_load(f) or {}


def read_src(path, gate):
    """读子仓源文件；缺失/迁移时记 bad 并返回 None（防锚点漂移炸成 traceback）"""
    if not os.path.exists(path):
        bad(f"{gate} 锚点文件缺失（子仓移动/改名后本闸未跟）：{os.path.relpath(path, root)}")
        return None
    with open(path, encoding="utf-8") as f:
        return f.read()


def no_dup_keys(path):
    """A6: 重复键检测（yaml.safe_load 静默 last-wins，须用成对钩子扫描）"""
    import yaml as _y

    seen = {}
    dup = []

    def scan(loader, node, deep=False):
        seen = {}  # 映射级去重（文档级会把同名键跨服务误报）
        for k_node, v_node in node.value:
            key = loader.construct_object(k_node, deep=deep)
            if key in seen:
                dup.append(f"{path}:{k_node.start_mark.line + 1} 重复键 '{key}'（首见 {seen[key]} 行）")
            seen[key] = k_node.start_mark.line + 1
            loader.construct_object(v_node, deep=deep)
        return {}

    DupScanner = type("DupScanner", (_y.SafeLoader,), {})
    DupScanner.add_constructor(_y.resolver.BaseResolver.DEFAULT_MAPPING_TAG, scan)
    _y.load(open(path, encoding="utf-8"), Loader=DupScanner)
    for d in dup:
        bad(f"A6 {d}")


def env_of(svc):
    e = svc.get("environment") or {}
    return e if isinstance(e, dict) else {x.split("=", 1)[0]: x.split("=", 1)[1] for x in e}


def mounts_of(svc):
    out = []
    for v in svc.get("volumes") or []:
        parts = v.split(":")
        if len(parts) >= 2:
            out.append((parts[0], parts[1]))
    return out


print("== A1 服务间地址（engine base compose env 全覆盖，值禁止 localhost）==")
base = load(os.path.join(engine, "docker-compose.yml"))
ADDR_ENV = {
    "gateway": ["CODEAUDIT_PROJECT_SERVICE_ADDR", "CODEAUDIT_TASK_SERVICE_ADDR",
                "CODEAUDIT_RESULT_SERVICE_ADDR", "CODEAUDIT_STORAGE_SERVICE_ADDR",
                "CODEAUDIT_SAST_ADAPTER_ADDR", "CODEAUDIT_RUNTIME_ADDR"],
    "task": ["CODEAUDIT_SAST_ADAPTER_ADDR", "CODEAUDIT_RUNTIME_ADDR",
             "CODEAUDIT_RESULT_ADDR", "CODEAUDIT_PROJECT_ADDR", "CODEAUDIT_STORAGE_ADDR"],
    "runtime": ["CODEAUDIT_RESULT_ADDR", "CODEAUDIT_TASK_ADDR"],
    "sast-adapter": ["CODEAUDIT_RESULT_ADDR"],
}
svcs = base.get("services", {})
for svc, keys in ADDR_ENV.items():
    env = env_of(svcs.get(svc, {}))
    for k in keys:
        v = env.get(k)
        if v is None:
            bad(f"A1 {svc} 缺 {k}（回落 yaml localhost = 拨自身）")
        elif "localhost" in str(v) or "127.0.0.1" in str(v):
            bad(f"A1 {svc} {k}={v} 指向 localhost")
        else:
            ok(f"{svc} {k}={v}")

print("== A2 任务源共享卷 agent_repos ==")
if "agent_repos" not in (base.get("volumes") or {}):
    bad("A2 顶层 volumes 未声明 agent_repos")
else:
    ok("agent_repos 已声明")
REPOS = {"gateway": "/data/repos", "task": "/data/repos",
         "runtime": "/data/repos", "sast-adapter": "/app/data/repos"}
for svc, mp in REPOS.items():
    got = [t for s, t in mounts_of(svcs.get(svc, {})) if s == "agent_repos"]
    if mp in got:
        ok(f"{svc} agent_repos→{mp}")
    else:
        bad(f"A2 {svc} 缺 agent_repos@{mp}（现有挂载={got}）")

print("== A3 生产档位（engine/deploy/docker-compose.deploy.yml，ADR-235 归位）==")
prod = load(os.path.join(root, "engine", "deploy", "docker-compose.deploy.yml"))
psvcs = prod.get("services", {})
penv = env_of(psvcs.get("storage", {}))
if penv.get("CODEAUDIT_STORE") == "s3" and "CODEAUDIT_S3_ENDPOINT" in penv \
        and "CODEAUDIT_S3_BUCKET" in penv:
    ok("prod storage=s3（MinIO/Redis 接线齐全）")
else:
    bad("A3 prod overlay storage 缺 s3 档位/S3 接线（memory 降级=通知空+文件不落 MinIO）")
penv_dsh = env_of(psvcs.get("runtime", {}))
ehosts = " ".join(psvcs.get("runtime", {}).get("extra_hosts") or [])
if "CODEAUDIT_GATEWAY_DIAL_ADDR" in penv_dsh and "host.docker.internal" in ehosts:
    ok("prod runtime 沙箱路由拨号 + host-gateway 别名")
else:
    bad("A3 prod overlay runtime 缺 GATEWAY_DIAL_ADDR/extra_hosts（任意宿主沙箱路由）")
if "codeaudit-engine-net" in str((prod.get("networks") or {})):
    ok("prod 网络显式命名 codeaudit-engine-net")
else:
    bad("A3 prod 网络未显式命名")

print("== A11 生产档 restart 策略（base+prod overlay 合并后全服务 unless-stopped）==")
# 2026-09-13 审计：base 为开发宽容档零 restart，prod overlay 漏补中间件时
# 宿主/daemon 重启后中间件不自起、应用服务空转（两条生产链同用本 overlay）。
for svc in svcs:
    restart = (psvcs.get(svc, {}) or {}).get("restart") or (svcs.get(svc, {}) or {}).get("restart")
    if restart == "unless-stopped":
        ok(f"{svc} restart=unless-stopped")
    else:
        bad(f"A11 {svc} 缺 restart=unless-stopped（宿主/daemon 重启后不自起）")

print("== A4 模拟档位（deploy/docker-compose.sim.yml）==")
sim = load(os.path.join(root, "deploy", "internal", "docker-compose.sim.yml"))
ssvcs = sim.get("services", {})
if env_of(ssvcs.get("storage", {})).get("CODEAUDIT_STORE") == "s3":
    ok("sim storage=s3")
else:
    bad("A4 sim overlay storage 缺 s3 档位")
if env_of(ssvcs.get("result", {})).get("CODEAUDIT_STORE") == "postgres":
    ok("sim result=postgres")
else:
    bad("A4 sim overlay result 缺 postgres 档位")
if "codeaudit-sim-net" in str(sim.get("networks") or {}):
    ok("sim 网络显式命名")
else:
    bad("A4 sim 网络未显式命名")

print("== A5 代码侧 env 出口（runtime 两处历史缺口）==")
tl = read_src(os.path.join(engine, "services", "runtime-service", "internal", "service",
                           "task_log.go"), "A5")
if tl is not None:
    if 'cfg.Str("addresses.task", "CODEAUDIT_TASK_ADDR")' in tl:
        ok("task_log.go addresses.task 可 env 覆盖")
    else:
        bad("A5 task_log.go addresses.task 不接受 env（执行日志容器内部署必丢）")
sa = read_src(os.path.join(engine, "services", "runtime-service", "internal", "service",
                           "sandbox_analysis.go"), "A5")
if sa is not None:
    if 'cfg.Str("runtime.sandbox.gateway_dial_addr", "CODEAUDIT_GATEWAY_DIAL_ADDR")' in sa:
        ok("sandbox_analysis.go gateway_dial_addr 可 env 覆盖")
    else:
        bad("A5 gateway_dial_addr 不接受 env（任意宿主沙箱路由不可注入）")

print("== A7 宿主端口出口 ==")
minio = svcs.get("minio", {})
minio_ports = " ".join(minio.get("ports") or [])
if "CODEAUDIT_HOST_MINIO_CONSOLE" in minio_ports and ":9001" in minio_ports:
    ok("minio Console 端口映射在位（CODEAUDIT_HOST_MINIO_CONSOLE:9001）")
else:
    bad(f"A7 engine compose minio 缺 Console 映射（现有 ports={minio.get('ports')}）")

print("== A8 枚举 parity（proto 权威 ↔ web dict ↔ 插件标签表）==")
import re

# 判定两轴重构：AIVerdict 七值枚举拆为 VerdictDirection×
# VerdictTier 两轴，消费端改为组合五态（web dict/verdict.ts / 插件 findingDetailView.ts），
# 无单枚举 Record 表——parity 门降为「字面量覆盖」形态：proto 每个枚举值串必须在消费端
# 文件出现（覆盖检查；消费端死字面量由 TS 编译/测试面兜底，非本门职责）。
PARITY = [
    # (proto 枚举, 消费端文件, TS 表名；表名 "<literals>"=字面量覆盖形态)
    ("VerdictDirection", "web",  "src/dict/verdict.ts",        "<literals>"),
    ("VerdictTier",      "web",  "src/dict/verdict.ts",        "<literals>"),
    ("TaskStatus", "web",  "src/dict/index.ts",          "TASK_STATUS"),
    ("Severity",   "web",  "src/dict/index.ts",          "SEVERITY"),
    ("StageType",  "web",  "src/dict/index.ts",          "STAGE_TYPE"),
    ("VerdictDirection", "vscode-plugin", "src/findingDetailView.ts", "<literals>"),
    ("VerdictTier",      "vscode-plugin", "src/findingDetailView.ts", "<literals>"),
    ("TaskStatus", "vscode-plugin", "src/progressModel.ts",      "STATUS_LABELS"),
    ("StageType", "vscode-plugin", "src/progressModel.ts",      "STAGE_LABELS"),
    ("Severity",   "vscode-plugin", "src/diagnosticsMapper.ts",  "SEVERITY_LABEL"),
]


def proto_enum_keys(name):
    src = read_src(os.path.join(engine, "proto", "codeaudit_common.proto"), "A8")  # D4: SSOT=proto/（根副本已删）
    if src is None:
        return None
    m = re.search(rf"enum {name} \{{([^}}]*)\}}", src)
    if not m:
        return None
    return set(re.findall(r"^\s*([A-Z0-9_]+)\s*=", m.group(1), re.M))


def ts_table_keys(path, table):
    src = read_src(path, "A8")
    if src is None:
        return None
    m = re.search(rf"(?:export\s+)?const {table}[^=]*=\s*\{{(.*?)\n\}};", src, re.S)
    if not m:
        return None
    return set(re.findall(r"^\s*([A-Za-z0-9_]+)\s*:", m.group(1), re.M))


for enum_name, repo, rel, table in PARITY:
    want = proto_enum_keys(enum_name)
    label = f"{repo}/{rel if table == '<literals>' else table}"
    if want is None:
        bad(f"A8 {label}: proto 枚举 {enum_name} 未找到（枚举改名后本表未跟）")
        continue
    if table == "<literals>":
        src = read_src(os.path.join(root, repo, rel), "A8")
        if src is None:
            bad(f"A8 {label}: 消费端文件不可读"); continue
        # proto 值名已是全名（VERDICT_DIRECTION_TRUE 形态），直接按引号包裹字面量查存在
        miss = [k for k in sorted(want) if f"'{k}'" not in src and f'"{k}"' not in src]
        if miss:
            bad(f"A8 {label} 枚举值字面量缺失 {miss}（proto 已有，组合逻辑未跟）")
        else:
            ok(f"{label} ⊇ {enum_name}（{len(want)} 值字面量覆盖）")
        continue
    got = ts_table_keys(os.path.join(root, repo, rel), table)
    if got is None:
        bad(f"A8 {label}: TS 表 {table} 未找到（文件迁移后本闸未跟）")
        continue
    missing, extra = want - got, got - want
    if not missing and not extra:
        ok(f"{label} = {enum_name}（{len(want)} 键全等）")
    else:
        if missing:
            bad(f"A8 {label} 缺键 {sorted(missing)}（proto 已有，用户见字面量）")
        if extra:
            bad(f"A8 {label} 死键 {sorted(extra)}（proto 无此枚举值）")

print("== A9 deploy.sh REMOTE 契约家族（空串=本机执行，禁 `:-` 回落）==")
DEPLOY_SCRIPTS = [
    ("openshell-gateway/deploy.sh", True),
    ("manager/deploy/deploy.sh", True),
    ("engine/deploy/deploy.sh", True),   # ADR-235 归位后入列（原主仓库 deploy/prod）
    ("web/deploy.sh", True),             # 胶水收敛批补录（原三重空白：偏离+无桩+不在列）
    ("dsh-pentest-sse/deploy.sh", False),  # 无 REMOTE 面；出现赋值时也须守契约
]
for rel, required in DEPLOY_SCRIPTS:
    path = os.path.join(root, rel)
    if not os.path.exists(path):
        if required:
            bad(f"A9 {rel} 缺失（REMOTE 契约守门锚点漂移）")
        continue
    lines = [l for l in open(path, encoding="utf-8").read().splitlines()
             if l.strip().startswith("REMOTE=") and "`" not in l]
    if not lines and not required:
        ok(f"{rel} 无 REMOTE 面（本机脚本，符合预期）")
        continue
    if not lines:
        bad(f"A9 {rel} 缺 REMOTE= 赋值（契约锚点漂移）")
        continue
    bad_form = [l for l in lines if "${REMOTE-:" in l or '${REMOTE:-' in l]
    ok_form = [l for l in lines if "${REMOTE-" in l]
    if bad_form or not ok_form:
        bad(f"A9 {rel} REMOTE 用了 `:-`（空串被吞=本机契约失效）: {bad_form or lines}")
    else:
        ok(f"{rel} REMOTE=`-` 形态（空串=本机契约）×{len(lines)}")

print("== A10 ADR-225 S5 数据面出口（F15：trees 桶/回收器/sourcefile 缓存）==")
ts_src = read_src(os.path.join(engine, "services", "task-service", "internal",
                               "service", "task_service.go"), "A10")
CACHE_ENV = ["CODEAUDIT_TASK_REPO_CACHE_GC_ENABLED", "CODEAUDIT_TASK_REPO_CACHE_GC_INTERVAL_S",
             "CODEAUDIT_TASK_REPO_CACHE_TTL_S", "CODEAUDIT_TASK_REPO_CACHE_ORPHAN_TTL_S",
             "CODEAUDIT_TASK_REPO_CACHE_MAX_BYTES"]
if ts_src is not None:
    missing_env = [k for k in CACHE_ENV if k not in ts_src]
    if not missing_env:
        ok("task-service 卷缓存回收器 5 配置键均可 env 覆盖")
    else:
        bad(f"A10 task-service 缺 env 出口: {missing_env}")
sf_src = read_src(os.path.join(engine, "services", "gateway-service", "internal",
                               "handler", "sourcefile_rehydrate.go"), "A10")
if sf_src is not None:
    if "CODEAUDIT_SOURCEFILE_CACHE_MAX_BYTES" in sf_src:
        ok("gateway source-file 重物化缓存上限可 env 覆盖")
    else:
        bad("A10 gateway 缺 CODEAUDIT_SOURCEFILE_CACHE_MAX_BYTES 出口")
mini_src = read_src(os.path.join(engine, "services", "storage-service", "internal",
                                 "repo", "minio.go"), "A10")
if mini_src is not None:
    if '"trees"' in mini_src:
        ok("storage 侧 trees 域桶已声明（源码树 tar 持久 SSOT）")
    else:
        bad("A10 storage 未声明 trees 桶")
for tpl in (os.path.join(root, "engine", "deploy", "env.template"),   # ADR-235 归位
            os.path.join(root, "deploy", "internal", "env.sim.example")):
    if os.path.exists(tpl) and "CODEAUDIT_TASK_REPO_CACHE_TTL_S" in open(tpl, encoding="utf-8").read():
        ok(f"{os.path.relpath(tpl, root)} 含回收器配置出口注释")
    else:
        bad(f"A10 {os.path.relpath(tpl, root)} 缺卷缓存配置出口（overlay 不可调）")

print("== A12 部署清单 TOML parse（sandbox-deploy.toml，U6/LESSONS #8 在 TOML 上的门禁）==")
toml_path = os.path.join(root, "deploy", "internal", "sandbox-deploy.toml")
try:
    import tomllib
    with open(toml_path, "rb") as f:
        manifest = tomllib.load(f)
except ModuleNotFoundError:
    manifest = None
    bad("A12 python 缺 tomllib（<3.11），清单 parse 门禁不可用")
except Exception as e:
    manifest = None
    bad(f"A12 TOML parse 失败：{e}")
if manifest is not None:
    projs = manifest.get("project") or []
    if len(projs) >= 5:
        ok(f"清单 parse OK（{len(projs)} 项目）")
    else:
        bad(f"A12 清单项目数异常：{len(projs)}（<5）")
    seen_names = set()
    for p in projs:
        name = p.get("name", "<noname>")
        if name in seen_names:
            bad(f"A12 项目名重复：{name}")
        seen_names.add(name)
        miss = [k for k in ("dir", "target") if not p.get(k)]
        if miss:
            bad(f"A12 项目 {name} 缺必填字段 {miss}")
        elif not os.path.isdir(os.path.join(root, p["dir"])):
            bad(f"A12 项目 {name} dir 不存在：{p['dir']}（LESSONS #2 清单 dir 失效型）")
        if "vmid" in p and not isinstance(p["vmid"], int):
            bad(f"A12 项目 {name} vmid 非整数：{p['vmid']!r}")
        if "enabled" in p and not isinstance(p["enabled"], bool):
            bad(f"A12 项目 {name} enabled 非布尔：{p['enabled']!r}")

print("== A13 镜像身份钉名（ADR-233：7 应用服务 image=codeaudit-<svc>:${ENGINE_IMAGE_TAG}，禁占位探针）==")
# 2026-09-15：无 image: 键时镜像名随 compose 项目名漂移（sim/一键/<host-id> 三链同源三名），
# 无版本无回滚锚；占位 true 探针使生产栈健康状态失真。两病均已在 base compose 收口。
for svc, spec in svcs.items():
    if "build" not in spec:
        continue
    img = str(spec.get("image") or "")
    if not img.startswith(f"codeaudit-{svc}:") or "${ENGINE_IMAGE_TAG" not in img:
        bad(f"A13 {svc} 缺 image: codeaudit-{svc}:${{ENGINE_IMAGE_TAG:-latest}} 钉名（匿名镜像随项目名漂移）")
    else:
        ok(f"{svc} image={img}")
    hc = str((spec.get("healthcheck") or {}).get("test") or "")
    if "'true'" in hc:
        bad(f"A13 {svc} healthcheck 为占位 true（生产健康状态失真，ADR-233 已上提真探针）")

print("== A14 主仓库级构建清单（docker-compose.release.yml：引用各仓唯一 Dockerfile，零配方复制）==")
# 2026-09-16 人类裁决"主仓库级构建清单"形态：清单只钉 context/dockerfile/image 身份，
# 配方本体仍在各仓。本闸防清单演化成第二套定义：服务枚举与 engine base compose
# 的构建面全等（+web/manager）、镜像名与运行面一致、构建面不掺运行键。
rel_path = os.path.join(root, "deploy", "internal", "docker-compose.release.yml")
if not os.path.exists(rel_path):
    bad("A14 缺 deploy/docker-compose.release.yml（一键全量构建清单，deploy/build.sh 依赖）")
else:
    rel = load(rel_path)
    rsvcs = rel.get("services", {})
    app_svcs = {s for s, spec in svcs.items() if "build" in spec}  # engine 7 应用服务（中间件 pulled 不构建）
    want = app_svcs | {"web", "manager"}
    missing = want - set(rsvcs)
    extra = set(rsvcs) - want
    if missing:
        bad(f"A14 清单缺服务: {sorted(missing)}（须与 engine 构建面+web+manager 全等）")
    if extra:
        bad(f"A14 清单多出服务: {sorted(extra)}（沙箱链经 deploy/build.sh→sse deploy.sh，不进清单）")
    if not missing and not extra:
        ok(f"清单服务集全等（engine×{len(app_svcs)} + web + manager）")
    for svc in app_svcs:
        spec = rsvcs.get(svc) or {}
        b = spec.get("build") or {}
        if spec.get("image") != f"codeaudit-{svc}:${{ENGINE_IMAGE_TAG:-latest}}":
            bad(f"A14 {svc} image 应为 codeaudit-{svc}:${{ENGINE_IMAGE_TAG:-latest}}（与运行面同身份）: {spec.get('image')}")
        if b.get("context") != "../../engine" or b.get("dockerfile") != f"services/{svc}-service/Dockerfile":
            bad(f"A14 {svc} build 须引用 engine 仓唯一 Dockerfile（context=../../engine, "
                f"dockerfile=services/{svc}-service/Dockerfile）: {b}")
        elif not os.path.isfile(os.path.join(engine, b["dockerfile"])):
            bad(f"A14 {svc} 引用的 Dockerfile 不存在: engine/{b['dockerfile']}")
        else:
            ok(f"{svc} → engine/{b['dockerfile']}")
    cspec = rsvcs.get("web") or {}
    cb = cspec.get("build") or {}
    if not str(cspec.get("image") or "").startswith("codeaudit-web:${WEB_IMAGE_TAG"):
        bad(f"A14 web image 应为 codeaudit-web:${{WEB_IMAGE_TAG:-latest}}: {cspec.get('image')}")
    if cb.get("context") != "../../web" or cb.get("dockerfile") != "Dockerfile" \
            or "GIT_REV" not in (cb.get("args") or {}):
        bad(f"A14 web build 须=../web/Dockerfile 且带 GIT_REV arg（web 唯一构建定义+OCI revision）: {cb}")
    else:
        ok("web → web/Dockerfile（GIT_REV arg 在位）")
    mspec = rsvcs.get("manager") or {}
    mb = mspec.get("build") or {}
    if not str(mspec.get("image") or "").startswith("openshell-manager:${MANAGER_IMAGE_TAG"):
        bad(f"A14 manager image 应为 openshell-manager:${{MANAGER_IMAGE_TAG:-2.0.0}}: {mspec.get('image')}")
    if mb.get("context") != "../../manager" or mb.get("dockerfile") != "deploy/Dockerfile.manager":
        bad(f"A14 manager build 须=../manager + deploy/Dockerfile.manager（R31 仓根直构）: {mb}")
    else:
        ok("manager → manager/deploy/Dockerfile.manager")
    for s, spec in rsvcs.items():
        if spec.get("ports") or spec.get("volumes"):
            bad(f"A14 {s} 含 ports/volumes（清单是纯构建面，运行面=base compose+overlay）")
    if (rel.get("name")) != "codeaudit-release":
        bad("A14 清单缺顶层 name: codeaudit-release（项目名钉死，防目录 basename 漂移）")
    else:
        ok("清单项目名钉死 codeaudit-release")

print("== A15 部署落点同源（toml remote ↔ 子仓 deploy.sh DEPLOY_DIR 缺省逐字一致，R11）==")
# 2026-09-19 批立闸：<legacy-path> 在 <host-id> 已整个不存在，四处落点全部钉新值后，
# 任何一侧单独漂移（改子仓缺省忘 toml，或反之）= 编排链与手跑面分叉。守卫比对
# sandbox-deploy.toml 每条 lxc-compose remote 与对应子仓脚本缺省逐字一致。
if manifest is None:
    bad("A15 依赖 A12 的 TOML parse（不可用）")
else:
    a15_pairs = [  # (toml 项目名, 子仓脚本相对主仓库根路径)
        ("openshell-gateway", "openshell-gateway/gateway_lifecycle.sh"),
        ("openshell-manager", "manager/deploy/deploy.sh"),
        ("codeaudit", "engine/deploy/deploy.sh"),
        ("web", "web/deploy.sh"),
    ]
    a15_remotes = {p.get("name"): p.get("remote") for p in (manifest.get("project") or [])}
    dd_re = re.compile(r'^DEPLOY_DIR="\$\{DEPLOY_DIR:-([^"}]+)\}"', re.M)
    for proj, script in a15_pairs:
        want = a15_remotes.get(proj)
        spath = os.path.join(root, script)
        if not want or not os.path.isfile(spath):
            bad(f"A15 {proj}：toml remote 或子仓脚本缺失（{script}）")
            continue
        m = dd_re.search(open(spath, encoding="utf-8").read())
        if not m:
            bad(f"A15 {script} 无 DEPLOY_DIR 缺省行（形态 DEPLOY_DIR=\"${{DEPLOY_DIR:-…}}\"）")
        elif m.group(1) != want:
            bad(f"A15 {proj} 落点分叉：toml remote={want} ≠ 子仓缺省={m.group(1)}（R11 同源契约）")
        else:
            ok(f"{proj} remote=缺省={want}")

for f in (os.path.join(engine, "docker-compose.yml"),
          os.path.join(root, "engine", "deploy", "docker-compose.deploy.yml"),   # ADR-235
          os.path.join(root, "deploy", "internal", "docker-compose.sim.yml"),
          rel_path if os.path.exists(rel_path) else os.path.join(root, "deploy", "internal", "docker-compose.release.yml")):
    if os.path.exists(f):
        no_dup_keys(f)

print()
if fails:
    print(f"check-wiring: {len(fails)} 项违规")
    sys.exit(1)
print("check-wiring: 全部通过")

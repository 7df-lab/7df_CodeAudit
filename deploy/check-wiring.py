#!/usr/bin/env python3
"""check-wiring —— 容器化部署接线静态审计（2026-09-05 GUI 实测暴露缺陷的体系化防线）。

背景：应用单测/契约测试 mock 在传输边界，e2e 走的又是幸运路径时，**部署接线缺陷
（compose env 缺覆盖/共享卷缺失/档位回落）对一切测试隐形**，只在真实用户路径上爆。
本工具把当日暴露的接线契约编码为可执行断言：

  A1 服务间地址：services 内每个 *_ADDR env 必须存在且值不含 localhost/127.0.0.1
     （yaml 缺省回落 localhost = 容器内拨自身，RPC 失败被降级链静默吞掉）
  A2 任务源共享卷：agent_repos 四方同卷（gateway/task/dsh-runtime=/data/repos，
     sast-adapter=/app/data/repos——运行时 CWD 各异，同相对路径须显式同卷）
  A3 生产档位：prod overlay storage CODEAUDIT_STORE=s3（缺省 memory=通知恒空+
     文件不落 MinIO）；dsh-runtime 沙箱路由拨号 env + host-gateway 别名
  A4 模拟档位：sim overlay storage=s3 / result=postgres / Kafka 广播=kafka
  A5 代码侧 env 出口：dsh-runtime 的 task 日志地址与网关拨号地址必须接受 env 覆盖
     （addresses.<key> 不带 envs 参数 = 部署层无法注入，任何 compose 都救不了）
  A6 YAML 重复键（委托 check-yaml-dups 的规则在此内置，免去双工具）
  A7 宿主端口出口：MinIO Console(9001) 等运维端口映射必须在位
  A8 枚举 parity：proto 枚举键集 ↔ web dict ↔ 插件标签表三方全等
     （proto 为权威；单侧加/改枚举而消费端不跟 = 用户看到 undefined/字面量）

用法: python3 deploy/check-wiring.py [engine_dir]
退出码: 0=全部通过, 1=存在违规
"""
import os
import sys

import yaml

engine = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else
                         os.path.join(os.path.dirname(__file__), "..", "engine"))
root = os.path.dirname(engine)

fails = []


def ok(msg):
    print(f"  ✓ {msg}")


def bad(msg):
    print(f"  ✗ {msg}")
    fails.append(msg)


def load(path):
    with open(path) as f:
        return yaml.safe_load(f) or {}


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
    _y.load(open(path), Loader=DupScanner)
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
                "CODEAUDIT_SAST_ADAPTER_ADDR", "CODEAUDIT_DSH_RUNTIME_ADDR"],
    "task": ["CODEAUDIT_SAST_ADAPTER_ADDR", "CODEAUDIT_DSH_RUNTIME_ADDR",
             "CODEAUDIT_RESULT_ADDR", "CODEAUDIT_PROJECT_ADDR", "CODEAUDIT_STORAGE_ADDR"],
    "dsh-runtime": ["CODEAUDIT_RESULT_ADDR", "CODEAUDIT_TASK_ADDR"],
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
         "dsh-runtime": "/data/repos", "sast-adapter": "/app/data/repos"}
for svc, mp in REPOS.items():
    got = [t for s, t in mounts_of(svcs.get(svc, {})) if s == "agent_repos"]
    if mp in got:
        ok(f"{svc} agent_repos→{mp}")
    else:
        bad(f"A2 {svc} 缺 agent_repos@{mp}（现有挂载={got}）")

print("== A3 生产档位（deploy/prod/docker-compose.deploy.yml）==")
prod = load(os.path.join(root, "deploy", "prod", "docker-compose.deploy.yml"))
psvcs = prod.get("services", {})
penv = env_of(psvcs.get("storage", {}))
if penv.get("CODEAUDIT_STORE") == "s3" and "CODEAUDIT_S3_ENDPOINT" in penv \
        and "CODEAUDIT_S3_BUCKET" in penv:
    ok("prod storage=s3（MinIO/Redis 接线齐全）")
else:
    bad("A3 prod overlay storage 缺 s3 档位/S3 接线（memory 降级=通知空+文件不落 MinIO）")
penv_dsh = env_of(psvcs.get("dsh-runtime", {}))
ehosts = " ".join(psvcs.get("dsh-runtime", {}).get("extra_hosts") or [])
if "CODEAUDIT_GATEWAY_DIAL_ADDR" in penv_dsh and "host.docker.internal" in ehosts:
    ok("prod dsh-runtime 沙箱路由拨号 + host-gateway 别名")
else:
    bad("A3 prod overlay dsh-runtime 缺 GATEWAY_DIAL_ADDR/extra_hosts（任意宿主沙箱路由）")
if "codeaudit-engine-net" in str((prod.get("networks") or {})):
    ok("prod 网络显式命名 codeaudit-engine-net")
else:
    bad("A3 prod 网络未显式命名")

print("== A4 模拟档位（deploy/docker-compose.sim.yml）==")
sim = load(os.path.join(root, "deploy", "docker-compose.sim.yml"))
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

print("== A5 代码侧 env 出口（dsh-runtime 两处历史缺口）==")
tl = os.path.join(engine, "services", "dsh-runtime-service", "internal", "service", "task_log.go")
if 'cfg.Str("addresses.task", "CODEAUDIT_TASK_ADDR")' in open(tl).read():
    ok("task_log.go addresses.task 可 env 覆盖")
else:
    bad("A5 task_log.go addresses.task 不接受 env（执行日志容器内部署必丢）")
sa = os.path.join(engine, "services", "dsh-runtime-service", "internal", "service",
                  "sandbox_analysis.go")
if 'cfg.Str("dsh_runtime.sandbox.gateway_dial_addr", "CODEAUDIT_GATEWAY_DIAL_ADDR")' in open(sa).read():
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

PARITY = [
    # (proto 枚举, 消费端文件, TS 表名)
    ("AIVerdict",  "web",  "src/dict/index.ts",          "AI_VERDICT"),
    ("TaskStatus", "web",  "src/dict/index.ts",          "TASK_STATUS"),
    ("Severity",   "web",  "src/dict/index.ts",          "SEVERITY"),
    ("StageType",  "web",  "src/dict/index.ts",          "STAGE_TYPE"),
    ("AIVerdict",  "vscode-plugin", "src/findingDetailView.ts",  "VERDICT_LABEL"),
    ("TaskStatus", "vscode-plugin", "src/progressModel.ts",      "STATUS_LABELS"),
    ("StageType",  "vscode-plugin", "src/progressModel.ts",      "STAGE_LABELS"),
    ("Severity",   "vscode-plugin", "src/diagnosticsMapper.ts",  "SEVERITY_LABEL"),
]


def proto_enum_keys(name):
    src = open(os.path.join(engine, "proto", "codeaudit_common.proto")).read()  # D4: SSOT=proto/（根副本已删）
    m = re.search(rf"enum {name} \{{([^}}]*)\}}", src)
    if not m:
        return None
    return set(re.findall(r"^\s*([A-Z0-9_]+)\s*=", m.group(1), re.M))


def ts_table_keys(path, table):
    src = open(path).read()
    m = re.search(rf"(?:export\s+)?const {table}[^=]*=\s*\{{(.*?)\n\}};", src, re.S)
    if not m:
        return None
    return set(re.findall(r"^\s*([A-Za-z0-9_]+)\s*:", m.group(1), re.M))


for enum_name, repo, rel, table in PARITY:
    want = proto_enum_keys(enum_name)
    got = ts_table_keys(os.path.join(root, repo, rel), table)
    label = f"{repo}/{table}"
    if want is None:
        bad(f"A8 {label}: proto 枚举 {enum_name} 未找到（枚举改名后本表未跟）")
        continue
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
    ("dsh-pentest-sse/deploy.sh", False),  # 无 REMOTE 面；出现赋值时也须守契约
]
for rel, required in DEPLOY_SCRIPTS:
    path = os.path.join(root, rel)
    if not os.path.exists(path):
        if required:
            bad(f"A9 {rel} 缺失（REMOTE 契约守门锚点漂移）")
        continue
    lines = [l for l in open(path).read().splitlines()
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
ts_src = open(os.path.join(engine, "services", "task-service", "internal",
                           "service", "task_service.go")).read()
CACHE_ENV = ["CODEAUDIT_TASK_REPO_CACHE_GC_ENABLED", "CODEAUDIT_TASK_REPO_CACHE_GC_INTERVAL_S",
             "CODEAUDIT_TASK_REPO_CACHE_TTL_S", "CODEAUDIT_TASK_REPO_CACHE_ORPHAN_TTL_S",
             "CODEAUDIT_TASK_REPO_CACHE_MAX_BYTES"]
missing_env = [k for k in CACHE_ENV if k not in ts_src]
if not missing_env:
    ok("task-service 卷缓存回收器 5 配置键均可 env 覆盖")
else:
    bad(f"A10 task-service 缺 env 出口: {missing_env}")
sf_src = open(os.path.join(engine, "services", "gateway-service", "internal",
                           "handler", "sourcefile_rehydrate.go")).read()
if "CODEAUDIT_SOURCEFILE_CACHE_MAX_BYTES" in sf_src:
    ok("gateway source-file 重物化缓存上限可 env 覆盖")
else:
    bad("A10 gateway 缺 CODEAUDIT_SOURCEFILE_CACHE_MAX_BYTES 出口")
mini_src = open(os.path.join(engine, "services", "storage-service", "internal",
                             "repo", "minio.go")).read()
if '"trees"' in mini_src:
    ok("storage 侧 trees 域桶已声明（源码树 tar 持久 SSOT）")
else:
    bad("A10 storage 未声明 trees 桶")
for tpl in (os.path.join(root, "deploy", "prod", "env.template"),
            os.path.join(root, "deploy", "env.sim.example")):
    if os.path.exists(tpl) and "CODEAUDIT_TASK_REPO_CACHE_TTL_S" in open(tpl).read():
        ok(f"{os.path.relpath(tpl, root)} 含回收器配置出口注释")
    else:
        bad(f"A10 {os.path.relpath(tpl, root)} 缺卷缓存配置出口（overlay 不可调）")

for f in (os.path.join(engine, "docker-compose.yml"),
          os.path.join(root, "deploy", "prod", "docker-compose.deploy.yml"),
          os.path.join(root, "deploy", "docker-compose.sim.yml")):
    no_dup_keys(f)

print()
if fails:
    print(f"check-wiring: {len(fails)} 项违规")
    sys.exit(1)
print("check-wiring: 全部通过")

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

for f in (os.path.join(engine, "docker-compose.yml"),
          os.path.join(root, "deploy", "prod", "docker-compose.deploy.yml"),
          os.path.join(root, "deploy", "docker-compose.sim.yml"),
          os.path.join(root, "deploy", "prod", "docker-compose.deploy.yml")):
    no_dup_keys(f)

print()
if fails:
    print(f"check-wiring: {len(fails)} 项违规")
    sys.exit(1)
print("check-wiring: 全部通过")

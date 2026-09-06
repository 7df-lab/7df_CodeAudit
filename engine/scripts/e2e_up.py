#!/usr/bin/env python3
"""
E2E 服务网启动器（scripts/e2e_up.py 供 bash/CI 调用；tests/e2e 由 pytest 引用其函数）。

职责:
  1. 编译各 Go 服务二进制（go build -o）
  2. 后台进程方式拉起 result → dsh-runtime → sast-adapter → task 四服务（ai-inference 已删 ADR-175）
     （knowledge-service 已删 ADR-197，Skills/KG/RAG 契约随 proto 手术移除）
  3. 提供等待端口就绪与优雅退出

依据:
  - ADR-113/114/117 端口分配
  - 09 §2 通信矩阵（服务互联拓扑）
"""
import os
import signal
import socket
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BIN = ROOT / ".toolchain" / "bin"

# (name, port, env) — 端口依据 ADR-113/114/117 + 契约夹具约定 50051
SERVICES = [
    ("result",       50058, {"CODEAUDIT_STORE": "memory"}),                    # 07 §10 内存降级路径
    ("dsh-runtime",    50057, {
                              "CODEAUDIT_RESULT_ADDR": "localhost:50058"}),
    ("sast-adapter", 50051, {"CODEAUDIT_RESULT_ADDR": "localhost:50058"}),
    ("task",         50054, {"CODEAUDIT_SAST_ADAPTER_ADDR": "localhost:50051",
                              "CODEAUDIT_DSH_RUNTIME_ADDR": "localhost:50057",
                              "CODEAUDIT_RESULT_ADDR": "localhost:50058"}),
]

_procs: dict[str, subprocess.Popen] = {}


def build_all() -> None:
    env = os.environ.copy()
    env.update({
        "PATH": f"{ROOT / '.toolchain' / 'go' / 'bin'}:{env.get('PATH', '')}",
        "GOPROXY": "https://goproxy.cn,direct",
        "GOCACHE": str(ROOT / ".toolchain" / "gocache"),
        "GOMODCACHE": str(ROOT / ".toolchain" / "gomod"),
    })
    for name, _, _ in SERVICES:
        svc_dir = ROOT / "services" / f"{name}-service"
        BIN.mkdir(parents=True, exist_ok=True)
        subprocess.run(
            ["go", "build", "-o", str(BIN / name), "./cmd"],
            cwd=svc_dir, env=env, check=True,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        )
        print(f"[e2e-up] built {name}")


def port_open(port: int, timeout: float = 1.0) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(timeout)
        return s.connect_ex(("127.0.0.1", port)) == 0


def start_all(wait_secs: int = 60) -> None:
    logs = ROOT / ".agent" / "evidence"
    logs.mkdir(parents=True, exist_ok=True)
    for name, port, extra in SERVICES:
        if port_open(port):
            print(f"[e2e-up] {name} already on :{port}, skip")
            continue
        logf = open(logs / f"e2e_{name}.log", "ab")
        proc = subprocess.Popen(
            [str(BIN / name)],
            cwd=ROOT, env={**os.environ, **extra},
            stdout=logf, stderr=subprocess.STDOUT,
        )
        _procs[name] = proc
        deadline = time.time() + wait_secs
        while time.time() < deadline:
            if port_open(port):
                break
            if proc.poll() is not None:
                raise RuntimeError(f"{name} exited early rc={proc.returncode}")
            time.sleep(0.3)
        else:
            raise RuntimeError(f"{name} not listening on {port} within {wait_secs}s")
        print(f"[e2e-up] {name} up on :{port}")


def stop_all() -> None:
    for name, proc in list(_procs.items()):
        if proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
        print(f"[e2e-up] stopped {name}")
        _procs.pop(name, None)


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "up"
    try:
        if cmd == "build":
            build_all()
        elif cmd == "up":
            build_all()
            start_all()
            print("[e2e-up] all services up")
        elif cmd == "down":
            stop_all()
        else:
            print(f"unknown command: {cmd}")
            sys.exit(2)
    finally:
        if cmd != "down":
            pass  # up 模式保持后台运行（由调用方 stop_all/down 回收或随容器销毁）

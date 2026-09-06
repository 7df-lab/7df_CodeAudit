# -*- coding: utf-8 -*-
"""E2E 套件配置（SMK-6 全模式扩展; 端口依据 ADR-113/114/117）。"""
import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if os.path.join(ROOT, "libs", "proto-gen", "python") not in sys.path:
    sys.path.insert(0, os.path.join(ROOT, "libs", "proto-gen", "python"))

# PYTHONPATH 注入本地 bandit（无 docker 环境）
PYLIBS = os.path.join(ROOT, ".toolchain", "pylibs")
if os.path.isdir(PYLIBS):
    env_pp = os.environ.get("PYTHONPATH", "")
    if PYLIBS not in env_pp:
        os.environ["PYTHONPATH"] = f"{PYLIBS}:{env_pp}" if env_pp else PYLIBS

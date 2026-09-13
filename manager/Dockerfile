# openshell-manager — FastAPI 版镜像（ADR-174）
# 构建上下文 = 仓库根；build/wheels/ 放离线 wheel（fastapi/uvicorn 及依赖，
# 宿主 `pip download --platform manylinux2014_x86_64 --python-version 3.12
# --only-binary=:all: fastapi uvicorn -d build/wheels` 生成，不入 git——
# fresh clone 须先执行该下载再 build（如实化）。
#
# ⚠ 秘密警示：基础镜像 openshell-manager:1.0.0 内含
# .token 部署文件，本路径产物随之携带——禁止 push/导出/分发；生产重建
# 一律走 deploy/Dockerfile.manager（token 只走环境变量，非 root 运行）。
# 本路径仅限 107 本地离线叠加 fastapi/uvicorn。
FROM openshell-manager:1.0.0

COPY build/wheels /tmp/wheels
# 原镜像运行用户非 root（HOME=/nonexistent），构建期切 root 装依赖，
# 装完立即切回非 root（曾漏切回 → 产物 root 运行；guardrails
# test_root_dockerfile_discipline 锁定）
USER root
RUN pip3 install --no-index --find-links=/tmp/wheels fastapi uvicorn \
    && rm -rf /tmp/wheels
USER 65534:65534

# FastAPI 服务层（api.py）+ name→UUID 解析与分块对齐修复的 gateway.py
COPY openshell_manager /app/openshell_manager

WORKDIR /app
CMD ["python3", "-m", "openshell_manager"]

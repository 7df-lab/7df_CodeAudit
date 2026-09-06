# openshell-manager — FastAPI 版镜像（ADR-174）
# 构建上下文 = 仓库根；build/wheels/ 放离线 wheel（fastapi/uvicorn 及依赖，
# 宿主 `pip download --platform manylinux2014_x86_64 --python-version 3.12
# --only-binary=:all: fastapi uvicorn -d build/wheels` 生成，不入 git）。
#
# 基于现役 openshell-manager:1.0.0 叠加而非全新基础镜像：1.0.0 已带
# libs/OpenShell Python SDK、grpcio、config.json 与 .token 部署文件，
# 离线环境只需补 fastapi/uvicorn 与新服务层。
FROM openshell-manager:1.0.0

COPY build/wheels /tmp/wheels
# 原镜像运行用户非 root（HOME=/nonexistent），构建期切 root 装依赖
USER root
RUN pip3 install --no-index --find-links=/tmp/wheels fastapi uvicorn \
    && rm -rf /tmp/wheels

# FastAPI 服务层（api.py）+ name→UUID 解析与分块对齐修复的 gateway.py；
# http_api.py 旧 stdlib 实现随包保留作参照
COPY openshell_manager /app/openshell_manager

WORKDIR /app
CMD ["python3", "-m", "openshell_manager"]

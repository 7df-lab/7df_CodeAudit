# CodeAudit 安装器镜像——「docker 命令一键部署」形态。
#
# 构建与运行（DooD：容器内跑部署脚本，容器起在宿主 daemon）：
#   docker build -f deploy/Dockerfile -t codeaudit-installer .
#   docker run --rm -it --network host \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v "${PWD}:${PWD}" -w "${PWD}" codeaudit-installer
# --network host 必需：部署健康门全打 127.0.0.1（宿主 loopback）——容器自己的
# loopback 够不到宿主发布端口（清洁房 docker 安装二跑实证）。Windows Docker
# Desktop 需在设置里启用 host networking（未启用时请用 Git Bash 原生路径安装）。
# 关键：仓库必须按【宿主同路径】挂载（-v PWD:PWD）——bind 挂载由宿主 daemon 按
# 宿主路径解析（清洁房 docker 安装首跑实证：容器内别名 /repo 会使 compose 的
# ./gateway.toml、./configs 等 bind 全部 404）。Windows PowerShell 同型：
# -v ${PWD}:${PWD} -w ${PWD}（Docker Desktop 需开启该盘符文件共享）。
# 单仓检出（子目录随仓）；历史 submodule 口径已随拍平终结。
#
# 容器内 /repo 即宿主检出台 bind mount——compose build context、镜像构建、
# 密钥生成（deploy/internal/production.env）全部落在宿主真实文件上，容器只是
# 提供一致的 bash/python/docker 工具面（免 Windows 装 Git Bash/WSL）。
FROM alpine:3.20

RUN apk add --no-cache \
        docker-cli docker-cli-compose \
        git bash python3 py3-yaml \
        openssl curl tar findutils coreutils \
    && mkdir -p /repo
# 以 root 运行：挂载 docker.sock 本即 root 等价信任域；且 /repo bind mount 文件
# 属主为宿主用户，非 root 写不进密钥/产物（首跑实测 Permission denied）。
WORKDIR /repo
ENTRYPOINT ["bash", "deploy/deploy.sh"]
CMD ["install"]

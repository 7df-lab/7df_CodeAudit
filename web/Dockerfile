# CodeAudit Console — 前端独立容器
# 多阶段构建：node 构建静态产物 → nginx 托管 + /v1 反代网关（HTTP+WS）
# 构建: docker build -t codeaudit-web .
# 运行: docker run -p 8088:80 -e CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:8080 codeaudit-web
#
# 镜像身份唯一：codeaudit-web 的构建定义只有本文件
# +docker-compose.yml 一处，sim/<host-id> 生产/一键三条链消费同一 tag，不再各自编译。
FROM node:20-alpine AS build
WORKDIR /app
# npm registry 通道：构建容器直连
# registry.npmjs.org 在无出口网络下 npm ci 必超时。ARG 缺省官方源（有出口宿主
# 零变化）；部署/构建入口按出口探测注入 npmmirror（production-deploy.sh
# deploy_web / compose ${NPM_REGISTRY}）。npm≥9.3 的 replace-registry-host=npmjs
# 缺省行为会把 lock 内 npmjs.org 的 resolved 重写到配置源——lock 不改一字。
ARG NPM_REGISTRY=https://registry.npmjs.org
# 先拷贝锁文件独立成层——依赖未变时命中缓存，跳过 npm ci
COPY package.json package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --no-audit --no-fund
COPY . .
# tsc -b（类型门禁）+ vite build（生产产物 → /app/dist）
RUN npm run build

FROM nginx:1.27-alpine
# 溯源标签：GIT_REV 由部署入口注入（web/deploy.sh / production-deploy.sh 均取
# 构建树 git HEAD；无 .git 的发布树回落 unknown），docker inspect 可查
ARG GIT_REV=unknown
LABEL org.opencontainers.image.title=codeaudit-web \
      org.opencontainers.image.revision=${GIT_REV}
COPY nginx/default.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
# 官方镜像 entrypoint 对 templates/*.template 做 envsubst——仅替换"已定义的环境变量"，
# nginx 运行时变量（$uri/$remote_addr 等）不受影响
# B5: 上游缺省对齐 deploy.sh/compose/主仓库生产事实源 8090（裸 docker run 缺省口径）；
# Dockerfile 语法约束：ENV 值不允行尾注释（`#` 会被并进解析——sim rebuild 构建实证），注释独立成行
ENV CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:8090
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD wget -qO /dev/null http://127.0.0.1/ || exit 1

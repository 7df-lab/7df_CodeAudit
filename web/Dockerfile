# CodeAudit Console — 前端独立容器（自 codeaudit/web/console 抽离，2026-09-05）
# 多阶段构建：node 构建静态产物 → nginx 托管 + /v1 反代网关（HTTP+WS）
# 构建: docker build -t codeaudit-console .
# 运行: docker run -p 8088:80 -e CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:8080 codeaudit-console
FROM node:20-alpine AS build
WORKDIR /app
# 先拷贝锁文件独立成层——依赖未变时命中缓存，跳过 npm ci
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY . .
# tsc -b（类型门禁）+ vite build（生产产物 → /app/dist）
RUN npm run build

FROM nginx:1.27-alpine
COPY nginx/default.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
# 官方镜像 entrypoint 对 templates/*.template 做 envsubst——仅替换"已定义的环境变量"，
# nginx 运行时变量（$uri/$remote_addr 等）不受影响
ENV CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:8080
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD wget -qO /dev/null http://127.0.0.1/ || exit 1

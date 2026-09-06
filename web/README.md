# CodeAudit Console（codeaudit-console）

CodeAudit 平台的前端控制台，自 `codeaudit/web/console` 抽离的独立容器化项目；伞仓（codeaudit-umbrella）七子仓之一，是平台前端的**唯一事实源**（engine 内部前端副本已摘除，前端门禁归本仓）。

- 技术栈：React 18 + TypeScript 5（strict）+ antd 5（zh_CN 中文界面）+ react-router 6 + TanStack Query 5 + axios，Vite 5 构建，Vitest + Testing Library 测试
- 通信约束：浏览器只与本容器**同源**通信——REST/WebSocket 全部走 `/v1/*`，由开发期 Vite 代理或生产期 nginx 反代转发到 gateway，客户端零直连微服务

## 目录结构

```
Dockerfile                   多阶段：node:20-alpine 构建 → nginx:1.27-alpine 托管
docker-compose.yml           一键起容器（网关地址/端口可注入）
nginx/default.conf.template  SPA 回退 + /v1 反代（REST+WS）+ gzip/缓存/上传余量
index.html                   入口 HTML（中文站点，data-URL 空 favicon）
src/
  main.tsx                   装配：StrictMode + antd zh_CN + QueryClient + Router + Session + ErrorBoundary
  App.tsx                    路由表 + 全局壳（顶栏导航/会话守卫/未读角标/API 错误挂载点）
  api/
    client.ts                axios 实例：token 管理、401 单飞刷新、503 重试、429 退避、
                             JSON 风格查询参数序列化、类型化端点层（上传/项目/任务/报告/源文件）
    types.ts                 服务端类型（= proto message 的 snake_case protojson 形状）
    apiEvents.ts             API 错误/恢复事件总线常量
  auth/session.tsx           会话上下文：login/logout/当前用户/F5 静默续签
  tasks/stateMachine.ts      任务状态机的客户端展示镜像（按钮可见性）+ 动作分发 + 轮询间隔
  findings/chainParser.ts    AI 结论 Source→Sink 链路文本解析器（还原为可点选的 hops）
  dict/index.ts              proto 枚举 → 中文展示映射（扫描模式/状态/严重级/阶段等）
  components/                TaskLogPanel（流水线日志）、AIInteractionLogPanel（AI 交互时间线）、
                             errors（403/404/501/503 统一错误 UX）
  pages/                     LoginPage、Projects(+Detail)、tasks/（列表/创建/详情）、
                             findings/（列表/详情）、views/（融合/审核/对比）、reports、
                             notifications、admin/Users
  testsupport/fakeGateway.ts 测试台：axios adapter 层伪造网关（详见「测试」）
  __tests__/                 21 个测试文件（vitest run 入口自动发现）
```

## 本地开发

```bash
npm ci
CODEAUDIT_GATEWAY_URL=http://localhost:8080 npm run dev   # /v1 代理到网关（含 WS），缺省 localhost:8080
npm test                # vitest run（jsdom，84 用例）
npm run build           # tsc -b 类型门禁 + vite build → dist/
npm run preview         # 本地预览生产构建（/v1 代理配置与 dev 相同）
```

环境变量按运行入口区分，两个名字不要混用：

| 变量 | 用在哪 | 缺省 | 说明 |
|------|--------|------|------|
| `CODEAUDIT_GATEWAY_URL` | `npm run dev` / `preview` | `http://localhost:8080` | Vite 代理的网关目标 |
| `CODEAUDIT_GATEWAY_UPSTREAM` | 容器（nginx） | `host.docker.internal:8080` | nginx 反代上游（容器内可达地址） |
| `CODEAUDIT_CONSOLE_PORT` | docker compose | `8088` | compose 映射到宿主机的端口 |

伞仓入口：`make test-web`（= 本仓 `npm test`）、`make build-web`（= `npm run build`）。

## 页面与路由

| 路由 | 页面 | 要点 |
|------|------|------|
| `/login` | 登录 | `POST /v1/auth/login`；未登录访问其余路由自动跳转至此 |
| `/` | — | 重定向到 `/projects` |
| `/projects` | 项目列表 | 服务端翻页；创建项目；上传代码压缩包（zip/tar.gz ≤25MB，multipart 直传 `/v1/uploads/archive`，网关零落盘转 storage，`file_id` 写入项目 `config.upload_file_id`） |
| `/projects/:id` | 项目详情 | 项目信息 + 源码来源只读展示（上传件或仓库地址）+ 关联任务列表 + 删除 |
| `/tasks` | 任务列表 | 服务端游标翻页 + 项目/模式筛选 |
| `/tasks/new` | 任务创建向导 | 五种扫描模式（A 纯SAST / B 纯AI / C SAST+AI融合（推荐）/ D AI增强SAST / E SAST+AI对比；两个旧模式仅历史兼容）；SAST 工具多选；压缩包上传（任务级覆盖）；创建成功自动发起启动（审批流已废除） |
| `/tasks/:id` | 任务详情 | 左右两栏：左=AI 交互日志时间线常驻；右=任务信息/阶段时间线/执行日志/报告摘要/发现 Tabs（按模式出现融合/审核视图）。WebSocket `/v1/tasks/{id}/ws` 在线时服务端 250ms 聚合推帧，断线回退 10s 轮询（终态自停）；动作按钮按状态机可见 |
| `/tasks/:id/comparison` | 对比视图 | 模式E：SAST/AI 三分桶对比 + 指标脚注 |
| `/findings/:fid` | 发现详情 | triage 工作台：AI 结论、代码上下文全文（`/v1/tasks/{id}/source-file`）、Source→Sink 链路跳转 |
| `/reports` | 报告中心 | 报告列表/在线查看/下载；失败报告可重新生成 |
| `/notifications` | 通知中心 | 当前用户通知 + 标记已读；顶栏菜单挂未读角标（60s 兜底轮询） |
| `/admin/users` | 用户管理 | V1 口径：按 ID 查询用户与权限（缺省查当前用户）+ 修改该用户状态（启用/停用）；无用户列表/自助注册（待 proto V2.1） |
| `*` | 404 | 未知路由显示 404 错误页（不静默重定向） |

发现列表页内嵌于任务详情的发现 Tab；发现详情主体（`FindingDetailBody`）同时被列表行内展开复用。

## 客户端关键机制

**会话与令牌**（`api/client.ts` + `auth/session.tsx`）
- access_token 仅存内存，refresh_token 存 localStorage（键 `codeaudit.refresh_token`）
- 401 → 单飞刷新（并发请求只发一次 refresh）→ 重放原请求；刷新失败清会话跳 `/login`；刷新走裸 fetch 避免拦截器递归
- F5/直链进站时用 refresh_token 静默续签恢复会话；logout 携带 access_token 调 `POST /v1/auth/logout`

**错误 UX 契约**（拦截器 + `components/errors.tsx`）
- 403 权限不足 / 501 能力未接入 → 全页错误组件（事件总线驱动，auth 端点除外）
- 503 → 自动重试 3 次（1s/2s/4s 退避），耗尽后显示降级横幅，服务恢复自动撤销
- 429 → 读取 `retry_after` 记录退避截止时刻，轮询间隔自动拉长，避免撞击限流器

**查询参数序列化**：网关只认 JSON 风格嵌套参数（`?pagination={"page_size":20,"cursor":"…"}`，protojson 直解），axios 实例统一覆写 `paramsSerializer`——标量照常、对象/数组 JSON 编码。所有列表分页/游标依赖此行为。

**任务状态机**（`tasks/stateMachine.ts`）：仅是服务端状态机的**展示镜像**，决定按钮可见性/置灰；服务端才是转换权威，非法提交会被后端拒绝。动作映射：CREATED→启动；QUEUED→启动/取消；RUNNING→暂停/取消；PAUSED→恢复/取消；FAILED/TIMEOUT→取消；DEAD→人工重试/取消。创建成功后前端自动 `POST /v1/tasks/{id}/start`（审批流已废除，2026-09-01 人类裁定）。

**发现链路解析**（`findings/chainParser.ts`）：把 AI reasoning 自由文本中的 file:line / 行区间 / 中文行引用等六类引用形态还原为结构化 hops（不推测角色，仅关键词命中才标 source/sink），供发现详情点选定位。

## 测试

```bash
npm test     # vitest run，jsdom 环境，src/test-setup.ts 补 antd 所需 matchMedia
```

测试台 `src/testsupport/fakeGateway.ts` 在 **axios adapter 层**伪造网关，纪律：

1. 只在 HTTP 传输层造假——`api/client.ts` 的真实代码（FormData 上传、参数序列化、401 刷新、503 重试）全量执行；
2. 未建模的路由直接抛错（响亮失败），不做静默空成功；
3. 错误用 `httpError(status, body)` 构造，经真实拦截器链回放（401 刷新/429 退避/503 重试均可测）;
4. 断言请求形状用测试台返回的 `requests` 日志。

## 容器化

```bash
docker build -t codeaudit-console .

# 网关在宿主机 8080：
docker run -d -p 8088:80 -e CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:8080 codeaudit-console

# 或 compose（Linux 的 host.docker.internal 映射已内置 extra_hosts，开箱即用）：
CODEAUDIT_GATEWAY_UPSTREAM=gateway.internal:8080 docker compose up -d
# 浏览器访问 http://<宿主>:8088
```

> 网关不在宿主机时，`CODEAUDIT_GATEWAY_UPSTREAM` 直接给容器可达的 IP/域名即可。

### 构建与镜像

- 多阶段：`node:20-alpine` 内 `npm ci` + `npm run build`（锁文件独立成层缓存依赖）→ `nginx:1.27-alpine` 托管 `dist/`
- 网关地址经官方镜像 envsubst 机制注入 nginx 模板（只替换已定义的环境变量，nginx 运行时变量不受影响）
- `EXPOSE 80`；自带 HEALTHCHECK（wget 本机 80）

### 产物分包（vite.config.ts）

按变更频率拆四块：应用代码 `index`（≈76KB，每版都变）/ react 全家桶 `react`（≈164KB）/ 其余三方 `vendor`（≈126KB，axios/react-query）/ antd 生态 `antd`（≈997KB，antd+icons+rc-*+dayjs，版本随 antd 锁定同步升版）。nginx 对 `/assets/` 30d immutable 缓存下，发版后回访只需重下 index 块。块依赖为单向无环图；antd 单块体积为组件库固有成本，`chunkSizeWarningLimit: 1000` 为有依据接受。

### nginx 要点（nginx/default.conf.template）

- `/`：SPA `try_files` 回退（react-router history 模式）
- `/v1/`：反代网关，`Upgrade/Connection` 头透传 WebSocket（任务详情页 `/v1/tasks/{id}/ws` 推送）
- `client_max_body_size 30m`：代码压缩包上传 ≤25MB（网关 UploadArchive 上限）+ multipart 余量
- `proxy_read/send_timeout 300s` + `proxy_buffering off`：报告下载/日志聚合等长响应余量
- `/assets/` 产物 hash 长缓存 30d（immutable）；`index.html` 不缓存（发版即生效）；gzip JS/CSS/JSON/SVG

## 验证状态（2026-09-05，问题修复后实测）

- ✅ node 22.23.2 / npm 10.9.8：`npm test` 19 个测试文件 84 用例全部通过
- ✅ `npm run build`：`tsc -b` 零错误，vite 构建零告警（混合导入告警、chunk 环、体积告警均已消除）；
  分包：index 75.6KB / vendor 125.8KB / react 163.9KB / antd 997.0KB
- ✅ `npm run preview` 冒烟：SPA 首页 200、路由回退 `/tasks` 200、四块产物均被引用；`/v1` 代理已生效转发
  （本机当时无运行中的网关，ECONNREFUSED 属预期）
- ⏳ `docker build / compose up`：本机无 docker 运行时，未实测；compose 已内置 Linux `extra_hosts`
  映射，在有 docker 的宿主上按上方命令执行即可

## 验证状态（2026-09-06，六缺陷修复会话补测）

- ✅ `npm test` 21 个测试文件 101 用例全部通过（新增 6 条修复回归用例；假绿检验：还原修复后新用例全红）
- ✅ `npm run build`：`tsc -b` 零错误 + vite 四块分包正常（index 82.4KB / vendor 125.8KB / react 163.9KB / antd 997.0KB）
- ✅ GUI 全链路实测（本机 preview + playwright，`/v1` 代理指 107 模拟栈网关 :18080）：
  登录 → 新建项目（压缩包上传）→ 自动建任务+启动 → WS 实时徽标 → bandit+opengrep 真扫 COMPLETED
  → 发现列表快捷 triage → 结论筛选精确过滤（选"确认为真"恰剩 1 行）→ 报告下载文件名 `<id>.json`；
  全程零浏览器控制台错误。AI 日志 Modal 开关的滚底跟随语义由 jsdom 回归用例覆盖
  （模拟栈当时无含 AI 交互日志的任务，GUI 层如实降级）。

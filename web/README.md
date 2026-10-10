# CodeAudit Console（codeaudit-web）

CodeAudit 平台的前端（web 控制台）。自早期单仓 `codeaudit/web/console` 演化而来，产物名 `codeaudit-web`（"console"一词让渡给未来项目）；主仓库（codeaudit）七子仓之一，是平台前端的**唯一事实源**（前端门禁归本仓）。

- 技术栈：React 18 + TypeScript 5（strict）+ antd 5（zh_CN 中文界面）+ react-router 6 + TanStack Query 5 + axios，Vite 5 构建，Vitest + Testing Library 测试
- 通信约束：浏览器只与本容器**同源**通信——REST/WebSocket 全部走 `/v1/*`，由开发期 Vite 代理或生产期 nginx 反代转发到 gateway，客户端零直连微服务

## 目录结构

```
Dockerfile                   多阶段：node:20-alpine 构建 → nginx:1.27-alpine 托管；GIT_REV 构建参数 → OCI revision 标签
docker-compose.yml           一键起容器（网关地址/端口可注入）；顶层 name 钉项目名 codeaudit-web，
                             是 codeaudit-web 镜像的唯一构建入口（部署设计见 docs/deployment.md）
deploy.sh                    <host-id> 生产链部署（统一契约 deploy|check|status|…；含旧项目名一次性迁移守卫）
nginx/default.conf.template  SPA 回退 + /v1 反代（REST+WS）+ gzip/缓存/上传余量
index.html                   入口 HTML（中文站点，data-URL 空 favicon）
docs/                        接口契约与防回归机制文档（AI 会话第二入口）
  external-interfaces.md     外部接口契约：浏览器↔网关 HTTP/WS 全端点预期输入/输出（[E-nn] 锚点）
  internal-interfaces.md     内部接口契约：模块间调用面/缓存键失效图/复用关系（[I-nn] 锚点）
  dataflows-other.md         其余数据流：令牌生命周期/WS×轮询/下载流/反代链/构建部署
  regression-guard.md        回归防线总纲：三道防线 + 缺陷模式档案 + 修复工作流
  design-tokens.md          视觉设计令牌：配色/字体/版式 SSOT 索引
  deployment.md              部署设计：第一性原理审计/镜像身份唯一/三链流程/多前端扩展口径
scripts/
  guard.sh                   静态守卫（npm run guard）：禁区模式 grep 拦截 + 锚点存在性自检
  mutation-check.sh          变异检验（npm run mutation-check）：证明历史 bug 锁会红
src/
  main.tsx                   装配：StrictMode + antd zh_CN + QueryClient + Router + Session + ErrorBoundary
  App.tsx                    路由表 + 全局壳（顶栏导航/会话守卫/未读角标/API 错误挂载点）
  api/
    client.ts                axios 实例：token 管理、401 单飞刷新、503 重试、429 退避、
                             JSON 风格查询参数序列化、类型化端点层（上传/项目/任务/报告/源文件）
    types.ts                 服务端类型（= proto message 的 snake_case protojson 形状）
    apiEvents.ts             API 错误/恢复事件总线常量
  auth/session.tsx           会话上下文：login/logout/当前用户/F5 静默续签
  tasks/stateMachine.ts      任务状态机的客户端展示镜像（按钮可见性）+ 动作分发（轮询间隔已随 B5 收口移除）
  findings/chainParser.ts    AI 结论 Source→Sink 链路文本解析器（还原为可点选的 hops）
  dict/index.ts              proto 枚举 → 中文展示映射（扫描模式/状态/严重级/阶段等）
  components/                TaskLogPanel（流水线日志）、AIInteractionLogPanel（AI 交互时间线）、
                             errors（403/404/501/503 统一错误 UX）
  pages/                     LoginPage、Dashboard（总览首页）、Projects(+Detail)、tasks/（列表/创建/详情）、
                             findings/（列表/详情）、views/（融合/审核/对比）、reports、
                             notifications、admin/Users
  testsupport/fakeGateway.ts 测试台：axios adapter 层伪造网关（详见「测试」）
  __tests__/                 测试文件（vitest run 入口自动发现，数量以当次门禁输出为准）
```

## 本地开发

```bash
npm ci
CODEAUDIT_GATEWAY_URL=http://localhost:8080 npm run dev   # /v1 代理到网关（含 WS），缺省 localhost:8080
npm run dev:mock        # 无后端视觉走查：挂演示网关
  npm test                # vitest run（jsdom；用例随防线递增，以当次门禁输出为准）
npm run build           # tsc -b 类型门禁 + vite build → dist/
npm run preview         # 本地预览生产构建（/v1 代理配置与 dev 相同）
```

环境变量按运行入口区分，两个名字不要混用：

| 变量 | 用在哪 | 缺省 | 说明 |
|------|--------|------|------|
| `CODEAUDIT_GATEWAY_URL` | `npm run dev` / `preview` | `http://localhost:8080` | Vite 代理的网关目标 |
| `CODEAUDIT_GATEWAY_UPSTREAM` | 容器（nginx） | Dockerfile 内置 `host.docker.internal:8090`（B5 对齐生产事实源；compose 同名变量可覆盖） | nginx 反代上游（容器内可达地址） |
| `CODEAUDIT_WEB_PORT` | docker compose | `8088` | compose 映射到宿主机的端口 |

主仓库入口：`make test-web`（= 本仓 `npm test`）、`make build-web`（= `npm run build`）。

## 页面与路由

| 路由 | 页面 | 要点 |
|------|------|------|
| `/login` | 登录 | `POST /v1/auth/login`；未登录访问其余路由自动跳转至此 |
| `/register` | 注册 | V2.1：`POST /v1/auth/register`，公开路由（与 /login 同级，Shell 之外）；注册即登录，邀请码制（服务端校验） |
| `/` | 总览（首页仪表盘） | P4：跨项目态势——项目/任务/进行中/完成/失败/报告统计带 + 任务状态分布（Progress 纯组件）+ 最近任务/最近报告/未读通知；任务域走 `listAllTasks` 全量翻页（['tasks-all'] 共享缓存；服务端钳 page_size 上限 100——此前一次请求 200 被截断、页长冒充总数，统计自此全集口径） |
| `/projects` | 项目列表 | 服务端翻页；创建项目；上传代码压缩包（zip/tar.gz ≤100MB，multipart 直传 `/v1/uploads/archive`，网关零落盘转 storage，`file_id` 写入项目 `config.upload_file_id`） |
| `/projects/new` | 新建项目（薄壳页） | 流程统一：ProjectCreateModal 宿主（与 /tasks/new 入口范式同构）；`?new=1` 旧深链兼容重定向至此；表单含"创建后立即按默认模式扫描并启动"复选框（默认勾选=保持创建即扫描的缺省行为，可只建项目） |
| `/projects/:id` | 项目详情 | 项目信息 + 源码来源展示与补配（配置源码/更换源码弹窗：上传件或仓库地址二选一）+ 关联任务列表 + 创建扫描任务+ 删除 |
| `/tasks` | 任务列表 | 服务端游标翻页 + 项目/模式筛选 |
| `/tasks/new` | 任务创建向导 | 五种扫描模式（A 纯SAST / B 纯AI / C SAST+AI融合（推荐）/ D AI增强SAST / E SAST+AI对比；两个旧模式仅历史兼容）；SAST 工具多选；创建成功自动发起启动（审批流已废除）。源码归属收口到项目层级（任务级压缩包上传已退役）；扫描模式默认值跟随所选项目 default_scan_mode、Step0 就地创建项目已配齐源码字段 |
| `/tasks/:id` | 任务详情 | 左右两栏：右=AI 交互日志时间线常驻（默认；`codeaudit.task_detail_ai_side` 偏好可切回左），左=任务信息/阶段时间线/执行日志/报告摘要/发现 Tabs（按模式出现融合/审核视图；图任务主面板=图执行画布）。WebSocket `/v1/tasks/{id}/ws` 在线时服务端 250ms 聚合推帧，断线回退 10s 轮询（终态自停）；动作按钮按状态机可见 |
| `/tasks/:id/comparison` | 对比视图 | 模式E：SAST/AI 三分桶对比 + 指标脚注 |
| `/findings/:fid` | 发现详情 | triage 工作台：AI 结论、代码上下文全文（`/v1/tasks/{id}/source-file`）、Source→Sink 链路跳转 |
| `/reports` | 报告中心 | 报告列表/在线查看/下载；失败报告可重新生成；报告 ID 列深链阅读视图 |
| `/reports/:id` | 报告阅读视图 | 报告人性化批：JSON 报告站内阅读——报告头（任务/项目/生成时间）+ 复核结论审计句 + 严重度光谱带（比例堆叠、点选过滤）+ 发现阅读卡（可展开碳黑代码证据、命中行高亮、首卡默认展开）+「原始 JSON」抽屉核对通道；HTML 档维持沙箱窗渲染，非 JSON 文本档原文卡直显，空档引导重新生成。入口：报告中心「在线查看」/任务详情「在线查看完整报告」/ID 深链 |
| `/notifications` | 通知中心 | 当前用户通知 + 标记已读/全部已读；客户端分页、条数可调；顶栏菜单挂未读角标（60s 兜底轮询） |
| `/change-password` | 修改密码 | V2.1：首登 `must_change_password` 强改密锁死页 + 顶栏主动改密共用；`POST /v1/users/me/password` |
| `/admin/users` | 用户管理 | V2.1（admin 门禁）：用户列表（GET /v1/users 游标"加载更多"）+ 搜索/状态过滤 + 管理员建号（POST /v1/users）+ 启用/停用 + 重置密码；非 admin 由路由守卫与页面双重拦截 |

| `/orchestrations` | 负载编排管理 | 编排（pack）CRUD：内置/自建编排列表、YAML 规格编辑、seed_intents、停用/恢复（起多批次） |
| `/admin/workspaces` | 工作区管理 | manager 透传面：工作区列表/建区/删区（含资源守卫 409 透传）；行内「推理配置」抽屉承载原推理 Provider 页（/v1/inference/* 全套：provider 增删改/凭据只进不出/路由切换，default=全局缺省） |
| `*` | 404 | 未知路由显示 404 错误页（不静默重定向） |

发现列表页内嵌于任务详情的发现 Tab；发现详情主体（`FindingDetailBody`）同时被列表行内展开复用。

## 客户端关键机制

**会话与令牌**（`api/client.ts` + `auth/session.tsx`）
- access_token 仅存内存，refresh_token 存 localStorage（键 `codeaudit.refresh_token`）
- 401 → 单飞刷新（并发请求只发一次 refresh）→ 重放原请求；刷新失败**分类处置**（400/401/403/无令牌=凭据失效清会话跳 `/login`；5xx/网络层=保留凭据待恢复）；刷新走裸 fetch 避免拦截器递归
- F5/直链进站时用 refresh_token 静默续签恢复会话；logout 携带 access_token 调 `POST /v1/auth/logout`

**错误 UX 契约**（拦截器 + `components/errors.tsx`）
- 403 权限不足 / 501 能力未接入 → 全页错误组件（事件总线驱动，auth 端点除外）
- 503 → 自动重试 3 次（1s/2s/4s 退避），耗尽后显示降级横幅，服务恢复自动撤销
- 429 → 读取 `retry_after` 记录退避截止时刻，轮询间隔自动拉长，避免撞击限流器

**查询参数序列化**：网关只认 JSON 风格嵌套参数（`?pagination={"page_size":20,"cursor":"…"}`，protojson 直解），axios 实例统一覆写 `paramsSerializer`——标量照常、对象/数组 JSON 编码。所有列表分页/游标依赖此行为。

**任务状态机**（`tasks/stateMachine.ts`）：仅是服务端状态机的**展示镜像**，决定按钮可见性/置灰；服务端才是转换权威，非法提交会被后端拒绝。动作映射：CREATED→启动；QUEUED→启动/取消；RUNNING→暂停/取消；PAUSED→恢复/取消；FAILED/TIMEOUT→取消；DEAD→人工重试/取消。创建成功后前端自动 `POST /v1/tasks/{id}/start`。

**发现链路解析**（`findings/chainParser.ts`）：把 AI reasoning 自由文本中的 file:line / 行区间 / 中文行引用等六类引用形态还原为结构化 hops（不推测角色，仅关键词命中才标 source/sink），供发现详情点选定位。

## 已知取舍

有意识接受并留档的设计取舍（审计 B3-7 建档；"知道并接受"≠"不知道"）：

- **WS token 走 URL 参数**（`/v1/tasks/{id}/ws?token=…`）：浏览器 WebSocket API 无法自定义
  请求头，`Authorization` 头方案在握手期不可行。接受依据：内部部署边界 + access token 短时
  （过期即 401 单飞刷新，旧 token 立即失效）；URL 形式的泄露面（访问日志/代理链）在该边界内可控。
- ~~503 自动重试不区分 HTTP 方法~~（**B5-P2-5 已收窄为仅幂等 GET**：1s/2s/4s 退避 3 次；
  POST 不再自动重试——`clientInterceptors.test.ts` 锁定。写路径幂等兜底口径不变：网关生成
  幂等键、GenerateReport 本身幂等）。
- **报告 HTML 的 `<style>` 原样渲染、不做清理**：在线查看将报告正文经 sandboxed iframe
  （`openReportWindow`：about:blank 宿主窗 + `<iframe sandbox src=blob:>`，sandbox 空token
  无 allow-scripts/allow-same-origin）渲染，不解析/剥离其中的样式标签。接受依据：双保险——
  iframe sandbox 使脚本与同源权限全灭；blob 内容前置 CSP meta（`REPORT_WINDOW_CSP_META`，
  `default-src 'none'; style-src 'unsafe-inline'`）即便 sandbox 通道被降级仍灭脚本。
  样式只能内联生效、无法发起网络请求，残留样式最坏影响布局不影响安全；
  JSON 分支保持转义 `<pre>` 文本（fix-plan-0911 §14 升级）。

## 测试

```bash
npm test            # vitest run，jsdom 环境，src/test-setup.ts 补 antd 所需 matchMedia
npm run guard       # 静态守卫（秒级）：禁区模式拦截 + 防御锚点自检，每次交付提交前必跑
npm run mutation-check  # 变异检验（2-4min，要求干净工作区）：证明历史 bug 回归锁会红
```

测试台 `src/testsupport/fakeGateway.ts` 在 **axios adapter 层**伪造网关，纪律：

1. 只在 HTTP 传输层造假——`api/client.ts` 的真实代码（FormData 上传、参数序列化、401 刷新、503 重试）全量执行；
2. 未建模的路由直接抛错（响亮失败），不做静默空成功；
3. 错误用 `httpError(status, body)` 构造，经真实拦截器链回放（401 刷新/429 退避/503 重试均可测）;
4. 断言请求形状用测试台返回的 `requests` 日志（method/url/query/body/config）。

### 回归防线（防同类 bug 复发）

三道防线见 `docs/regression-guard.md`：①契约锚定测试（docs 两份接口文档的 [E-nn]/[I-nn]
锚点 ↔ 用例双向追溯）；②`npm run guard` 静态守卫（整模块 mock / 死契约复活 / 防御锚点
丢失直接拦截）；③`npm run mutation-check` 变异检验（对 §3 缺陷模式档案中的要害项
注入等价变异，证明对应测试会红——锁失效即红灯；档案/变异随修复批次递增）。缺陷修复工作流：先写红测试 → 修 →
建档（§3 缺陷模式档案加行/加固锁）→ 变异验证 → commit 记账。

## 容器化

```bash
docker build -t codeaudit-web .

# 网关在宿主机 8080：
docker run -d -p 8088:80 -e CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:8080 codeaudit-web

# 或 compose（Linux 的 host.docker.internal 映射已内置 extra_hosts，开箱即用）：
CODEAUDIT_GATEWAY_UPSTREAM=192.0.2.122:8080 docker compose up -d
# 浏览器访问 http://<宿主>:8088
```

> 网关不在宿主机时，`CODEAUDIT_GATEWAY_UPSTREAM` 直接给容器可达的 IP/域名即可。

### 构建与镜像

- 多阶段：`node:20-alpine` 内 `npm ci` + `npm run build`（锁文件独立成层缓存依赖）→ `nginx:1.27-alpine` 托管 `dist/`
- 网关地址经官方镜像 envsubst 机制注入 nginx 模板（只替换已定义的环境变量，nginx 运行时变量不受影响）
- `EXPOSE 80`；自带 HEALTHCHECK（wget 本机 80）
- **镜像身份唯一**：
  `codeaudit-web:latest` 的构建定义只此一处（本仓 Dockerfile + compose）；sim 栈
  改为 image 引用消费，不再以 `build.context: ../web` 越仓编译；compose 顶层
  `name: codeaudit-web` 钉死项目名（目录 basename 不再决定）；`GIT_REV` 构建参数
  注入 OCI revision 标签（部署链自动取源码树 HEAD，`docker inspect` 可查）

### 产物分包（vite.config.ts）

按变更频率拆四块：应用代码 `index`（≈76KB，每版都变）/ react 全家桶 `react`（≈164KB）/ 其余三方 `vendor`（≈126KB，axios/react-query）/ antd 生态 `antd`（≈997KB，antd+icons+rc-*+dayjs，版本随 antd 锁定同步升版）。nginx 对 `/assets/` 30d immutable 缓存下，发版后回访只需重下 index 块。块依赖为单向无环图；antd 单块体积为组件库固有成本，`chunkSizeWarningLimit: 1000` 为有依据接受。

### nginx 要点（nginx/default.conf.template）

- `/`：SPA `try_files` 回退（react-router history 模式）
- `/v1/`：反代网关，`Upgrade/Connection` 头透传 WebSocket（任务详情页 `/v1/tasks/{id}/ws` 推送）
- `client_max_body_size 101m`：代码压缩包上传 ≤100MB（网关 UploadArchive 上限，前端 file.size 预检）+ multipart/编码 1MB 余量（`nginx/default.conf.template` 为 101m 事实源）
- `proxy_read/send_timeout 300s` + `proxy_buffering off`：报告下载/日志聚合等长响应余量
- `/assets/` 产物 hash 长缓存 30d（immutable）；`index.html` 不缓存（发版即生效）；gzip JS/CSS/JSON/SVG

## 验证

门禁三件 = `npm test`（vitest 全量）/ `npm run guard`（静态守卫）/ `npm run mutation-check`
（变异自检，工作区干净前提）；用例数、守卫项数、分包体积一律**以当次门禁输出为准**，
不在本 README 记快照计数（口径沿「导航化不记数」先例，历史验证快照见 git 历史）。
GUI 走查入口：`npm run dev:mock`
（无后端视觉走查）或主仓库模拟栈 + `deploy/internal/tests/ui_check.py`。

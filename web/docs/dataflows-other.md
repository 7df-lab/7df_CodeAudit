# CodeAudit Console — 其余数据流与代码交互（外部/内部接口之外）

> 范围：不属于「HTTP 端点契约」（external-interfaces.md）也不属于「模块调用契约」
> （internal-interfaces.md）的数据流转与代码交互：浏览器本地存储、构建/部署链、
> 代理与反代、浏览器对象流（下载/新窗口）、渲染兜底等。
> 每节标注对应的守卫/测试锚点（编号 [D-nn]，guard.sh 锚定字符串锁其中易复发项）。

---

## D-1 令牌生命周期（浏览器本地存储 × 内存）

```
登录/注册成功:
  access_token  → 模块内存变量（client.ts 私有，永不落盘）      [I-02]
  refresh_token → localStorage['codeaudit.refresh_token']      [I-03]

F5 / 直链进站（SessionProvider 挂载 effect）:
  无 refresh_token → booting=false → Shell 跳 /login
  有 refresh_token → bootRefresh()（裸 fetch POST /v1/auth/refresh）
                     ├─ 成功 → 内存 access 更新 + refresh 滚动覆盖 → GET /v1/users/me → user 就位
                     └─ 失败 → clearSession() → booting=false → /login

401 运行时刷新: 见 external §2（E-41/E-42 单飞链）。
登出: POST /v1/auth/logout（带 access）→ finally 双清（内存+localStorage）→ user=null。
```

要点：access 在内存意味着**任何页面刷新后必有一次 refresh 往返**——booting 状态被
Shell 与 LoginPage 双处消费防闪烁。锚点：session.test.tsx。

## D-2 任务详情实时数据流（WS × 轮询 × 增量游标）

```
            ┌──────────── WS /v1/tasks/{id}/ws 在线（250ms 聚合帧）────────────┐
挂载 → connect┤                                                                │
            └─ 非收束断线 → 立即补拉一次快照回填（116af13：服务端游标已越过
               WS pend 内容，只能经快照补齐）→ 5s 重连循环（终态收束后停）→ 快照轮询 10s 兜底 ←────┘
两条来源共用 absorbSnapshot（internal I-90）：
  logs_after = 最新 log_id（去重集合 SeenIds）
  ai_cursor  = 单调递增字节游标（base64 chunk → UTF-8 追加）
终态且 AI 收束 → 轮询自停 + WS settled 不再重连 + 服务端关连接。
卸载 → clearTimeout + ws.close()（连接泄漏回归：此前只置标志不 close，
       旧挂载 onmessage 持续写缓存、反复进出任务页连接累积——2026-09-06 修复）。
```

锚点：TaskDetailSnapshot.test.tsx、TaskDetailPage.test.tsx（卸载关闭）；变异 M4/M5。

## D-3 轮询器清单与限流预算

| 轮询器 | 间隔 | 自停条件 | 模块 |
|--------|------|----------|------|
| 任务快照（详情页唯一轮询器，ADR-170 四并一） | 10s（`pollIntervalMs` 可被 429 拉长） | WS 在线暂停；终态+AI 收束自停 | TaskDetailPage |
| 未读角标 `['notify-unread']` | 60s | 仅登录态（enabled !!user） | App.tsx |
| react-query 全局 | retry 1；refetchOnWindowFocus false | — | main.tsx |

历史根因：4 个独立轮询器并 1 前请求 20/min 级逼近 07 §7 单用户 50/min 限流（429 冻结页面）。
**禁止新增独立轮询器**——新数据源必须并入快照口或走 WS。

## D-4 浏览器对象流（下载 / 新窗口 / 剪贴板）

| 流 | 路径 | 要点 | 锚点 |
|----|------|------|------|
| 报告下载 | `GET /v1/reports/:id/download`(blob) → `URL.createObjectURL` → 隐形 `<a download="${id}.${ext}">`.click() → revoke | 文件名扩展名由 `reportFileExt(format)`（I-63）；两处复用（ReportsPage/TaskDetail） | dict.test.ts |
| AI 日志下载 | `Blob([text], 'text/plain;charset=utf-8')` → `ai-interaction.ai.log` | 纯前端文本落盘 | AIInteractionLogPanel.test.tsx |
| 在线查看（HTML） | blob 首字节 `<` → `window.open(objectURL)` | — | — |
| 在线查看（JSON） | blob.text → pretty JSON → `<pre>` 写入新窗口，`<>&` 转义防注入 | — | — |
| 报告内联摘要 | getReportContent（text 模式嗅探）→ `JSON.parse(content).summary` 四指标卡 | parse 失败静默不渲染（报告格式漂移不崩页） | clientContract.test.ts |

## D-5 表单/上传的 antd 拦截流

| 流 | 契约 | 锚点 |
|----|------|------|
| 任务向导上传 | `Upload.beforeUpload` → `uploadArchive(file)` → file_id 置 state + fileList done → `return false` 阻断 antd 默认上传；`onRemove` 必须同步清 file_id（否则任务仍走 storage 通道——ADR-202「输入框置灰但列表有文件」矛盾态） | TaskNewPage.test.tsx |
| 项目弹窗上传 | `Upload.Dragger.beforeUpload` → 同上，但 `return Upload.LIST_IGNORE`（受控 fileList 禁 antd 追加）；弹窗重开/提交后清空（防 file_id 残留跨项目） | ProjectsPage.test.tsx |
| 创建后自动链 | 建任务成功 → `autoRunTask`（start）失败仅 warning 不阻塞导航（任务页可手动续走）；建项目成功 → 自动 createTask(config 留空) → autoRun → navigate 任务页 | ProjectsPage.test.tsx |

## D-6 反代与代理链（部署面）

```
浏览器 ──同源──> [dev/preview: Vite proxy /v1 → CODEAUDIT_GATEWAY_URL (ws:true)]
              └─[容器: nginx /v1 → CODEAUDIT_GATEWAY_UPSTREAM]
                    ├─ Upgrade/Connection 头映射（WS 升级透传，ADR-172）
                    ├─ client_max_body_size 30m（25MB 上传 + multipart 余量）
                    ├─ proxy_read/send_timeout 300s + proxy_buffering off（长响应）
                    ├─ /assets/ 30d immutable；index.html no-cache；gzip JS/CSS/JSON/SVG
                    └─ 非 /v1 路径 try_files → index.html（SPA history 路由回退）
```

要点：
- **同源纪律**：客户端代码只发相对路径（baseURL `'/'`），零直连微服务——改绝对地址即违宪（14号 P1）。
- 产物分包（vite manualChunks 四块：index/react/vendor/antd）与 nginx 缓存策略耦合：
  发版回访只需重下 index 块；`chunkSizeWarningLimit: 1000` 为 antd 单块固有体积的有依据接受。

锚点：nginx 模板与 vite 配置为部署期人工验证（preview 冒烟）；guard.sh 锚定 `/v1` 反代存在性（文本级）。

## D-7 错误边界与渲染兜底

| 层 | 兜底行为 | 锚点 |
|----|----------|------|
| `ErrorBoundary`（main.tsx 挂全树） | 渲染期异常 → 可读错误面板 + 重载按钮（白屏=不可诊断的静默失败） | — |
| `zh()` 字典回退 | 未知枚举键回显原键（数据不隐藏） | dict.test.ts |
| TaskLogPanel `hhmmss` | int64 字符串统一 `Number()` 再 new Date（NaN 回归防御） | TaskLogPanel.test.tsx |
| FindingDetailBody `base64ToUtf8` | atob 后必须经 `TextDecoder('utf-8')` 字节层解码（中文乱码回归，2026-08-30 会话#41） | codeContext.test.tsx |
| SourceFileViewer | 行号超界钳制末行；>2 万行截断渲染并如实标注 | codeContext.test.tsx |
| 吸收层 `absorbSnapshot` | 游标/去重兜底轮询与 WS 交叠重复 | TaskDetailSnapshot.test.tsx |

## D-8 构建、容器与部署链

| 环节 | 事实 | 锚点 |
|------|------|------|
| 本地门禁 | `npm test`（vitest run，jsdom；test-setup 补 matchMedia）+ `npm run build`（`tsc -b` 类型门禁 + vite build） | 伞仓 make test-web / build-web |
| 镜像 | 多阶段：node:20-alpine（npm ci + build，锁文件独立成层）→ nginx:1.27-alpine 托管 dist；HEALTHCHECK wget 80 | Dockerfile |
| 部署 | `deploy.sh` 统一契约 `deploy\|check\|status\|start\|stop\|restart\|logs`；收敛式同步源码树 → compose up -d --build → 健康等待 + `/v1` 401 透传断言防假健康 | deploy.sh（伞仓 pb-B/pb-A 联动） |
| 类型门禁范围 | tsconfig include `src`——**测试文件同受 strict 检查**（新测试破坏 tsc = 门禁红） | tsconfig.json |

## D-9 数据形状的「单点锚定」原则（贯穿性约定）

REST 响应形状唯一锚定点 = `api/client.ts` 类型化端层；UI 组件只允许消费该层类型。
历史上 20 处散落 `as` 手写形状曾让 `ProjectsPage res.dir` 死链路存活三个版本（ADR-203 实证）。
配套机制：
- 静态守卫：页面/组件目录禁止 `as {` 形状断言（guard.sh G-01）；
- 契约测试：clientContract.test.ts 逐条锚定 E-xx 请求/响应形状；
- 变异检验：改动形状处理逻辑时 mutation-check.sh 证明测试会红（见 regression-guard.md）。

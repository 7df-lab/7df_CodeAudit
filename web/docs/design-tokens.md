# 视觉设计令牌（design tokens）——代码级 SSOT 的文档面

> 设计整改 2026-09-13（四批落地，P0/P1 引入本体系）。
> 代码事实源：`src/dict/tokens.ts`（语义配色）· `src/theme/index.ts`（antd 主题）·
> `src/theme/evidence.ts`（证据面板色板）。本文只做索引与规则，改值改代码、同步本文。

## 设计方向（"安全实验室"）

个性花在产品最独特的"证据"上：代码、Source→Sink 污点链、severity 阶梯、暗色终端
证据面。亮色组件区保持 antd 默认体系的克制，全站只有一处放胆——发现详情的证据链
与统一碳黑证据面板。

## 色彩

| 角色 | 值 | 事实源 |
|---|---|---|
| 主色·靛墨 | `#3056D3`（colorPrimary/colorInfo/colorLink seed） | `theme/index.ts` |
| UI 字体 | 中文优先系统栈（PingFang SC → HarmonyOS Sans SC → Microsoft YaHei） | `theme/index.ts` `UI_FONT` |
| 表头底 | `#F4F6FB`（极浅靛） | `theme/index.ts` components.Table |
| severity 六级 | 绛红 `#A8071A` > 警红 `#FA541C` > 琥珀 `#FA8C16` > 秋黄 `#D4B106` > 钢灰 `#595959`；UNSPECIFIED 无色 | `dict/tokens.ts` `SEVERITY_COLOR` |
| 任务状态 | antd 语义 preset：RUNNING=processing / COMPLETED=success / FAILED·DEAD=error / TIMEOUT·PAUSED=warning / 其余 default | `dict/tokens.ts` `STATUS_COLOR` |
| AI 结论（verdict） | TRUE_POSITIVE=error / LIKELY_TRUE·NEEDS_MANUAL=warning / 误报族=default | `dict/tokens.ts` `VERDICT_COLOR` |
| 阶段圆点 | 完成 `#52c41a` / 执行中 `#3056D3` / 失败 `#ff4d4f` | `dict/tokens.ts` `STAGE_DOT_COLOR` |
| 证据面板·碳黑 | 底 `#0D1117`（全站唯一暗底）+ 语义 6 色（link/ai/ok/system/warn/accent/error）+ 正文/弱化/边框 | `theme/evidence.ts` `EVIDENCE` |

语义规则（P0 修正的历史撞义，勿回退）：

- **实心色（hex）= 缺陷本身的属性**（severity）；**antd 语义 preset（浅底 pill）= 工作流
  状态**（任务状态/verdict）。两层不混用。
- **红 = 危险/真漏洞**：verdict TRUE_POSITIVE 用红系，FALSE_POSITIVE 用灰——红色绝不
  表示"不是漏洞"。
- severity 强度序必须 CRITICAL > HIGH（旧映射 critical=volcano 弱于 high=red 是倒置）。

## 字体

- **证据嗓音（等宽）**：`MONO_FONT`（JetBrains Mono → SFMono → Menlo → Consolas，零
  webfont）。规则：凡机器产生的标识一律等宽——任务/报告短 ID、项目 ID、文件路径、
  代码、日志、行号。中文回落系统字体，仅 ASCII 走等宽。
- AI 交互面板字号 14/18px 已定版，**保留不动**。

## 版式

- 内容区**全宽流式**：不定宽——1440 定宽在 ≥1920 屏两侧各浪费
  ~240px+，数据密集型控制台应吃满宽度；窄屏走 P2 的媒体查询折叠。

- `PageHeader`（`components/PageHeader.tsx`）：全站统一页首——Title margin 归零
  （修 48px 空带）、与内容区间距 16、"标题+状态位 | 动作位"结构。独立页 level 3、
  嵌套视图（展开行/Tab 内）level 4。
- 状态体系（`components/states.tsx`）：`PageLoading`（页面级加载）/ `ListSkeleton`
  （列表首屏）/ `EmptyState`（空态+行动指引）/ `QueryError`（失败+重试）——全站只用
  这四件，不再各写各的"加载中…"。

## 走查

`npm run dev:mock`：挂 `testsupport/demoGateway.ts` 演示网关（adapter 层，真实 client
拦截器链全量执行），无后端打开全站核对设计；演示数据刻意覆盖 severity 全阶梯、taint
链路、三级别日志与全部代表状态。

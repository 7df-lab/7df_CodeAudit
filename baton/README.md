# baton — 换 agent 不丢任务的交接协议

> 中立任务日志 + 任务脊柱 + 跨 agent 换手。定位：**独立开源产品**（Apache-2.0），
> 孵化于 CodeAudit 项目，零依赖审计业务。平台期品牌预留 **OmniDrive**（阶段③启用）；
> 本仓是细腰模块本体——`.tasklog/` 目录名给 agent 读（描述性），仓名给人看（交棒）。

状态：设计定稿（12 项关键决策见下文决策表）；schema **v0.1** 在
`schema/`（graph.* 六事件入族，）；**M2 投影器 / M3 简报渲染器 /
M4 换手基线锚均已实现**（Go 零依赖，单测+黄金全绿）。输入方言族三选一：
dsh(.sse.log) / maka / **acp**，`-graph` 图日志归并；
S1 真实流实证已过；S2/S3 场景级实证结论见
`docs/integration-test-plan.md`。文档索引：契约三件套
`docs/{api-external,internal-interfaces,data-flows}.md` + `projection-dsh.md` + `adr.md`。


## 开发与测试（本仓门禁）

```bash
cd baton
gofmt -l . && go vet ./... && go test ./...   # 门禁三连
BATON_UPDATE_GOLDEN=1 go test ./...           # 黄金样例更新（改投影/渲染逻辑后先跑此再跑门禁）
go run ./cmd/baton project -sse|-maka|-acp <输入日志> [-graph <path.graph.log>] -out <工作区根> \
  -task T-1042 -goal "…" -tenant acme -user xw [-constraint …可重复 \
  -agent dsh -model … -blob-threshold 2048 -git-anchor …]   # -sse/-maka/-acp 恰选一
go run ./cmd/baton brief -in <工作区根> -format maka|dsh [-tail 20] [-out <file>]
go run ./cmd/baton handoff -in <工作区根> [-phase checkpointed -git-anchor … -tree-ref … -now RFC3339]
```

零第三方依赖（可移植性是产品属性）。无部署、无端口、无镜像。

## 1. 定位：五层栈里的接缝

```
前端层（Zed/web/vscode-plugin） ⇅ ACP ⇅ harness 层（DSH/Maka/opencode/codex/zcode）
  ⇅ provider/descriptor ⇅ 控制面（engine+manager；agent-compose 同格）
  ⇅ 沙箱 driver ⇅ 执行安全层（openshell，生产现役）
```

市场上 ACP 统一了前端接入、openshell 统一了副作用出口、agent-compose 统一了编排声明，
**没有任何一层统一"任务与日志"**。baton 补这个接缝：中立事件日志贯穿所有层，
使"换刀（agent/模型）不丢任务"成为可验证的承诺。

## 2. 共识决策清单

| # | 决策 |
|---|---|
| 1 | 独立开源产品，主仓库新子仓孵化（Go；schema 用独立 JSON Schema） |
| 2 | 场景阶梯：自家审计任务 → 企业内多团队通用任务 → 外部商业客户 |
| 3 | 能力阶梯：①换手 ②北向 ACP 多端 ③openshell+agent-compose 式编排 ④静态 harness 路由表 ⑤网关计费（商业期） |
| 4 | 换手 = 同一 task_id 续跑，agent 只是持笔人变更；dsh-runtime 演进为通用 runtime |
| 5 | 日志生产 v0 = 旁路投影：DSH 从 `.sse.log` 读时翻译（`.ai.log` 是纯文本，禁作输入） |
| 6 | 九族事件 + 脊柱严格域中立（审计语义只进 tool 载荷）+ blob 分离 + usage 顺手收 |
| 7 | 公理：**缺失结果 ≠ 没跑过**——未闭合 tool.call 合成 `outcome:"indeterminate"`，交工作区仲裁 |
| 8 | 快照为真相、保活为优化：换手 = 日志指针 + 工作区指针，不依赖沙箱实例连续性 |
| 9 | 安全点阶梯：L1 回合边界（DoD）→ L2 工具边界（字段 v0 埋好）→ L3 任意时刻（后置，含 park） |
| 10 | 种子契约恒定（`.tasklog/` + 简报 + 尾部）；驱动收敛到 ACP，无 ACP 者用 descriptor 过渡 |
| 11 | 接入顺序：DSH↔Maka（DoD）→ zcode → opencode（原生 ACP）→ codex（codex-acp）→ 其他 |
| 12 | Apache-2.0；换手 DoD（含负向对照）通过后开源，本仓直接转正 |

静态路由表（阶段④，先取巧后实测）：gpt→codex / opus→claude / deepseek→DSH / GLM→zcode / 其他→Maka。

## 3. 事件模型（v0 基础 + v0.1 graph 扩展，）

目录布局（放进任务工作区根）：

```
.tasklog/
  events.jsonl      行流（骨架，小）；seq=行号=因果序=游标
  spine.json        脊柱投影（最新快照）
  blobs/<sha>       大载荷正文（内容寻址，阈值 2048B，可配）
  WORKSPACE.json    工作区身份标记（UUID + git anchor + 前任链）
```

事件族（基础九族 v0 + graph.* 六事件 v0.1；schema/event.schema.json）：

| 族 | 用途 |
|---|---|
| `task.created` / `task.status` / `task.subagent` | 任务生命周期；状态枚举对齐 engine TaskStatus；子任务父子会话 |
| `spine.updated` | 脊柱快照（goal/constraints/**facts**/decisions/open/hypothesis/next） |
| `model.message` | 用户/助手消息（parts: text/reasoning；大文走 parts_ref） |
| `tool.call` / `tool.result` | 工具调用与结果；approval 字段承载权限决策；outcome 含 `indeterminate` |
| `usage.recorded` | token 计量（input=未缓存；计费输入=input+cache_read+cache_write） |
| `context.compacted` | 压缩记账：谁在何时切了什么，spine_unchanged 显式确认 |
| `handoff.*` | requested / checkpointed / seeded / completed——换手全记录 |
| `workspace.checkpointed` | 工作区快照锚（git anchor / 桶级 tree_ref） |
| `graph.*` | 图执行六事件 |

认识论分层：**facts**（已验证真值，带 evidence seq）/ **decisions**（已做决策）/
**hypothesis**（未验证假设）——三层不得混写。

## 4. 换手流程（L1，回合边界）

```
gate.Wait 挂起（回合边界）
  → handoff.requested（记录 from/to/reason）
  → 投影 .sse.log → events.jsonl + blobs/（usage 补收）
  → handoff.checkpointed（log_seq + 工作区 tar/git 锚）
  → 渲染简报 + 尾部 → handoff.seeded（via=prompt|AGENTS.md|task-card）
  → 新 agent 挂同一工作区快照起跑（descriptor/ACP 驱动）
  → handoff.completed（fidelity + holes）
  → 新 agent 任期事件继续 append 同一 events.jsonl
```

L2 字段已埋：配对校验、`indeterminate`、WORKSPACE.json；L3（任意时刻崩溃一致性）
后置——业界实证（Maka 默认关闭自己的 safe-resume）说明其代价。

## 5. 方言适配器族

投影器是**一族**方言适配器，每接一个 agent 配一个：

- **DSH**：输入 = `.sse.log` 原始帧，映射规范见 `docs/projection-dsh.md`；
- **Maka**：事后投影其自有 RuntimeEventLog（每回合节奏），**版本钉死 + 格式漂移金丝雀**
  （schema 校验失败即 park 投影，简报标注不确定窗口）——本体 Apache Maka (Incubating)
  已下载实测（npm nightly 0.2.0-dev.39.20260916），形态/事件映射/凭据纪律/档案交换面

  含 sim 真实工具回合 20 事件样本 testdata/maka-live-sample.jsonl）；
- **ACP**：输入 = engine acpSession 驱动器逐帧留存的 acp-out.jsonl
  （`project -acp`），契约见 `docs/api-external.md` §1 与 `docs/data-flows.md` A-acp；
- 已知缺口：~~帧无时间戳~~ 已解决（R5/TP17-T3：`: ts` 注释透传，旧文件降级）。

## 6. 驱动契约与种子契约

- **种子契约（恒定）**：`.tasklog/` 全档案 + 简报（脊柱投影渲染）+ 尾部逐字，
  对所有 agent 一致；渲染目标按家：Maka=初始 prompt、DSH=任务卡前置段、
  zcode/codex=AGENTS.md。
- **驱动契约（收敛中）**：ACP 为目标方言（opencode 原生 / codex 走 codex-acp）；
  DSH/zcode/Maka 用 provider descriptor 过渡；ACP 的 session 持久化/resume 仍在 RFD，
  换手语义永远在本层不依赖它。

## 7. DoD（换手首发验收，三判据）

1. 真实审计任务 DSH 中途切 Maka，**无人工重述背景**下完成后续步骤，产物过测试/工件核对；
2. 换手瞬间"任务相信什么"可从 events.jsonl + spine.json 回答；
3. **负向对照**：只给工作区不给简报，同类任务失败率显著上升——简报价值可归因。

## 8. 里程碑

| # | 交付 |
|---|---|
| M1 | ✅ schema v0（`schema/`，含 spine/WORKSPACE/L2 字段） |


  勘误：原文「待 S1」系 09-17 前旧口径） |
| M3 | ✅ 简报渲染器 ×2（`internal/brief`：读 .tasklog/ 渲染 maka 初始 prompt / dsh 任务卡前置段；脊柱渲染+机械状态段+尾部逐字，方言只差标题层级。认识论纪律：brief 不自动提炼 facts/decisions——脊柱有什么渲染什么，贫 spine 空段省略。**R122 增补**：投影器侧新增 `internal/project/lessons.go` 机械提炼**失败要点**（kind=lesson，确定性/去重/上限保最新）入 spine.facts，专供 engine graph-reseed 种子挂接；observation 仍不自动提炼（贫 spine 纪律不变） |
| M4 | 换手流程（gate 冻结→投影→快照→种子→Maka 挂载→任期事后投影+金丝雀）——engine 需求 **R1–R8 全部已实施**；种子消费端已接线；Maka 任期投影适配器已落地；金丝雀=PARKED 计档机制（见 data-flows）；S2/S3 场景级实证结论见 `docs/integration-test-plan.md`（R1–R8 全量已实施为最新口径） |
| M5 | DoD 实验（含负向对照）→ Apache-2.0 开源（`docs/integration-test-plan.md` S3） |

v0 明确不做：北向 ACP、编排、路由表执行、计费、L3 任意时刻换手。

## 9. engine 集成点

| 能力 | 位置 |
|---|---|
| 冻结点 gate.Wait | `engine/services/runtime-service/internal/sandbox/pause.go:69-102` |
| 原始帧落盘 writeRaw | `.../internal/service/ai_interaction_log.go:169-173` |
| 任务卡 prompt 注入点 | `.../internal/sandbox/sandbox_analysis.go:219` |
| 桶级工作区 tar 快照 | `engine/services/task-service/internal/service/task_service.go:499-516` |
| teardown 必删沙箱（需绕过） | `.../internal/sandbox/session.go:364-394` |
| 孤儿对账器（需 handoff-hold） | `.../internal/sandbox/reconciler.go:37-67` |
| usage 丢弃点（投影补收） | `.../internal/sandbox/sandbox.go:974-976` |

## 10. 纪律与待办

- [x] git init + 主仓库 submodule 接线（M2 首次交付完成）
- [ ] engine 侧 ADR：JSON Schema 豁免 proto 唯一 SSOT 家规（投影器集成进 engine 前）
- [x] 入口命令已同步主仓库文档地图（project 随 M2、brief 随 M3）
- [ ] 命名分期执行：模块=baton；平台品牌=OmniDrive（阶段③启用）
- [x] `.sse.log` 帧时间戳 → R5/TP17-T3 已落地

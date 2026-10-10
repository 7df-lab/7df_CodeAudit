# baton 外部接口：CLI 面预期输入与输出

> **文档第一性判据**：谁读——引擎侧调用者（engine baton_hook.go 的接线维护）、
> 未来方言 runtime 作者（接入新 agent 时照单调用）、运维手跑排障者。什么变更触发
> 维护——CLI 旗标/退出码/stdout 形状/幂等语义任一变更（本档是 CLI 契约 SSOT，
> 与 README §开发与测试 的速查条目互链；实现=cmd/baton/main.go）。
> **调用形态**：baton 无守护进程、无端口、无 API——外部接口全部是**子进程 CLI
> 面**（架构 §2 钦定的文件+CLI 接缝，R1 钩子模式冻结，禁止 API 化）。

## 0. 通用约定

- 退出码：`0` 成功；`1` 运行失败（输入损坏/IO 错误，stderr 有 `投影失败:`/`渲染失败:`/`换手锚定失败:` 前缀消息）；`2` 用法错误（缺必填/非法枚举，stderr 打 usage）。
- 所有产物路径以"任务工作区根"（`-out`/`-in`）为基准，档案落 `<root>/.tasklog/`。
- 时间源：三个子命令均支持 `-now RFC3339`（测试固定时间；缺省当前时刻）。

## 1. `baton project`（M2 投影器）

### 预期输入（旗标）

| 旗标 | 必填 | 缺省 | 语义 |
|---|---|---|---|
| `-sse <path>` | △ | — | 引擎 `.sse.log` 原始帧路径（DSH 方言）；**必须** `.sse.log` 后缀（`.ai.log` 是人性化纯文本，显式拒绝） |
| `-maka <path>` | △ | — | Maka RuntimeEventLog JSONL（maka 方言）。转译器形态：合成 SSE 帧喂既有投影机（九族映射=sim 真实样本实锚）；金丝雀 schema 漂移行→PARKED 计档+`.tasklog/PARKED` 落盘+简报标注不确定窗口（README §5 纪律），投影继续 |
| `-acp <path>` | △ | — | ACP 帧流 JSONL。转译器形态同 maka（合成 SSE 帧喂既有投影机，零双实现）；映射=initialize→bridge.hello(provider=acp:\<agentName\>)/agent_message_chunk→assistant/message/user_message_chunk→user/message/tool_call→tool/call/turn_done→turn/end；**auto_permission 不投影**（v0：驱动器注记无 toolCallId，审批对账挂不上真实调用——审计留痕在 engine humanLog ⚠ 行）；金丝雀白名单（sessionUpdate 标签/_driver 注记面），漂移行→PARKED 同纪律；ts 留空=投影时刻降级（不虚构） |
| `-graph <path>` | — | — | 图任务第二输入 `<taskID>.graph.log`（v0.1 ；空/不存在=非图任务零影响；ts 归并同刻 graph-first，spine open 折叠） |
| `-out <dir>` | ✓ | — | 任务工作区根；产物写 `<dir>/.tasklog/` |
| `-task <id>` | ✓ | — | task_id（换手不变更的身份键） |
| `-goal <text>` | ✓ | — | 任务目标（脊柱 goal 来源；首行摘要即可） |
| `-tenant <s>` | — | 空 | 租户（envelope 打标，空=档案不带该维度） |
| `-user <s>` | — | 空 | 发起用户 |
| `-constraint <s>` | — | 空 | 任务约束，**可重复**（`-constraint a -constraint b`） |
| `-agent <s>` | — | `dsh` | 持笔 harness 标识（pen.agent） |
| `-model <s>` | — | 流内值 | 覆盖 bridge.hello 模型名 |
| `-blob-threshold <n>` | — | 2048 | 大载荷 blob 分离阈值（字节） |
| `-workspace-id <uuid>` | — | 随机 | 固定工作区 UUID（测试用） |
| `-git-anchor <sha>` | — | 空 | 工作区 git 基准 commit |
| `-now <RFC3339>` | — | 当前 | 固定时间源（测试用） |

### 预期输出

**stdout（成功）**：
```
events=<N> blobs=<M>
  PARKED=<K>…（maka/acp 金丝雀触发时；K=schema 漂移行数）
  <事件类型名>            <条数>     ← ByType 逐行（类型名升序，确定序——stdout 面与档案确定性契约同源）
  ⚠ <警告文本>                      ← 零或多行（撕裂行丢弃/ts 降级/未映射帧/事件
                                       计数/WORKSPACE 再生/他任期前缀等）
```

**文件副作用（幂等语义）**：
- `events.jsonl` / `spine.json`：**全量重写**（SSE 派生段可重算；原子落盘 tmp+rename）；若既有档案尾部是 `handoff.*` 生命周期段，重投影后**原样续写**（seq 连续、ts/载荷保历史原值；中段残留 handoff=脏档案不保留并出 ⚠ 警告；前缀含他任期事件=⚠ 计数告警后重写）。spine.facts 含 projector 机械提炼的失败要点（kind=lesson，R122 v0：确定性重算、去重、上限 20 保最新——engine 注入侧 ≤600 字截断互补；详见 projection-dsh.md 投影规则 4）。
- `blobs/<sha256>`：只增去重（同内容不重写；原子落盘）。
- `WORKSPACE.json`：已存在且 task 相符则**原样保留**（重跑不换身份）；task 不符=**硬闸退出 1**；文件损坏=⚠ 告警+身份再生覆写。

**退出 1 的错误样例**：输入非 `.sse.log` 后缀 / `.ai.log` 误用（专用错误文案指路）/ 输出目录不可建 / `-graph` 指向的文件扫描失败（超 1MiB 行或读错误——不静默截断剩余 graph 事件）/ WORKSPACE task 硬闸。

## 2. `baton brief`（M3 简报渲染器）

### 预期输入

| 旗标 | 必填 | 缺省 | 语义 |
|---|---|---|---|
| `-in <dir>` | ✓ | — | 任务工作区根（读 `<dir>/.tasklog/`） |
| `-format <s>` | ✓ | — | `maka`（初始 prompt，H1 起）\| `dsh`（任务卡前置段，H2 起）——两方言只差标题层级，内核同一 |
| `-tail <n>` | — | 20 | 尾部逐字事件条数；`0`=省略该段 |
| `-out <file>` | — | stdout | 输出文件路径（写文件则 stdout 无输出） |

### 预期输出

- **文本**：`简报 = 标题 + 元信息段 + 脊柱段（goal/constraints/facts/decisions/open/hypothesis/next，空段省略）+ 机械状态段（终态/indeterminate 调用/换手史）+ 尾部逐字 N 条`。认识论纪律：不自动提炼 facts/decisions——脊柱有什么渲染什么。
- **确定性**：纯读档案、无副作用、无时间源注入——**同一档案必出同一文本**（黄金测试锁定）。
- 退出 1：档案缺失/行非法；退出 2：未知 `-format`。

## 3. `baton handoff`（M4 基线锚，v0 仅 checkpointed）

### 预期输入

| 旗标 | 必填 | 缺省 | 语义 |
|---|---|---|---|
| `-in <dir>` | ✓ | — | 任务工作区根（含 `.tasklog/`） |
| `-phase <s>` | — | `checkpointed` | v0 仅此值（其余即退出 2） |
| `-git-anchor <sha>` | — | 空 | 工作区 git 基准（可得则传） |
| `-tree-ref <s>` | — | 空 | 工作区快照桶引用（R3） |
| `-now <RFC3339>` | — | 当前 | 固定时间源 |

### 预期输出（两形态）

- 正常：stdout `checkpointed seq=<N>`——append 一条 `handoff.checkpointed{log_seq=当前事件行数, workspace.git_anchor, workspace.tree_ref}`，seq 承接档案末行+1。
- **幂等防重**：尾部已是 checkpointed 且锚定即当前全量（SSE 无增长的重复锚定，冻结回调重试场景）→ stdout `skipped: 基线已锚定（seq=<N>）`，不 append。
- 退出 1：`.tasklog/` 或 `events.jsonl` 缺失 / events 为空（"无基线可锚"，调用方降级不阻断）/ 行非法 / **seq 与行号不符（档案有洞/重号——log_seq 锚前提被破坏，R121 起 fail-closed 拒绝锚定）**。
- **单实例调用约定（注记）**：Checkpoint 为读-算-append 非原子，同一档案并发调用会产出重复 seq——引擎侧单调用方（冻结回调串行）假设成立；多实例并发属未定义行为。

## 4. 版本兼容面（调用方须知）

- **R5 帧时间戳双向兼容**：输入 `.sse.log` 含 engine writeRaw 的 `: ts <RFC3339Nano>` 注释行 → 帧驱动事件 ts=帧接收时刻；旧文件无注释 → 降级投影时刻（不报错）。非法注释=帧级校验降级+警告。
- **输入只认单 data 行帧**（与 bridge.mjs 发送侧一致）；多 data 行拼接不支持（projection-dsh.md 已知边界）。
- 末行撕裂（data JSON 截断）**丢弃+警告**，不作事件。


> （退出码三态/stdout 首行形状/usage 文案/幂等防重双形态）；brief 方言枚举自本批
> 起 CLI 层预校验（未知 `-format` → 2，对齐 §0"非法枚举=用法错"与 handoff phase
> 同型先例——此前透传 Render 报 1 属契约矛盾，被该测试当场抓出修正）。

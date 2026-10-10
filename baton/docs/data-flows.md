# baton 数据流转与代码交互

> **文档第一性判据**：谁读——需要全链视角的会话（排障"某个文件是谁写的/谁读的"、
> 换手联调、engine 接缝核对）。什么变更触发维护——任一流的产消方变更。
> 前两份档（api-external=CLI 面 / internal-interfaces=包间签名）管"接口形状"，
> 本档管"数据从哪来到哪去"。

## 流 A：投影主链（.sse.log → .tasklog/ 档案）

```
engine writeRaw 落盘 .sse.log          [engine 侧产生，baton 只读]
  │  （帧首前有 ": ts <RFC3339Nano>" 注释行，R5）
  ▼
sseframe.Scan ── Frame{Name,Data,Ts} ──▶ project 帧名分派
  │                                       （映射规范=projection-dsh.md）
  ▼                                    sessionEvent / assistantChunk / toolCall /
project 会话状态机                        toolResult / usage / …
  │  turnOpen/turnNo、text+reason 缓冲（回合收束或 message 权威版提交）、
  │  openCalls（tool.result 帧处闭合；回合收束未闭合=合成 indeterminate 兜底）、
  │  approval 粘性挂载（preset 会话级预授权挂下一个 tool.call）
  │  扫描完成后 extractLessons：失败要点机械提炼 → spine.facts（kind=lesson；
  │     R122 v0——去重/上限 20 保最新/120B 截断/FAILED 终态回带 evidence seq）
  ▼
emit → model.Event（seq 单调=行号）──超阈值载荷──▶ store.Put → blobs/<sha256>
  ▼
writeOutputs：
  events.jsonl（SSE 派生段全量重写 + 尾部 handoff.* 段原样续写）
  spine.json（最新脊柱快照）
  WORKSPACE.json（首跑创建，重跑保留=身份不换）
```

**ts 语义分层**：帧驱动事件 ts=帧接收时刻（": ts" 注释）；扫描前合成（task.created/spine 更新）与 EOF 合成（未闭合 tool.result）恒投影时刻；非法注释=帧级降级+警告。

**撕裂行**：末行 data JSON 不完整 → 丢弃+警告（投影规则 1，ValidJSON 裁决）。

## 流 A-maka：Maka 方言投影

```
engine makaSession 回合收束（teardown 前）导 runtime_events 表
  │  （execIn python3+sqlite3 只读 → JSONL；runtime.sqlite=持久层——"the log

  ▼
<tasklog>/<taskID>/maka-events.jsonl
  ▼
baton project -maka：转译器（internal/project/maka.go）合成 SSE 帧喂既有投影机
  │  九族映射=sim 真实样本实锚；金丝雀：schema 漂移行→PARKED 计档+
  │  .tasklog/PARKED 落盘+brief 标注不确定窗口（投影继续，丢行=丢档案）
  ▼
.tasklog/ 四件套（与 DSH 流同构；pen.agent=maka）
```

## 流 A-acp：ACP 帧流投影

```
engine acpSession 驱动器逐帧留存 JSON-RPC 线序（响应/session/update/_driver 注记；
acp_session.go，baton 只读）          [engine 侧产生]
  ▼
acp-out.jsonl
  ▼
baton project -acp：转译器（internal/project/acp.go）合成 SSE 帧喂既有投影机
  │  映射=initialize(id=1)→bridge.hello(provider=acp:<name>)/user_message_chunk→
  │  user/message/agent_message_chunk→assistant/message/tool_call→tool/call/
  │  turn_done(_driver)或 prompt 响应(id=3)→turn/end；
  │  auto_permission 不投影（注记无 toolCallId，审批对账挂不上真实调用——
  │  代答审计在 engine humanLog ⚠ 行）；金丝雀白名单（sessionUpdate 标签/
  │  _driver 注记面），漂移行→PARKED 同纪律；ts 恒空=投影时刻降级（不虚构）
  ▼
.tasklog/ 四件套（与 DSH 流同构；pen.agent=acp）
```

注：engine 另有 bridge --acp 桥形态（把 ACP agent 伪装成 dsh runtime，输出 dsh
方言 SSE）——该路径走流 A（.sse.log），不经本流；本流消费的是管道式 v0 的原始
ACP 线序留存。

## 流 B：换手链（L1 回合边界）

```
engine gate 冻结点（回合收束、SSE 停止增长）        [engine 侧时序，baton 被调]
  │
  ├─▶ baton project（重投影：SSE 增长进派生段，尾部 handoff 段保历史）
  ├─▶ baton handoff（append handoff.checkpointed{log_seq, git-anchor, tree-ref}；
  │     SSE 无增长的重复锚定=幂等 skipped）
  └─▶ baton brief -format maka|dsh（渲染种子简报：脊柱+机械状态+尾部逐字）
        │
        ▼
      新 agent 挂同一工作区快照起跑（descriptor/ACP 驱动，engine 侧动作）

        │  events.jsonl append 非 handoff 事件，下一次 baton project 重投影会
        │  重写派生段（他任期事件被覆盖，⚠ 计数告警不再静默）；handoff.* 段
        │  永不被重写。任期分段（每段标 pen、重算只覆盖本段）为后续设计项。
        ▼
      handoff.completed（fidelity+holes，M4 后续阶段）
```

## 流 C：简报链（档案三源 → 文本）

```
events.jsonl（原文行+解析双持）──┐
spine.json ──────────────────────┼─▶ brief.Render → 标题+元信息+脊柱段
WORKSPACE.json ──────────────────┘      +机械状态段+尾部逐字 N 条
                                        （纯读无副作用，同档案同文本）
```

## 与 engine 的代码交互面（文件接缝，baton 侧只述不改）

| 交互 | engine 侧位置 | baton 侧消费 |
|---|---|---|
| `.sse.log` 原始帧+`: ts` 注释 | writeRaw（ai_interaction_log.go） | sseframe.Scan 输入 |
| 投影触发（回合收束 defer） | baton_hook.go batonProjectTurn | `baton project` 调用者 |
| 冻结回调 | registerBatonFreeze→WaitFrozen | `baton project`+`baton handoff` 调用时序 |
| 模板挂载 | sim/deploy compose BATON_BIN | 二进制可达性 |
| usage 消费（engine 侧） | task-service usage_collector 读 events.jsonl | **反向流**：baton 档案被 engine 读（归集） |

## 幂等/重入矩阵

| 操作 | 重入语义 |
|---|---|
| `baton project` 重跑 | 派生段全量重算重写（**原子落盘** tmp+rename）；handoff 尾段续写；blobs 去重只增（原子写）；WORKSPACE 保留（损坏→⚠ 再生覆写；task 不符→硬闸退出 1）；他任期前缀事件→⚠ 计数告警 |
| `baton handoff` 重试 | SSE 无增长→幂等 skipped；有增长→正常 append 新锚（append 后 fsync） |
| `baton brief` 重渲染 | 纯读，同档案同文本 |
| 交错（project↔handoff） | handoff 段永远在尾部；project 重写不吞 handoff 段（中段残留=脏档案丢弃+警告） |
| graph.log 输入 | 非法行丢弃+警告；扫描错误（超 1MiB 行/读错）→退出 1（不静默截断） |

# baton 内部接口：包间预期输入与输出

> **文档第一性判据**：谁读——仓内贡献者（改任一包前核契约）、新方言适配器作者
> （第二个投影器沿 §I4 形状）、测试作者（锁接口不变量）。什么变更触发维护——
> 任一 §I 节的签名/契约变更。依赖方向（编译期强制）：
> `cmd/baton → {project, brief, handoff} → {model, sseframe, store}`；
> brief/handoff 互不依赖、不依赖 project/sseframe/store（纯档案消费面）。

## I1 `sseframe`：帧解析（.sse.log → Frame 流）

- `Scan(r io.Reader, fn func(Frame) error) error`
  - 预期输入：SSE 文本流（`event: <名>` / `data: <JSON 原文>` 行、空行分隔、`: ping` 注释心跳、`: ts <RFC3339Nano>` 帧时刻注释）。
  - 预期输出：逐帧回调 `Frame{Name, Data, Line, Ts}`——Data 为**未解析 JSON 原文**；Line=1 基 data 行号（告警定位）；Ts 只作用紧随的一个帧（组帧即消费，不留存）。
  - 契约：撕裂尾行**不猜**（由调用方 `ValidJSON` 裁决）；单 data 行假设（发送侧 bridge.mjs 同构）；fn 返回 err 即中止。
- `ValidJSON(s string) bool`——完整 JSON 判定（撕裂检测）。
- 缓冲上限 16MiB/行（submit_findings 大参数面）。

## I2 `store`：内容寻址正文库

- `New(dir string) (*BlobStore, error)`——建目录（幂等）。
- `Put(b []byte) (string, error)`——预期输出 `blob:<sha256 前 16 位>`；同内容去重、已存在不重写、进程内已写记录短路。
- `Count() int`——本次进程新建数（Stats.Blobs 来源）。

## I3 `model`：事件模型（包间 DTO + JSON 契约）

- `Event`（envelope）：`seq（=行号=因果序=游标）/ ts / task / tenant? / user? / session?（缺省 main）/ pen{agent,model?} / type` + 载荷指针字段（omitempty——每型事件恰一个载荷，schema/event.schema.json 是形状 SSOT）。
- 载荷族：基础九族（v0 事件模型）+ `Subagent`（subagent.started/finished）+ graph 族七载荷（`GraphNodeDeclared/Started/Result/PlanTriggered/Paused/Converged` + `GraphAudit`——v0.5 认领/重开/人消息审计，五事件共用；/§2.2）。`Spine`（goal/constraints/facts/decisions/open/hypothesis/next——认识论三层不得混写）、`Message`（parts: text/reasoning，大文 parts_ref）、`Call/Result`（outcome 含 `indeterminate`=公理"缺失结果≠没跑过"）、`Usage`（input=未缓存，计费输入=input+cache_read+cache_write）、`HandoffCheckpoint{log_seq, workspace.git_anchor/tree_ref}` 等。
- `WorkspaceFile`（WORKSPACE.json 身份标记）/`Pen`（持笔人）。

## I4 `project`：DSH 方言投影器（方言适配器参考形状）

- `Run(opts Options) (*Stats, error)`
  - 预期输入：Options（CLI 旗标全映射，见 api-external §1）。前置校验：`-sse/-maka/-acp` 三选一恰一 + 各自后缀（`.sse.log` 认、`.ai.log` 专用拒绝文案；maka/acp 认 `.jsonl`）；maka/acp 经转译器（maka.go/acp.go）合成 SSE 帧喂同一投影机（零双实现）。
  - 预期输出：`Stats{Events, ByType, Blobs, Warnings}` + 四类文件副作用（幂等/双段语义见 api-external §1）。
  - 内部链：sseframe.Scan → 帧名分派（session/assistant/message/tool/usage…，映射规范=projection-dsh.md）→ 会话状态机（回合开闭/text+reason 缓冲/openCalls 闭合/approval 粘性）→ model.Event → store.Put（超阈值载荷）。
  - **新方言适配器作者须知**：方言差异只允许收在帧分派层（frame/sessionEvent/assistantChunk…）；emit/spine/输出写入是方言中立层，不得复制。

## I5 `brief`：简报渲染器

- `Render(opts Options) (string, error)`
  - 预期输入：`Options{InRoot, Format(maka|dsh), Tail}`。
  - 预期输出：简报文本（段序与确定性见 api-external §2）；错误=方言未知/档案缺失/行非法。
  - 内部链：读 events.jsonl（原文行+解析双持）+ spine.json + WORKSPACE.json 三源 → renderer（方言 profile 只差标题层级）。
  - 契约：**纯读、无副作用、无时间源**——同档案同文本。

## I6 `handoff`：换手事件 append 面

- `Checkpoint(opts Options) (seq int, skipped bool, err error)`
  - 预期输入：`Options{InRoot, GitAnchor, TreeRef, Now}`。
  - 预期输出三态：正常 append（seq=新行号，skipped=false）；幂等跳过（尾部即 checkpointed 且 `log_seq == len(evs)-1`，skipped=true）；错误（档案缺失/空 events/行非法/**seq 与行号不符**（fail-closed：log_seq 锚前提被破坏）——调用方降级）。
  - 契约：只 append 不重写；envelope 抄末条事件（task/tenant/user/pen 延续）；`log_seq`=锚定前行数（基线指针语义）。

# baton 实现决策记录（xx）

> 对齐 engine/.agent/decisions.md 的 xx 体例：实现级决策（架构共识在
> README §2 决策清单，修改需 grilling 会话；本档记"选项+理由"的实施裁决）。

### graph.* 事件族入族+双输入投影（-graph 第二输入）

- **域中立纪律同 spine**（C4）：载荷字段只用控制面词表——nodeId/type(枚举)/intent(一句话自然语言)/declaredBy/from/outcome(枚举)/resultRef/trigger(枚举)/openNodeIds/summary。禁业务 schema 词；severity/findings 等审计语义只可能在 resultRef 的文本摘要里搭车（≤200 字，报告视图过滤面）。
- **D1 分道保持**：outcome 枚举 completed|failed|rejected|indeterminate 与 tool.result 的 outcome 族同精神（拒答≠失败≠没跑完）。
- **双输入归并（ts graph-first）**：projector Options.GraphPath 非空时读 graph.log（JSONL {type,seq,time,data}，engine graphEmitter 形状）→与 .sse.log 帧流按时刻归并；**同刻 graph 先行**——engine 发射纪律保证因果安全（node_started 先于其 session 帧、node_result 后于其终帧，设计 §1/§9），投影侧按 time<=frameTs 插队实现。降级边界：无 `: ts` 的旧 SSE 帧不触发 graph 插队（graph 事件落 SSE 段后）——图任务的 .sse.log 全量带 ts（之后产出的新档），老档无 graph 输入，语义不冲突。
- **spine 折叠**（§1）：node_declared→spine.Open 增；node_result 终态（completed|failed|rejected）→Open 减；indeterminate 留 Open（与 engine GraphState 折叠规则逐字一致——engine 跨仓契约；两侧规则漂移由 golden 夹具+engine 回放测试共同锁）。Open 变化即发 spine.updated（图模式事件量 ≤ 节点数×2，无洪水）。facts 走既有 submit 通道不重复入 spine（§1 明示）。
- **seq 重排**：归并后统一 len(events)+1 连续分配（handoff 尾段续写同构）；graph.log 自身的 seq 不进 events.jsonl（engine 内部因果序，投影序才是档案因果序）。
- **session 归属**：graph 事件的 session=engine 帧的 sessionId（node_started/node_result 带 g-<nodeId>；declared/plan/paused/converged=main）——与 task.subagent/model.message 的会话标签同一消费面（B1 实证 model.message session=g-n001 先例）。
- **消融记录（砍掉的）**：①graph.log 撕裂行重试/修复（消费侧丢弃+Warning，同 SSE 撕裂纪律）；②graph 事件的 blob 分离（载荷全小于阈值面——intent/resultRef 均 ≤200 字契约）；③-graph 必填校验（空=无图任务，非错误——向后兼容）。
- **验证**：schema JSON 合成样例过校验器（golden_graph 夹具）；双输入归并 ts 序断言；spine 折叠等价断言（与 engine 回放测试同夹具形状）。

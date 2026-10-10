# 联调联试计划（M2 实证 → M4 换手 → M5 DoD）


> 联调属跨仓集成（pb-A），实施时须：`codeaudit-sim` 栈级认领（主仓库 U3）→
> `docs/playbooks/pb-A-integration.md` 流程 → 证据原始输出归档 `.agent/evidence/`

> BATON_BIN 编排接线；六项结论与三缺陷修正见 projection-dsh.md「已知缺口」；

> S2/S3 场景级实证已全部达成。**
> **S2 dsh 换手内核已实证**：
> pause→gate 冻结→投影(events=36)/手动 handoff 锚(seq=37)→重投影幂等(handoff 段原位保留)→
> brief 渲染，全链 PASS。三新发现（补丁回合不受闸门/pause 中段锚丢失/pause 尾段滞留 RUNNING）


> A 换手×10 vs C 直跑×10 vs B 负向×10，三判据全 PASS——判据1 A findings 12/漏检 0
> vs C 4/7 无劣化；判据2 盲测完整性 30/30+findings 溯源 29/30 逐字+1 语义改写；
> 判据3 B 零工具/零完成回合/漏检 2 显著回退；subagent 样本 0 触及保持开放。
> 证据=主仓库 .agent/evidence/baton-s3-20260930/。开源转正触发条件已满足，执行留。**

## 前置依赖矩阵

| 场景 | 依赖 engine 需求 | 依赖 baton 里程碑 |
|---|---|---|
| S1 投影正确性实证 | 无（只读 `.sse.log`，零侵入） | M2 ✅ |
| S2 L1 换手全链 | R1 R2 R3 R7（优化）R6（Maka 接入） | M3 M4 |
| S3 DoD 三判据 | S2 全部 | M5 |

## S1 投影正确性实证（第一个可做，无 engine 依赖）

**目标**：合成夹具 → 真实 `.sse.log` 的校准（夹具依 engine 源码实读构造，未经真实流验证）。

步骤：
1. sim 栈（`make deploy-sim`）跑 1–2 单真实审计任务至完成；
2. 取容器卷 `data/ai-interaction/<taskID>.sse.log`（共享卷 `/data/repos/ai-interaction`）；
3. `baton project` 投影，逐类核对：
   - usage 数字与引擎侧已知量级对照（若引擎无对照面，记录绝对值供建基线）；
   - `submit_findings` 参数 blob 与 result 面板发现清单一致性（title/file_path/start_line）；
   - **`approval/asked/decided` 真实字段形状**（投影器的 decision 归一化是否成立）；
   - **tool.result 配对启发式**（干净回合→completed 近似）是否成立，不成立则改用
     turn/end reason + 下一回合 user/message 首块组合判据；
   - 子任务（subagent.*）与 session envelope 覆盖率；
   - 0.1.5 `session.stream` 与 `assistant/message` 内嵌 stream 双通道是否双记（防双记逻辑实证）。
4. 差异项回写 `projection-dsh.md` 与夹具； `.tasklog/` 样本归档 `.agent/evidence/`。

**验收**：真实流投影无 panic、无未识别告警风暴；上述六项逐一有结论（成立/修正）。

## S2 L1 换手全链（DSH → Maka）

前置：R1–R3、R7 已实施；M3 渲染器（Maka 初始 prompt + `.tasklog/` 外置）与 M4 流程就绪。

步骤（sim 栈）：
1. DSH 跑真实任务至第 N 回合，经 PauseTask 链在 gate 冻结（R2）；
2. 冻结点投影 + 快照（tree_ref）→ handoff.* 四事件落 events.jsonl；
3. 简报渲染（spine + 尾部逐字）注入 Maka 初始 prompt；Maka 经 descriptor（R6）挂同一
   工作区起跑；沙箱保活优化（R4）观察项；
4. Maka 任期事后投影（RuntimeEventLog → 中立事件，版本钉死 + 金丝雀）；
5. 全程 events.jsonl 单文件连续（task_id 不变，pen 变更点=handoff.*）。

**验收**：换手无人工重述；Maka 首回合动作含对工作区/简报的核对行为（可从其任期
model.message 观测）；中断演练（冻结后杀进程）重投影幂等。

## S3 DoD 三判据（含负向对照）

1. **接手完成**：≥10 个真实审计任务 DSH→Maka 中途换手，产物过 result 面板核对
   （发现/补丁有效性与直跑组无显著劣化）；
2. **状态可查**：任选换手点，"任务此刻相信什么"能且只能从 events.jsonl+spine.json
   回答（评审人盲测）；
3. **负向对照**：同任务集 A/B 两组——A 组完整种子（简报+尾部+`.tasklog/`），
   B 组只给工作区快照。B 组失败率/回退步数显著高于 A 组 → 简报价值可归因。

**证据**：全部判据的命令原始输出归档 `.agent/evidence/`（U8 口径）；S3 通过即触发
Apache-2.0 开源转正（共识决策 #12）。

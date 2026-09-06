# pb-A — 协同测试部署（模拟栈 + e2e）

> 适用：多仓集成验证、升级后回归、部署演练、e2e 排障。
> 前提：本机无 docker 引擎时，在 docker 宿主（LXC 107 或任意 docker 机）执行栈操作；
> 模拟栈与生产可在同一 daemon 共存（独立 project `codeaudit-sim` / 网段 / 端口段）。

## 前置

1. `make status` —— 各子仓 分支/领先/落后/未提交 一览（每天开工第一件事）。
2. `make pull` —— 全部子仓 `--ff-only` 快进。**被拒绝 = 某子仓有未推提交**：
   先弄清是谁的（U3，可能是并行会话），推完或等其收尾，不要强推。
3. `bash .agent/session.sh claim codeaudit-sim "e2e回归"` —— 模拟栈整体互斥。
4. 若 `deploy/env.sim` 不存在：按 `deploy/env.sim.example` 建（真实值 gitignored；
   `OPENSHELL_MANAGER_TOKEN` 留空则用例 07 走"诚实降级"断言）。

## 步骤

```bash
make deploy-sim        # 构建+启动全栈：base compose + sim overlay；等 gateway 健康（上限 420s）+ PG 种子
make logs-sim gateway  # 确认 gateway 起来（健康由 deploy-sim 自动等，卡住再看日志定位）
make test-sim          # e2e 九用例（deploy/tests/run.sh）
```

- 单用例重跑：`bash deploy/tests/run.sh 08`（08=项目级上传→自动任务全链，GUI 用户
  路径回归锚点；04=任务自带上传件的直连路径；09=可观测面）。
- **部署接线变更（compose env/卷/网络/档位）必须过 `python3 deploy/check-wiring.py`**
  （sandbox-deploy check 已自动带）——接线缺陷对单测/契约/e2e 幸运路径全部隐形
  （LESSONS #10）。
- **GUI 人工全流程**（登录→上传→任务→AI 交互日志→终态核验）见
  [docs/manual-test-guide.md](../manual-test-guide.md)（模拟栈口径，2026-09-05 自 engine 迁入）。
- 用例 07（AI 链路）依赖栈外 manager/沙箱镜像/LLM 出网——不可达时断言**诚实失败**（终态
  FAILED/DEAD 且 error_message 非空）也算通过；这是设计口径，不是缺陷。
- 长任务模式 C/D/E 不在常规 e2e 内（10 号测试计划口径：里程碑级手动用例）。

## DoD（完成判据）

- `tests/run.sh` 输出 9 用例全过（07 AI 链路按诚实失败口径过），**原始输出归档**
  `.agent/evidence/sim-e2e-<日期>.log`（本机，不入 git）。
- 起栈失败或用例失败 = 未完成，进入失败出口。

## 失败出口

- 起栈失败 / 容器不健康 → [pb-E-recovery.md](pb-E-recovery.md)（先 `compose config` 验 parse，再 logs 定位）。
- e2e 用例失败 → pb-E 的"e2e 失败定位"节；定位到**某子仓缺陷** → 转
  [pb-B-iteration.md](pb-B-iteration.md) 修复（先在模拟栈上复现证据留档，再进子仓）。
- 需要彻底重置：`make destroy-sim`（down -v，数据卷清除）再 `make deploy-sim`。

## 收尾

```bash
bash .agent/session.sh release codeaudit-sim
# 账本 .agent/status.md 记一行：时间 | A | sim-e2e | 结果(用例通过数/失败项) | 证据路径
# 栈保留（下次可复用数据卷）或 make down-sim（保卷停栈），按需
```

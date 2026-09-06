# AGENTS.md — CodeAudit 伞仓工作区宪法（AI 会话第一入口）

> 任何 AI 会话（coding agent）在伞仓根目录开工前必须完整读本文件。
> **两级宪法**：本文件管【跨仓编排 · 栈操作 · 并行协同】；进入子仓目录后，
> 子仓宪法优先——`engine/AGENTS.md`、`dsh-runtime/AGENTS.md` 是各自子仓的宪法，
> 其余子仓（web/vscode-plugin/manager/openshell-gateway/dsh-pentest-sse）入口 = 各自 README。
> 冲突时：子仓事务从子仓宪法，跨仓事务从本文件；仍冲突，以时间上更后的人类指令为准。

---

## 0. 伞仓文档地图（AI 视角）

```
AGENTS.md（本文件）     宪法层：模式路由 / 红线 / 并行协议 / 权限边界
docs/dev-prod-map.md    事实地图：逐仓开发/测试/生产命令与关键文件、端口总表、SSOT 清单
docs/playbooks/         操作手册：五类会话的步骤级流程（含命令、DoD、失败出口）
LESSONS.md              九类可复发问题档案（改部署/写配置/跑迁移前先翻）
deploy/README.md        部署纪律事实源：三层环境口径、四步自检清单
.agent/status.md        伞仓工作账本（跨会话对账，只增不删）
.agent/session.sh       并行互斥：claim/release/show（锁在 .agent/sessions/，本机）
```

## 1. 开工三问（先定位，再动手）

1. **我要做哪类工作？**——下表五选一，读对应 playbook 再动手：
   | 模式 | 什么时候 | 手册 |
   |------|----------|------|
   | A 协同测试部署 | 多仓集成验证 / 模拟栈 / e2e / 升级回归 | `docs/playbooks/pb-A-integration.md` |
   | B 子项目迭代 | 单个子仓的功能或修复 | `docs/playbooks/pb-B-iteration.md` |
   | C 沙箱镜像链路 | dsh-runtime 或 dsh-pentest-sse 变更 | `docs/playbooks/pb-C-sandbox-image.md` |
   | D 生产部署 | 上线 / 收敛 LXC 107 | `docs/playbooks/pb-D-prod-deploy.md`（须人类指令） |
   | E 故障恢复 | 任何步骤失败 / 栈异常 | `docs/playbooks/pb-E-recovery.md`（症状→处置表） |
2. **我要动的范围被占用了吗？**——`bash .agent/session.sh show`，需要写操作就先 `claim`（§3）。
3. **我的完成判据是什么？**——对应 playbook 的 DoD。环境缺工具/缺栈 = 如实 blocked，
   禁止用 mock 或转述冒充通过。

## 2. 红线（违反任何一条 = 交付无效）

| # | 红线 |
|---|------|
| U1 | **两级宪法**：子仓内工作以子仓 AGENTS.md / README 为准，本文件不覆盖子仓红线；伞仓层不做子仓内部设计裁决 |
| U2 | **伞仓层禁止直接修改子仓文件**——迭代开发必须 `cd` 进子仓进行。伞仓 git 面只有：`deploy/`、`docs/`、`.agent/`、`README.md`、`LESSONS.md`、`Makefile`、`.gitmodules` 与子仓指针 |
| U3 | **认领后写**：动子仓工作树或模拟栈前必须 `session.sh claim`；他人已认领的范围只读不改；每次提交前 `git status` 核对文件归属，发现非本会话文件混入即停（LESSONS #9） |
| U4 | **测试部署一律走容器**（playbook 流程）；宿主机裸栈已退役（deploy/README 人类指令 2026-09-05） |
| U5 | **生产部署是外向动作**：只读 `check` 通过 + 四步自检（deploy/README）+ 有人类指令，三者齐备才执行；生产容器只允许经 deploy.sh 变更（LESSONS #6） |
| U6 | **结构化配置提交前必须 parse 验证**（compose config / yaml 查重键 / toml 解析，LESSONS #8） |
| U7 | **事实源纪律**：改了入口命令、端口口径、SSOT 文件，必须同 commit 同步 `docs/dev-prod-map.md` 与受影响 playbook；文档断言必须实测，禁止想当然（LESSONS #5） |
| U8 | **诚实降级**：门禁跑不了 / 环境缺失 → 账本记 blocked 并写明差距；e2e / 部署的结论只认命令原始输出（归档 `.agent/evidence/`，本机不入 git），不认任何转述 |

## 3. 并行会话协议

- **认领**：`bash .agent/session.sh claim <scope> "<说明>"`
  scope = 子仓名（engine / web / vscode-plugin / manager / openshell-gateway / dsh-runtime / dsh-pentest-sse）
  或 `codeaudit-sim`（模拟栈整体，栈级互斥）。原子创建，被占则报持有者并拒绝。
- **释放**：收尾必 `release`。锁是本机文件；跨机器的协同可见性靠 status.md 账本行（随 git 走）。
- **账本**：`.agent/status.md` 只增不删——认领、完成、部署、协同事件各记一行，收尾时回写。
- **陈锁**：`show` 对超过 24h 的锁标注 ⚠；确认持有会话已消亡才可 rm，并在账本记一行清理原因。
- **冲突已发生**：以时间上更后的人类指令为准；禁止机械回滚他人改动（可能破坏已授权工作，LESSONS #9）。

## 4. 权限边界（伞仓层）

| 允许 | 禁止 |
|------|------|
| 修改 `deploy/`、`docs/`、`.agent/`、`README.md`、`LESSONS.md`、`Makefile` | 在伞仓层直接改子仓文件（U2） |
| 子仓指针落账（`make pin MSG="..."` 或随交付 commit 推进指针） | 绕过 deploy.sh 手工操作生产容器（U5，LESSONS #6） |
| 模拟栈全生命周期（`make deploy-sim / test-sim / down-sim / destroy-sim / logs-sim`） | 无人类指令跑 pb-D 生产部署 |
| 修改本文件以外的全部伞仓文档 | 修改本文件（宪法变更需人类批准） |

## 5. 快速指针（路径为伞仓根目录相对，直接 read/grep，不要猜）

| 我需要… | 去哪 |
|---------|------|
| 某仓怎么开发 / 测试 / 构建产物 | `docs/dev-prod-map.md` §2 |
| 某端口归谁 / 网段口径 | `docs/dev-prod-map.md` §5 |
| gitignored 文件缺了怎么重建（token/opengrep/…） | `docs/dev-prod-map.md` §4 |
| 某脚本默认值是不是旧世界残留 | grep 旧标识 + LESSONS #2 + `sandbox-deploy.sh check` |
| engine 任务认领 / 门禁 / ADR | `engine/.agent/`（先读 `engine/AGENTS.md`） |
| dsh-runtime 开发约定 | `dsh-runtime/AGENTS.md` |
| 部署纪律细则 / 四步自检 | `deploy/README.md` |
| 历史问题（某症状以前发生过吗） | `LESSONS.md` |

## 6. 维护纪律

- 新增或变更入口命令、端口、SSOT 文件 → 同步 `docs/dev-prod-map.md`（U7）；
- 流程变了 → 改对应 playbook，并在 `.agent/status.md` 记一行；
- 本文件修改需人类批准（对齐 engine AGENTS.md §4 的宪法保护条款）。

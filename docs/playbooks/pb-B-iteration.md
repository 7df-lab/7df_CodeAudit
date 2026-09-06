# pb-B — 子项目迭代开发

> 适用：单个子仓的功能开发 / 缺陷修复 / 配置调整。开发发生在**子仓内**（U2），
> 伞仓层只做认领、对账与指针落账。

## 前置

1. `make status` 对账；确认目标子仓无他人未提交改动（有 → U3 只读不改，等其收尾）。
2. `bash .agent/session.sh claim <子仓名> "<任务一句话>"`。
3. `cd <子仓>`，**先读该仓入口文档**再动手：

| 子仓 | 入口（宪法/说明） | 门禁命令（事实源 dev-prod-map §2） |
|------|------------------|-----------------------------------|
| engine | `engine/AGENTS.md`（红线 R1-R10、任务认领、gate.sh） | `bash -c '. .toolchain/env.sh && bash .agent/verify.sh'`（伞仓快捷：`make test-engine`） |
| web | `web/README.md` | `npm test`（vitest）；类型门禁在 `npm run build`（`make test-web` / `make build-web`） |
| vscode-plugin | `vscode-plugin/README.md` | `npm test`（mocha，vscode 桩）；打包关卡 `npm run package` |
| manager | `manager/README.md` + `manager/deploy/README.md` | `python3 -m pytest tests/ -q`（26 条离线契约） |
| openshell-gateway | `openshell-gateway/README.md` | 无测试仓：改配置后 `./deploy.sh --check` + `./gateway_lifecycle.sh verify` |
| dsh-runtime | `dsh-runtime/AGENTS.md`（上游约定） | `pnpm test`（分层见其 docs/testing.md） |
| dsh-pentest-sse | `dsh-pentest-sse/README.md` | 无自动化测试：构建期断言 + `./deploy.sh` 冒烟（走 pb-C） |

## 步骤

1. **开发**：在子仓内完成改动；engine 仓还须遵守其工作循环（认领任务 → 五问 →
   实现 → 自测 → verify.sh → status.md 回写；未提交变更 >25 文件会被 G1 拦，R8 粒度）。
2. **过闸**：跑上表门禁，原始输出按各仓纪律留证（engine → 其 `.agent/evidence/`，本机）。
3. **提交推送**：子仓内标准 git 流程（engine 用其 R8 commit 格式 `[TPxx-Tyy] 动词+对象`；
   其余子仓沿用各自提交风格）。子仓 push 到各自 origin。
4. **联动判断**（决定要不要追加验证）：
   - 改了 **API/契约/部署形态**（engine proto、web nginx、manager 镜像、各 Dockerfile/compose）
     → 跑 [pb-A](pb-A-integration.md) 模拟栈回归——mock 单测对迁移类破坏零证明力（LESSONS #1）。
   - 改了 **dsh-runtime 源码或 dsh-pentest-sse 配方** → 走 [pb-C](pb-C-sandbox-image.md)。
   - 纯子仓内部、不影响集成面 → 子仓门禁绿即可。
5. **指针落账**：子仓已 push 后，回伞仓 `git add <子仓>` 并随本次交付 commit
   （或 `make pin MSG="..."` 留部署锚点）——指针必须指向**已推送**的子仓 HEAD。

## DoD

- 子仓门禁全绿（原始输出留证）；改动已 commit 且已 push；
- 联动验证按第 4 步执行或有明确"无需联动"的理由；
- 伞仓指针推进或明确留待 pin；账本回写。

## 失败出口

- 门禁失败：在子仓内修（小问题记该仓决策日志；engine 记 decisions.md ADR-1xx）；
  环境缺工具（如 engine 无 `.toolchain`）→ 账本记 blocked，禁止跳过门禁自称完成。
- 发现需要动**其他子仓**：先 release 当前理解偏差（任务边界变化），重新认领，
  按 U3 协同；跨多仓的集成性工作转 pb-A 编排。

## 收尾

```bash
bash .agent/session.sh release <子仓名>
# 账本记一行：时间 | B | <子仓名> | <做了什么+commit sha> | 门禁结果/联动验证结果
```

# docs/playbooks/ — 操作手册索引

> 每本手册 = 一类会话的**步骤级流程**：适用场景 / 前置 / 步骤（命令）/ DoD / 失败出口 / 收尾。
> 入口与红线见根目录 [AGENTS.md](../../AGENTS.md)；命令与口径的事实源是
> [docs/dev-prod-map.md](../dev-prod-map.md)——手册只做**编排与引用**，不复制事实（防漂移，LESSONS #2/#5）。

| 手册 | 模式 | 一句话 |
|------|------|--------|
| [pb-A-integration.md](pb-A-integration.md) | A 协同测试部署 | 同步全部子仓 → 起模拟栈 → e2e 七用例 → 失败路由 |
| [pb-B-iteration.md](pb-B-iteration.md) | B 子项目迭代 | 认领 → 进子仓开发 → 过子仓门禁 → 推送 → 判断联动验证 |
| [pb-C-sandbox-image.md](pb-C-sandbox-image.md) | C 沙箱镜像链路 | dsh-runtime(源码)↔dsh-pentest-sse(配方) 双仓联动重建镜像 |
| [pb-D-prod-deploy.md](pb-D-prod-deploy.md) | D 生产部署 | 只读检查 → 按拓扑部署 LXC 107 → 验证 → pin 锚点 |
| [pb-E-recovery.md](pb-E-recovery.md) | E 故障恢复 | 症状 → 处置表（同步拒绝/栈起不来/e2e 失败/漂移/端口/并行冲突） |

**编写与维护规则**：

1. 手册里的每条命令必须实测过才写入（LESSONS #5）；引用具体文件用仓库根相对路径。
2. 命令口径变更（改 Makefile、改 deploy 脚本、改端口）→ 同 commit 更新受影响手册与 dev-prod-map.md（AGENTS.md U7）。
3. 手册不含"为什么"（那在 LESSONS.md / deploy/README.md），只含"怎么做 + 做完判据"。
4. 新增手册须在 AGENTS.md §1 路由表挂号。

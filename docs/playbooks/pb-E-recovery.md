# pb-E — 故障恢复（症状 → 处置）

> 任何 playbook 失败后的第一跳转。先对症状，再动手；处置后回对应 playbook 续跑。
> 背后的"为什么"见 [LESSONS.md](../../LESSONS.md)（编号引用）与 [deploy/README.md](../../deploy/README.md)。

## 症状→处置表

| 症状 | 处置 |
|------|------|
| `make pull` 被拒（--ff-only） | 某子仓有未推提交。`make status` 看"领先"列 → 是本会话的就去 push；疑似并行会话的（U3）只读等待；确认废弃的人类裁决后才可 reset |
| 模拟栈起不来 / compose 报 parse 错误 | 先 `docker compose -f engine/docker-compose.yml -f deploy/docker-compose.sim.yml config >/dev/null` 验 parse（LESSONS #8，曾因 YAML 重复键炸过）；再 `make logs-sim <svc>` 定位；仍不明 → `make destroy-sim && make deploy-sim` 全新重置（数据卷清除） |
| gateway 健康等待超时（420s） | `make logs-sim gateway` 看拨号哪个下游失败 → 中间件（PG/Redis/MinIO/Kafka）没起就先看它们的健康检查门控 |
| e2e 用例失败 | 单用例重跑 `bash deploy/tests/run.sh <NN>` 缩小范围：03 失败→project/PG 链路；04 失败→`make logs-sim task` / `make logs-sim sast-adapter`（bandit 真扫链路）；05 失败→console 容器与 nginx 反代；06 失败→Kafka/Redis；07 看是"诚实失败"断言还是链路真断（manager 不可达属前者，通过是正确行为） |
| 某子仓单测绿但 e2e 断 | mock 在 HTTP 边界测不出真实链路断裂（LESSONS #1）——以 e2e 为准定位断点，转 pb-B 修复，修完必须回 pb-A 回归 |
| `sandbox-deploy.sh check` 报 codeaudit 漂移但刚部署过 | 排查顺序：①本地树是否又改了 ②**远端残留**（叠加同步从不删旧文件，LESSONS #7——换血流程在 deploy/README）③排除后重 deploy |
| compose 拒绝接管 / 容器名冲突 | 历史手工容器标签不符（LESSONS #6）：`docker compose build` 确认新镜像可建 → `docker rm -f <旧容器>` → 正规 deploy |
| 端口冲突 / 服务起不来报 bind | 查 [dev-prod-map §5](../dev-prod-map.md)：LXC 107 的 8080 归 openshell-gateway（CodeAudit 网关用 8090）；模拟栈固定 18080/18088；网段口径也在该表 |
| openshell-gateway 8081 health curl 不通 | 已知实测口径：发布但不可达（仅绑 loopback）。探测改用 TCP 8080 或 manager `GET /api/v1/gateway/health`——不是故障，别"修"它 |
| 缺 gitignored 文件（token/config/二进制） | 按 [dev-prod-map §4](../dev-prod-map.md) 重建表逐项处理（含 opengrep 的 sha256 校验方式与 manager token 的拉回命令） |
| 沙箱镜像构建失败 | [pb-C](pb-C-sandbox-image.md) 失败出口（stage1=sha256 不符重跑 fetch；pnpm 阶段=dsh-runtime 未 commit） |
| 并行会话互相覆写文件 | 立即停写；`git status` 核对归属；以时间上更后的人类指令为准，**禁止机械回滚**对方改动（LESSONS #9）；恢复后在账本记事件与裁决 |
| 我不确定某行为是不是 bug | 先查 LESSONS.md 是否已知问题；再查对应子仓 README/决策日志；仍不明 → 账本记 blocked + 疑点描述，提请人类 |

## 恢复后

- 处置成功 → 回原 playbook 从失败步骤续跑，不要从头重做已过的步骤；
- 涉及生产 / 删除数据 / 改密钥的处置 → 必须有人类指令（U5）；
- 每次真实故障与处置在账本记一行；新模式（表里没有的症状）→ 提请补进本表（同 commit 更新）。

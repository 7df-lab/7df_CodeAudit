# pb-D — 生产部署（LXC 107）

> **外向动作**（AGENTS.md U5）：须 ①有人类部署指令 ②只读 check 通过 ③四步自检
> （[deploy/README.md](../../deploy/README.md)「迁移/重构后自检清单」）齐备才执行。
> 部署纪律的事实源是 deploy/README.md 与 `deploy/prod/README.md`——本手册只编排顺序。

## 前置

1. 人类指令在案（账本记一行引用）。
2. `make status` + `make pull`：全部子仓干净且同步（生产从源码树构建，脏树不可复现）。
3. 只读全链检查（不改任何东西）：

```bash
bash deploy/sandbox-deploy.sh plan       # 5 项目拓扑/依赖/目标一览
bash deploy/sandbox-deploy.sh pull       # 全项目上游镜像预拉（多源兜底；all 也会自动跑）
bash deploy/sandbox-deploy.sh check      # 全项目漂移检查（gateway/manager/codeaudit + 镜像就位）
```

4. 解读 check：`in sync` 可直接部署；`drift` 正常（本地演进领先远端）——**部署即收敛**；
   但"漂移永不过"要怀疑远端残留（LESSONS #7，处置见 deploy/README）。
5. 涉及重构/迁移后首次部署：跑 deploy/README 四步自检（grep 旧标识 → plan+check →
   密钥交接 `manager/deploy/env` 在位 → compose 接管冲突确认）。

## 步骤

```bash
# 按拓扑序部署全部（openshell-gateway → openshell-manager → codeaudit → dsh-pentest-sse → web）
# 每项目 deploy 前自动跑镜像预拉（deploy/pull-images.sh 多源兜底）
bash deploy/sandbox-deploy.sh all

# 或单项（依赖未就绪会自动先部署依赖）：
bash deploy/sandbox-deploy.sh codeaudit deploy
```

codeaudit 单独入口：`bash deploy/prod/deploy.sh deploy`（tar 同步 → 远端 compose 构建
7 Go 服务+4 中间件 → 等 postgres → 建空库 → 等 health）。首次部署拉镜像+9 次构建耗时长。

## 验证

```bash
bash deploy/sandbox-deploy.sh status                  # 全项目状态
bash deploy/prod/deploy.sh check                      # codeaudit 源码树漂移（应 in sync）
curl -s http://gateway.internal:8090/health | head -1    # API 网关健康（8090！8080 归 openshell-gateway）
```

## DoD

- 四项目 `check`/`status` 全部就位，codeaudit 漂移已收敛（in sync）；
- `/health` 返回 ok（原始输出留证 `.agent/evidence/`）；
- 关键链路抽查：e2e 01/02（健康+认证）对生产端点手工过一遍，或说明为何跳过。

## 失败出口

- compose 拒绝接管 / 容器名冲突 → 曾被手工操作过的容器：先 `docker rm -f <name>` 再正规
  deploy（顺序与理由见 LESSONS #6；状态在卷与 .env 不丢）。
- 部署后健康不过 → `bash deploy/prod/deploy.sh logs 100` + pb-E；**回滚 = 重新部署上一
  pin 指向的源码树**（`git -C engine checkout <上一pin的sha>` 后重 deploy），密钥与数据卷不动。

## 收尾

```bash
make pin MSG="生产部署 <日期>: <变更摘要>"   # 部署版本集锚点（须人类确认后 push）
git push                                     # 伞仓指针落账
bash .agent/session.sh show                  # 确认无遗留占用
# 账本记一行：时间 | D | <项目/镜像tag> | 变更摘要+各子仓 sha | check/health 证据路径
```

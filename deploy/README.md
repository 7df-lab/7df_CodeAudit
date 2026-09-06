# deploy/ — 生产模拟环境（总体考虑）

> 2026-09-05 人类指令：总体项目与各子项目的生产部署统一考虑；**后续不允许在宿主机
> 进行开发测试**——一律使用 docker 模拟生产部署实况，并在模拟生产环境内尽可能
> 完成所有功能的测试。

## 1. 三层环境口径（同一 base compose，不同 overlay/env）

| 环境 | 位置 | 组成 | 用途 |
|------|------|------|------|
| 开发单元测试 | 仓库内（go test / vitest） | 无栈依赖，mock 在 HTTP 边界 | 逻辑回归 |
| **生产模拟（本目录）** | 任意 docker 宿主 | base compose + sim overlay + env.sim，project=codeaudit-sim | **集成/功能/部署验收**——宿主机裸栈直跑自此退役 |
| 生产（自部署） | 用户自己的 docker 服务器 | `bash deploy/production-deploy.sh`（base + prod overlay + production.env） | 第三方 clone 后一键部署 |
| 生产（本工作区） | LXC 107（伞仓 `deploy/prod/`） | base compose + prod overlay + env（同构） | 现役环境，操作入口 `deploy/prod/deploy.sh` 与 `sandbox-deploy.sh` |

三/四套环境**共用同一份服务定义事实源**（engine/docker-compose.yml，原 platform），差异全部收敛在
overlay + env——模拟与生产同构，"在模拟里通过"才对生产有证明力。

镜像可用性兜底（2026-09-05 实测沉淀）：目标 daemon 的 registry-mirrors 不可信（死源/白名单
拒 bitnami 等），`sandbox-deploy.sh pull` 与 `production-deploy.sh` 部署前经
`deploy/pull-images.sh` 多源预拉（幂等已有跳过 + 显式 mirror 拉取后 retag 回原名；
非 docker.io 镜像只直拉）。

## 2. 模拟栈拓扑

```
宿主发布端口（1xxxx 段）              codeaudit-sim-net (10.10.210.0/24，与生产 110 段隔离)
┌──────────────┐
│ console :18088 │─ nginx /v1 ──┐
└──────────────┘               ▼
                        ┌──────────────┐   gRPC(服务名:端口)
                        │ gateway :18080│──→ project:50052 / task:50054 / result:50058
└──────────────┘        │ (REST+WS)    │    sast-adapter:50051 / storage:50055 / dsh-runtime:50057
                        └──────┬───────┘
     ┌──────────┬──────────┬───┴────┬─────────┐
     │postgres  │ redis    │ minio  │ kafka   │   ← 基础设施容器（健康检查门控）
     │ 5432     │ 6379     │ 9000   │ 9092    │
     └──────────┴──────────┴────────┴─────────┘
外部依赖（不在栈内，env 注入）: openshell-manager（沙箱）、DSH 沙箱镜像、LLM 网络
```

与生产的差异仅为：独立 project 名/网段/端口段（同 daemon 可与生产共存）、console 纳入
栈内一并验证、gRPC 服务占位健康检查升级为真实 TCP 探针。

## 3. 使用

```bash
cd codeaudit-umbrella
cp deploy/env.sim.example deploy/env.sim   # 按需修改密钥/manager 地址
make deploy-sim                            # 构建+启动+等健康+PG 库表种子
make test-sim                              # e2e 功能测试套（07 个用例）
make logs-sim [service]                    # 排障
make down-sim                              # 停栈（数据卷保留）；destroy 彻底重置
```

也可直接 `deploy/sim.sh up|down|destroy|status|logs|wait|seed`。

### 3.1 生产态一键部署（用户自部署入口）

```bash
git clone --recurse-submodules <伞仓> && cd codeaudit-umbrella   # 或 clone 后 make update
bash deploy/production-deploy.sh deploy    # 预检→密钥→镜像→gateway→manager→engine→沙箱镜像→console
bash deploy/production-deploy.sh status | stop | down [-v]
```

- 参数 `deploy/production.env`（gitignored）首跑自动生成：密钥随机、宿主 IP 探测、
  端口/网段/Kafka 广播地址全部 env 化，改后重跑 deploy 即收敛。
- 部署前 opengrep 缺失时自动按 PROVENANCE.md 来源拉取（官方 release，sha256 复核；
  无 GitHub 出口按文件内指引手工 vendor）。
- 网关侧 JWT 签名密钥与 supervisor 镜像由 `gateway_lifecycle.sh ensure` 自举
  （2026-09-05 前 = 隐藏手工步骤，107 全量退役实测暴露后固化）。

## 4. 功能测试覆盖面（deploy/tests/run.sh）

| 用例 | 验证的真实链路 |
|------|----------------|
| 01 健康 | gateway /health；未认证 401 |
| 02 认证 | JWT 登录/错误口令拒绝/refresh 续签 |
| 03 项目 | 创建/列表/config 写读（PG+project-service） |
| 04 上传→SAST 全链（核心） | 压缩包直传 storage(MinIO) → task 按 upload_file_id 拉包解包 → bandit 真扫 → 发现落库 → 报告生成 |
| 05 控制台 | 容器内 nginx：SPA 首页/路由回退//v1 反代认证透传 |
| 06 通知 | 任务完成事件 → 通知中心可达非空（Kafka→Redis→notification） |
| 07 AI 链路 | 环境相关：manager+LLM 可达则全链 COMPLETED；不可达则断言**诚实失败**（终态+完整 error_message，不允许静默挂死） |
| 08 项目级上传→自动任务（GUI 用户路径回归） | 复刻 GUI 请求序列：上传→建项目→config 关联→空 config 任务→start——回归服务间地址接线（409 锚点）与任务源共享卷（空目录扫描锚点） |
| 09 可观测面 | 快照聚合含执行日志、通知非空——回归 AppendTaskLog 接线与 storage 存储档位 |

测试原则：只走 gateway/console 的 HTTP 面（黑盒，等价真实用户）；样本漏洞自带
（SQL 注入+硬编码凭据 Python 文件），不依赖外部仓库。

## 5. 边界与已知约束

- 本机（当前开发机）无 docker 引擎——模拟栈在 **docker 宿主**上拉起（CD 所在 LXC 107
  或任一装 docker 的机器）；`git clone` 伞仓后 `make deploy-sim` 即可。
- AI 全链依赖栈外组件（openshell-manager、dsh-pentest-sse 镜像、LLM 出网）：
  有则测全链，无则测诚实降级——两者都是被测行为。
- 长任务（模式C/D/E）在同一套链路上，只是多走 dsh-runtime；纳入日常回归会拉长耗时，
  按 10号测试计划口径作为里程碑级用例手动触发。
- `deploy/env.sim` 为真实密钥文件，**不入 git**（.gitignore 已覆盖）。

## 重构补记（2026-09-05 repo 体系迁移）

- platform→`engine/`、console→`web/`（engine/web 为 codeaudit 组二级仓）；
  sim 栈的构建上下文与脚本路径已同步（sim.sh PLATFORM_DIR=../engine；sim compose context=../web）。
- **生产部署事实源迁入本目录**：`prod/`（原 CD/codeaudit 的 overlay+deploy.sh+env.template）
  与 `sandbox-deploy.sh`+`sandbox-deploy.toml`（原 CD 根的沙箱镜像部署编排）。
  CD 已于同日析出归档，LXC 107 生产操作自此以 `deploy/prod/deploy.sh` 与
  `./sandbox-deploy.sh`（清单 dir 相对伞仓根）为准；manager token 单一事实源 =
  `../manager/deploy/env`。
- 沙箱三件套成为二级仓：`../manager/`（管理服务源码+deploy/）、`../openshell-gateway/`、
  `../dsh-pentest-sse/`（含 sandbox-artifacts/ 构建输入子目录）、`../dsh-runtime/`（DSH 运行时源码）。

## 迁移/重构后自检清单（部署链验收）

任何一次仓迁移、目录调整、脚本或清单改名之后，部署链必须过一遍。
2026-09-05 迁移审计的沉淀：问题收敛为**三类根因**，本清单逐类设防——

| 根因类 | 本次实例 | 防线 |
|---|---|---|
| 字符串引用不随 git 移动更新：脚本默认值行、注释旧仓名、清单 dir | deploy.sh 默认 SRC 指向已归档路径；分发器默认 toml 文件名不存在（说明该链自迁移后从未运行）；清单四个 dir 全失效 | 第 1、2 步 |
| gitignored 单一事实源不跟仓走：fresh clone 即缺，而脚本硬依赖 | `manager/deploy/env`（token）丢失；opengrep 二进制缺失致 sast-adapter 镜像必构建失败 | 第 3 步 |
| 部署配方与源码不在同一变更原子，落后即坏 | Dockerfile.manager 停在 stdlib 1.0.0 而源码已 FastAPI 化（当时分属两仓所致） | 源码与 deploy/ 同居一仓 + 纪律：改运行时形态（依赖/入口/端口）的提交必须同步同目录部署配方 |

具体动作：

1. **grep 旧标识**：旧仓名、旧路径、旧文件名在 `.sh`/`.toml`/`.yml`/README
   的残留。高发位点：脚本默认值行（`SRC=`/`TOML=`/`dir=`）、文件头注释、
   清单条目——"默认值是旧世界"往往说明该链路自迁移后从未被运行。
2. **只读验证**：伞仓根 `deploy/sandbox-deploy.sh plan` +
   `deploy/sandbox-deploy.sh check`。check 会把命令分发到各项目 deploy.sh
   对 LXC 只读比对，不改任何东西——"文件存在"不等于"链路能跑"。
3. **密钥文件交接**：gitignore 的本地单一事实源（`../manager/deploy/env`）
   不跟仓走，fresh clone 即缺。丢失时从现役实例拉回重建：
   ```bash
   pct exec 107 -- cat /root/os-deploy/deploy/openshell-manager/.env \
       > manager/deploy/env && chmod 600 manager/deploy/env
   ```
4. **compose 接管注意**：若现役容器曾被绕过 deploy.sh 手工操作（compose
   标签不一致），`up` 报容器名冲突——确认新镜像已构建后
   `docker rm -f <name>` 再重跑 deploy（中断秒级，token 由 `.env` 保持，
   脚本收尾自动验 healthz + 网关可达性）。

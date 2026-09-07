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
bash deploy/production-deploy.sh configure  # 可选：先交互确认参数，只落盘不部署
bash deploy/production-deploy.sh deploy     # 交互确认→预检→密钥→镜像→gateway→manager→engine→沙箱镜像→console
bash deploy/production-deploy.sh status | stop | down [-v]
```

- **交互确认**（2026-09-07 增）：终端运行 `deploy`/`configure` 时与部署人员核对个性化关键信息——
  访问入口地址（多网卡可选，**仅用于完成横幅/汇总显示，内部接线不依赖**）、**沙箱服务路由域**
  （2026-09-08 增，缺省 `sandbox.codeaudit.internal`，纯字符串路由键全程不解析，gateway
  server_sans 与 sse 冒烟断言同源联动）、异己端口冲突（给空闲建议并联动改配落盘）、引擎网段
  与宿主重叠（给跳位建议）；最后汇总参数确认才开工。
  `--yes`（或 `PROD_DEPLOY_ASSUME_YES=1`）或 stdin 非终端（CI/管道）自动跳过问答按现值执行。
  LLM provider 不在部署期配置——部署成功后按完成横幅指引自行注册管理。
- **沙箱构建素材免下载**（2026-09-08 增）：`pdtools/nuclei-templates/agent-tools` 等 gitignored
  大件部署前先 `fetch.sh --verify` 离线复核（sbom sha256 逐项），**在位即零下载**；缺失/漂移
  才全量拉取（需网络出口，离线主机按 sandbox-artifacts/README 手工补件）；`check` 命令只报
  在位性不下载。opengrep 同理（缺失自动按 PROVENANCE.md 来源拉取）。
- 参数 `deploy/production.env`（gitignored）首跑自动生成：密钥随机、宿主 IP 探测、
  端口/网段/Kafka 广播地址全部 env 化，改后重跑 deploy 即收敛。**联动键自动重算**：
  manager→网关端点与沙箱拨号随 `OPENSHELL_PORT`、console `/v1` 反代随 `CODEAUDIT_HOST_GATEWAY`
  ——改一个端口键全链跟随（dind 实测的反代 404 手误由此根治）；**manager 是内部面非交互面**：
  `OPENSHELL_MANAGER_URL` 恒为内部常量 `host.docker.internal:18800`（hosts 别名解析，零 DNS、
  零用户输入，2026-09-08 起与访问 IP 解耦）；沙箱镜像 tag 经 `DSH_IMAGE` 可调（缺省 `:latest`）。
- **解析机制（全新安装零 DNS 依赖，2026-09-08 代码实证）**：用户不需要提供任何 DNS——
  同 compose 网络内服务互访（`project:50052`、`kafka:9092` 等）走 **docker 内嵌 DNS**（127.0.0.11，
  dockerd 自带，与用户环境无关）；跨栈各跳走 `host.docker.internal` hosts 别名（`extra_hosts: host-gateway`
  注入，非 DNS）或裸 IP（manager）；沙箱服务路由域（缺省 `*.openshell.internal`，字符串）**从不被解析**——
  engine dsh-runtime 的 routeReq 等价 `curl --resolve`：拨 `host.docker.internal:8080`、路由域只进
  `Host` 头由网关匹配（`sandbox.go` routeReq；bridge.mjs 零出站连接；dsh-runtime 全服务零
  `net.LookupHost`）。用户以 **IP+端口**直接访问 console（`http://<IP>:8088`）与网关 API
  （`http://<IP>:8090`，部署完成横幅打印实际地址）。唯一出网依赖 = 部署后自注册的
  LLM provider endpoint（可达性由用户环境保证，可 IP 可域名）。
- 部署前 opengrep 缺失时自动按 PROVENANCE.md 来源拉取（官方 release，sha256 复核；
  无 GitHub 出口按文件内指引手工 vendor）。
- 网关侧 JWT 签名密钥与 supervisor 镜像由 `gateway_lifecycle.sh ensure` 自举
  （2026-09-05 前 = 隐藏手工步骤，107 全量退役实测暴露后固化）。

### 3.2 Windows 环境（双壳：Git Bash 优先 → WSL2 兜底，2026-09-08）

**架构一句话**：Docker Desktop 装在 Windows 侧（Windows 应用，自带隐藏的 docker-desktop
WSL 发行版承载 Linux 内核与 daemon）；容器永远跑在 Docker Desktop 的引擎里。引导脚本
装的 Ubuntu（若走到 WSL 壳）只是 bash 部署脚本的运行环境——**选壳只影响 bash 在哪里跑，
daemon 只有一份**。产品镜像全为 Linux 镜像，Linux 容器在 Windows 上必然经 WSL2/Hyper-V
内核，没有"绕开 WSL 的纯 Windows Docker"选项。

部署逻辑复用 §3.1 同一个 bash 入口（不维护第二套部署事实源），PowerShell 只做环境引导：

```powershell
# Git Bash 优先：已装 Git for Windows 即用它当壳（免管理员，仓库在 NTFS）
powershell -ExecutionPolicy Bypass -File deploy\windows\bootstrap.ps1 -RepoUrl <伞仓地址>
# 没有 Git Bash 时自动兜底 WSL2 路径（需管理员：启用 WSL → 装 Ubuntu → 仓库进 ext4）
# 动作透传：-Action configure|deploy|status|stop|down；局域网暴露（可选）：
powershell -ExecutionPolicy Bypass -File deploy\windows\expose-lan.ps1            # 8088/8090 → LAN
```

- **Git Bash 壳**：克隆统一 `-c core.autocrlf=false` + CRLF 校验（残留则
  checkout-index 强制重检出 LF）；bash 入口内置 MSYS 工具面回退——`ss`→`netstat`、
  `ip`/`hostname -I`→`ipconfig`（port_listening/access_ip/host_ip_candidates 三处）、
  `python3`→`python`（bootstrap 缺 Python 时 winget 装，仅有 python 时自动建
  `~/bin/python3` 垫片）；unzip 缺失仅告警（只影响素材全量拉取分支，在位即零下载不受影响）。
- **WSL 壳**：仓库克隆在 WSL 的 Linux 文件系统（`$HOME`）内——`/mnt/c` 又慢又可能因
  CRLF 损坏 shell 脚本；bash 入口在 WSL 下自动把访问面缺省地址切为 `localhost`
  （`is_wsl` 探测 /proc/version）并在汇总/横幅提示 portproxy。
- 访问口径（两壳一致）：Windows 本机浏览器 `http://localhost:<口>`；局域网其它设备用
  `expose-lan.ps1`（netsh portproxy + 防火墙）或 Win11 22H2+ 的 WSL 镜像网络模式。
- 状态（U8 如实记）：bootstrap/expose-lan 为静态编写，尚未在真实 Windows 上实测
  （本机无 pwsh/Windows）；bash 入口的可移植性回退分支经本机可测面验证
  （netstat 正则实测命中、configure/deploy 全链回归绿），MSYS/WSL 分支待实机。

## 4. 功能测试覆盖面（deploy/tests/run.sh）

| 用例 | 验证的真实链路 |
|------|----------------|
| 01 健康 | gateway /health；未认证 401 |
| 02 认证 | JWT 登录/错误口令拒绝/refresh 续签 |
| 03 项目 | 创建/列表/config 写读（PG+project-service） |
| 04 上传→SAST 全链（核心） | 压缩包直传 storage(MinIO) → task 按 upload_file_id 拉包解包 → bandit 真扫 → 发现落库 → 报告生成 |
| 05 控制台 | 容器内 nginx：SPA 首页/路由回退//v1 反代认证透传 |
| 06 通知 | 任务完成事件 → 通知中心可达非空（Kafka→Redis→notification） |
| 07 AI 链路 | 上传型项目（2026-09-07 修正：原挂假仓库项目恒 DEAD，全链从未被行使）：manager+沙箱+LLM 可达 → COMPLETED 且 AI 交互日志非空；沙箱不可达 → COMPLETED 走 RuleScan 兜底（发现标 NEEDS_MANUAL，设计行为）；崩坏 → 诚实失败（终态+完整 error_message，不允许静默挂死） |
| 08 项目级上传→自动任务（GUI 用户路径回归） | 复刻 GUI 请求序列：上传→建项目→config 关联→空 config 任务→start——回归服务间地址接线（409 锚点）与任务源共享卷（空目录扫描锚点） |
| 09 可观测面 | 快照聚合含执行日志、通知非空——回归 AppendTaskLog 接线与 storage 存储档位 |

测试原则：只走 gateway/console 的 HTTP 面（黑盒，等价真实用户）；样本漏洞自带
（SQL 注入+硬编码凭据 Python 文件），不依赖外部仓库。

仓库拉取（git clone）场景的可复现夹具：`bash deploy/tests/git_fixture_107.sh up`
在 107 起匿名 git-daemon（:19418，transient）+ sample-sast.git，项目
repo_url 填 `git://10.10.210.1:19418/sample-sast.git`（分支 main）；跑完 `down`
收敛，不在共享宿主留常驻进程。GUI 交互层黑盒门禁单入口
`python3 deploy/tests/ui_check.py`（本机 playwright：默认全流程闭环——UI 创建流+
运行期流式判据+终态页签/风险详情链路点选/报告/在线查看/通知，截图存证；
`--task <RUNNING>` 挂载模式只验流式）。

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

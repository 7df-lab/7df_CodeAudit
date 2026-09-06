# CodeAudit Umbrella（一级仓库）

CodeAudit 平台的**一级仓库（umbrella）**：二级子仓在 GitLab 上保持独立（`codeaudit` 组），
本仓以 submodule 跟随各仓主干（活工作区模式），实现"总览可见、对账可查、部署有单一入口"。

- GitLab：`https://gitlab.local/codeaudit/umbrella`（规范域名 `gitlab.local`）
- 模式：**本目录是并行开发工作区**——每个子目录都是完整独立仓库，跟踪各自 origin 主干，
  日常开发/提交/推送直接在子目录内进行（可并行开多个 Zcode 会话）；
  伞仓 `make pull` 跟随各仓主干，`make pin` 仅在需要记录"部署/对账版本集"时使用。
- 历史沿革：2026-09-05 完成 repo 体系重构（见下方平台地图）；更早的散落检出
  （codeaudit/codeaudit-console/openshell-manager/CD 等）已归档至 `/root/deepseek-harness/archive/`。

## 平台地图（二级仓 × 角色 × 消费关系）

| 本目录 | GitLab 项目 | 角色 | 被谁消费 |
|--------|-------------|------|----------|
| `engine/` | `codeaudit/engine` | 平台引擎（纯后端）：设计文档(01-14)/proto 契约/7 微服务（gateway/project/task/result/storage/sast-adapter/dsh-runtime）/tests；前端已迁出至 web | web、vscode-plugin 的 API 面；dsh-runtime 驱动沙箱 |
| `web/` | `codeaudit/web` | 前端控制台（React+antd，容器化） | 浏览器；经 nginx 反代 engine 网关 /v1 |
| `vscode-plugin/` | `codeaudit/vscode-plugin` | VS Code 插件（审阅/补丁应用/任务面板） | engine 网关 REST/WS |
| `manager/` | `codeaudit/manager` | OpenShell 沙箱管理服务（FastAPI 源码 + deploy/ 部署事实源） | dsh-runtime 经 manager API 拉起沙箱；gateway 流量经它路由 |
| `openshell-gateway/` | `codeaudit/openshell-gateway` | 沙箱管控网关（gRPC 8080/health 8081 的部署事实源：Dockerfile/gateway.toml/compose） | manager 调度沙箱执行 |
| `dsh-pentest-sse/` | `codeaudit/dsh-pentest-sse` | 沙箱桥接镜像仓：配方（Dockerfile/bridge/settings/plugins）+ 构建输入（sandbox-artifacts/ 子目录，fetch 脚本+SBOM+sha256+PROVENANCE；vendored 大件 gitignore 可复现拉取） | 沙箱镜像 dsh-pentest-sse:1.2.x |
| `dsh-runtime/` | `codeaudit/dsh-runtime` | DSH 运行时源码（github 上游 `deepseek-ai/deepseek-harness` fork + 本地补丁） | dsh-pentest-sse 镜像构建（git archive 取源）；沙箱内运行 |

**DSH 命名关系**（历史澄清）：

```
github.com/deepseek-ai/deepseek-harness          上游源码仓
        │ fork + 本地补丁（feb3854610 SSE 断流归因）
        ▼
codeaudit/dsh-runtime                            运行时源码（曾用名 admins/deepseek-harness-sse）
        │ dsh-pentest-sse/Dockerfile：git archive 取源 + sandbox-artifacts 工具层 + bridge
        ▼
docker 镜像 dsh-pentest-sse:1.2.x                打包产物（manager 按需拉起沙箱运行）
```

- `admins/dsh-harness`（pentest 侧仓）：DSH **插件源码**托管，与运行时仓是"插件 ↔ 宿主"关系。
- `four-direction-pentest-engine`：渗透测试引擎，**不属于本项目**。

## 常用操作

```bash
make status          # 各子仓 分支/落后/领先/未提交/未推送 一览（每天开工先看）
make pull            # 跟随各仓主干：全部子仓 pull --ff-only（日常同步入口）
make update          # 新 clone 本伞仓后的引导：init + 按主干检出（含递归）
make hooks           # 新 clone 后必跑一次：安装 pre-commit 敏感信息门禁

# 开发：直接进子目录（每个都是完整仓库，可各自开 Zcode 会话并行）
cd engine && bash -c '. .toolchain/env.sh && make test'   # 例：平台
cd web && npm test                                        # 例：前端

# 子仓改完推送（子目录内标准 git 流程），可选 make pin 留部署锚点
```

- `.gitmodules` 声明各子模块跟踪分支（main/master）+ `ignore = dirty`：子仓日常改动不污染
  伞仓 status；HEAD 前移会显示，提示可 `make pin` 留锚。
- `make pull` 用 `--ff-only`：本地子仓有未推提交时拒绝快进——刻意保护，先推子仓再同步。
- **部署测试一律走容器**：`make deploy-sim`（生产模拟栈）+ `make test-sim`（e2e），
  宿主机裸栈直跑已退役（deploy/README.md）。
- **仓迁移/重构后必过自检清单**：grep 旧标识 → `deploy/sandbox-deploy.sh plan`+`check`
  只读全链 → 密钥文件交接/重建（deploy/README.md「迁移/重构后自检清单」，含三类根因与
  本次实例）——"文件存在"不等于"链路能跑"。
- **普适问题档案**：[LESSONS.md](LESSONS.md)——2026-09-05 重构与生产收敛中九类可复发
  问题的立档（现象/根因/教训/防线），改部署、写配置、跑迁移前先翻。
- **AI 会话入口**：[AGENTS.md](AGENTS.md)（伞仓宪法：开工三问/模式路由 A-E/红线 U1-U8/
  并行会话认领协议）——AI 在伞仓根开工即读；操作手册在 [docs/playbooks/](docs/playbooks/)
  （协同测试部署/子仓迭代/沙箱镜像/生产部署/故障恢复，含命令与 DoD）；跨会话对账看
  `.agent/status.md`，并行互斥用 `bash .agent/session.sh {claim|release|show}`。
- **开发/测试/生产全景**：[docs/dev-prod-map.md](docs/dev-prod-map.md)——逐仓的开发、测试、
  生产流程/配置/文件速查，含端口口径总表、gitignored 单一事实源清单与重建方式、
  刻意保护项（改动入口命令/配置文件/端口口径时须同步该文档）。

## deploy/（生产模拟 + 生产 overlay + 沙箱镜像部署）

```
deploy/
├── sim.sh / docker-compose.sim.yml / env.sim.example   # 生产模拟栈（codeaudit-sim，1xxxx 端口/独立网段）
├── tests/run.sh                                        # e2e 功能测试套（7 用例）
├── prod/                                               # 生产部署事实源（原 CD/codeaudit：overlay+deploy.sh+env）
├── sandbox-deploy.sh + sandbox-deploy.toml             # 沙箱镜像构建部署编排（原 CD 根）
└── README.md                                           # 三层环境口径与使用说明
```

## 敏感信息门禁（push 到 GitHub 前必经）

本仓库同步到 GitHub（公开面），内网真实地址一律不得入库。占位符映射与扫描集中在
[sanitize.sh](sanitize.sh)（映射表在脚本头部 `MAP`，新真实值出现就加一行）：

```bash
make sanitize         # fix：工作区真实内网地址/域名 → 占位符（幂等）
make sanitize-check   # check：硬违例(退出码1) + 人工确认清单
make hooks            # 一次性：安装 pre-commit 钩子（fix→重暂存→check 自动化）
```

- pre-commit 钩子启用后，每次 `git commit` 自动执行清除与门禁；硬违例会阻断提交，
  确认无害可 `git commit --no-verify` 跳过（慎用）。
- check 的人工确认项列出未入映射表的私网 IP / 个人邮箱 / 私钥块 / sk- 形态密钥，
  测试夹具里的通用值（如 192.168.1.1）属正常，过目确认即可。
- 已入库的真实值（历史提交）不在此脚本能力范围，勿把历史直接 force push 外泄面。

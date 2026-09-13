<div align="center">

# CodeAudit

**SAST × 多智能体大模型 —— 融合式智能代码安全审计平台**

上传一个代码包，让 10 类传统 SAST 引擎与五角色 AI 审计流水线并行工作，
融合去重后输出一份**可解释、可修复、可回滚**的审计报告。

[![Go](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![React](https://img.shields.io/badge/React-18-61DAFB?logo=react&logoColor=white)](https://react.dev)
[![gRPC](https://img.shields.io/badge/gRPC-microservices-244C5C?logo=gRPC)](https://grpc.io)
[![Docker](https://img.shields.io/badge/deploy-Docker%20Compose-2496ED?logo=docker&logoColor=white)](https://docs.docker.com/compose/)
[![VS Code](https://img.shields.io/badge/IDE-VS%20Code%20Extension-007ACC?logo=visualstudiocode&logoColor=white)](https://code.visualstudio.com)

[核心特性](#-核心特性) · [系统架构](#%EF%B8%8F-系统架构) · [快速开始](#-快速开始) · [文档导航](#-文档导航) · [Roadmap](#%EF%B8%8F-roadmap)

</div>

---

## 为什么做 CodeAudit

| 痛点 | CodeAudit 的回答 |
|------|------------------|
| **SAST 误报噪音大**：规则引擎只认模式不认语义，海量告警靠人肉过滤 | 五种审计模式任选；跨工具去重 + 融合引擎（对齐 → 聚类 → 冲突消解 → AI 验证），输出**单一高信噪比清单** |
| **AI 结论不可信、不敢用**：模型报的漏洞既无法验证也难以度量 | 每条结论带 **Source→Sink 证据链**，经 Quality Validator 交叉验证；模式 E 支持 SAST 与 AI 结果**同维度对比**，可信度可度量 |
| **审计结果难落地**：报告躺在 PDF 里，修复靠人工搬运 | 发现直接携带**机器可应用补丁**，VS Code 内一键应用、落盘前自动 checkpoint、随时按发现回滚 |

一次典型审计：上传代码包 → 选择扫描模式 → 任务编排（状态机 + Saga 补偿）→ SAST 与 AI 流水线并行 → 融合去重 → 审计报告 + 通知 → IDE 内修复闭环。

## ✨ 核心特性

### 五种审计模式

| 模式 | 流程 | 适用场景 |
|------|------|----------|
| A 纯 SAST | 多工具并行扫描 → 跨工具去重合并 → 报告 | 快速扫描、修复验证、无 AI 依赖 |
| B 纯 AI | CPG 构建 → 沙箱内五 Agent 语义审计 → 报告 | 逻辑缺陷挖掘 |
| **C SAST + AI 融合（默认）** | SAST 工具组 ∥ AI 审计并行 → 融合去重输出单一清单 | 深度审计、CI/CD、定时批量 |
| D AI 增强 SAST | SAST 多工具扫描 → 逐条沙箱 AI 复核 → 汇总融合 | SAST 结果逐条精判、可信度优先 |
| E SAST + AI 对比 | 两者独立完成 → 三分桶（纯 SAST / 纯 AI / 重合）同维度对比 | 能力评估、工具选型、合规留痕 |

### 五角色 AI 审计流水线

| 角色 | 职责 |
|------|------|
| **Code Analyst** | 代码结构与依赖分析（CPG / AST） |
| **Vuln Detector** | 污点追踪 + LLM 语义分析定位漏洞 |
| **Severity Assessor** | CVSS 评分与影响面评估 |
| **Fix Advisor** | 生成修复方案与机器可应用补丁 |
| **Quality Validator** | 交叉验证与误报过滤（元审计闭环） |

### 多引擎融合，统一结果模型

- 内置 **10 类 SAST 适配器**（CodeQL / Semgrep / Bandit / SpotBugs / ESLint / PMD / Brakeman / Trivy / Flake8 / 自定义），全部解析为统一的 `UnifiedFinding` 模型；
- 融合引擎完成对齐、聚类、冲突消解与 AI 验证；审计、对比、审核**三类报告**经 Kafka 异步生成。

### 可解释，可修复，可回滚

- **证据链**：AI 结论还原 Source→Sink 逐步路径，控制台内逐跳点选查看，AI 交互全过程时间线留痕；
- **修复**：Fix Advisor 输出统一 diff 补丁（支持多文件增删移），VS Code 内一键应用；低风险修复可批量裁决；
- **回滚**：落盘前自动 checkpoint，支持按发现回滚与批量回滚，修复前后 diff 一目了然。

### 沙箱化的 AI 执行

AI 代码分析运行在**一次性 MicroVM 沙箱**中（按需拉起、任务结束即回收），引擎与沙箱之间只有一条受控南向通道。LLM 经统一推理路由接入（OpenAI 兼容协议），默认 MiMo-v2.5-pro，可切换 DeepSeek-Coder-V2、Qwen2.5-Coder 或任意兼容模型服务。

### Web 控制台 + IDE 插件双入口

- **Web 控制台**（React 18 + antd 5）：项目、任务、发现、融合/审核/对比视图、报告、通知中心、用户管理；流水线日志与 AI 交互时间线全程可视；
- **VS Code 插件**（19 个命令）：编辑器内一键扫描工作区、实时任务进度、漏洞行内标注 + 侧栏树、AI 交互上下文面板、修复与回滚；WebSocket 推送 + 轮询兜底，断线自动重连续订。

### 语言支持

- **第一梯队（完整支持）**：Python、JavaScript、TypeScript、Java、Go；
- **第二梯队（基础支持）**：C、C++、Rust、Ruby、PHP。

## 🖼️ 界面预览

以下为 Web 控制台实际运行截图——一次对真实开源 C++ 图像解码库（RawSpeed）的模式 C 融合扫描任务。

| 总览：任务状态与最近任务 | 任务详情：AI 交互时间线 |
|---|---|
| ![总览](docs/screenshots/dashboard.png) | ![任务详情](docs/screenshots/task-detail.png) |

| 发现列表：严重级 / 来源 / 位置与人工复核 | 融合视图：双引擎发现归并 |
|---|---|
| ![发现列表](docs/screenshots/findings.png) | ![融合视图](docs/screenshots/fusion-view.png) |

## 🏗️ 系统架构

```
     浏览器（Web 控制台）            VS Code 插件
            └──────── REST + WebSocket（/v1，JWT）────────┐
                                                         ▼
   ┌─────────────────────────────────────────────────────────────┐
   │                  engine（Go 微服务 × 7）                     │
   │                                                             │
   │   gateway ── 认证 · 路由 · 限流（对外唯一 API 入口）          │
   │      ├── project     项目 / 用户管理                         │
   │      ├── task        任务编排（状态机 + Saga）                │
   │      ├── result      审计结果 / 报告生成                     │
   │      ├── sast-adapter 10×SAST 适配 → 融合 / 对比引擎          │
   │      ├── storage     文件 / 消息 / 通知                      │
   │      └── dsh-runtime 五角色 Agent 编排                        │
   │                                                             │
   │   数据层：PostgreSQL · Redis · MinIO · Kafka                 │
   └──────────────────────────┬──────────────────────────────────┘
                              │ 沙箱唯一南向通道（受控 + Bearer）
                              ▼
        openshell-manager ──gRPC── openshell-gateway
                              │
                              ▼
        沙箱容器（MicroVM，按需拉起）：五角色 DSH Agent 流水线
```

| 类别 | 选型 |
|------|------|
| 后端 | Go 1.22 · gRPC · 7 微服务 · Kafka 异步事件 · JWT 网关 |
| 前端 | React 18 · TypeScript 5（strict）· antd 5 · Vite · nginx 容器化 |
| IDE | VS Code Extension（TypeScript） |
| AI | DSH 多智能体运行时 · OpenShell MicroVM 沙箱 · OpenAI 兼容推理路由 |
| 代码分析 | Joern（CPG）· Tree-sitter（AST）· 10×SAST 适配器 |
| 数据层 | PostgreSQL · Redis · MinIO · Kafka |
| 部署 | Docker Compose 一键部署（Kubernetes + Helm 规划中） |

## 📦 仓库结构

本仓库为总览仓，七个组件目录各司其职：

| 目录 | 说明 |
|------|------|
| [`engine/`](engine/) | 平台引擎：7 个 Go 微服务、proto 数据契约（SSOT）、14 册设计文档 |
| [`web/`](web/) | Web 控制台（React + antd，多阶段构建 nginx 容器） |
| [`vscode-plugin/`](vscode-plugin/) | VS Code 插件：扫描、结果展示、修复补丁应用与回滚 |
| [`manager/`](manager/) | 沙箱管理服务（FastAPI，纯管道不持业务状态） |
| [`openshell-gateway/`](openshell-gateway/) | 沙箱管控网关（按需拉起与调度沙箱） |
| [`dsh-pentest-sse/`](dsh-pentest-sse/) | 沙箱镜像配方（可复现构建：SBOM + sha256 + 溯源清单） |
| [`dsh-runtime/`](dsh-runtime/) | DSH 多智能体运行时（上游 fork + 加固补丁，849 个 spec） |

## 🚀 快速开始

### 方式一：一键自部署（任意 Docker 宿主）

```bash
git clone --recurse-submodules https://github.com/7df-lab/7df_CodeAudit.git
cd 7df_CodeAudit
bash deploy/production-deploy.sh deploy
```

脚本自动完成：环境预检 → 生成随机密钥 → 镜像多源预拉 → gateway → manager → engine → 沙箱镜像 → 控制台。
完成后访问：控制台 `http://<服务器IP>:8088`，网关 API `http://<服务器IP>:8090`（端口可在生成的 `deploy/production.env` 中调整）。

### 方式二：模拟栈体验 + 黑盒验收

```bash
git clone --recurse-submodules https://github.com/7df-lab/7df_CodeAudit.git
cd 7df_CodeAudit
cp deploy/env.sim.example deploy/env.sim
make deploy-sim        # 构建 + 启动 + 健康等待 + 库表种子
make test-sim          # 黑盒 e2e：9 个用例只走 HTTP 面
```

控制台 <http://localhost:18088>（模拟栈种子账号 `admin` / `admin`，仅用于体验环境），
网关 API <http://localhost:18080>。GUI 全流程约 3 分钟：登录 → 新建项目 → 上传代码包（≤25MB）→ 启动任务 → 查看发现与报告。

### 方式三：本地开发

```bash
# 后端引擎（Go ≥ 1.22；7 服务逐 module 构建）
cd engine && make build && make test          # Go 单测 + proto 契约测试（需 python3 + pytest）

# 前端控制台（Node ≥ 20）
cd web && npm ci
CODEAUDIT_GATEWAY_URL=http://localhost:8080 npm run dev   # /v1 代理到引擎网关（含 WS）

# VS Code 插件
cd vscode-plugin && npm ci && npm run compile  # VS Code 中 F5 启动扩展开发宿主
```

### 关于 AI 全链

完整 AI 审计需要三个外部组件：**沙箱管理服务（manager）**、**沙箱镜像**与**可出网的 LLM 推理服务**（OpenAI 兼容，在网关侧配置 provider 即可）。
未配置时平台**诚实降级**：任务在 AI 阶段走内置规则兜底（RuleScan），相关发现标注 `NEEDS_MANUAL`，任务终态与错误信息完整可查——绝不静默挂死；SAST 链路不受影响，两者都是被 e2e 明确测试的行为。

## 🧪 测试与质量门禁

| 模块 | 门禁 |
|------|------|
| engine | SSOT 红线检查 + 7 服务逐 module Go 单测 + pytest + proto 契约测试 |
| web | Vitest 组件/集成测试 + `tsc -b` 严格类型门禁 |
| vscode-plugin | Mocha 单测（补丁解析/应用为大头）+ VSIX 构建产物校验 |
| manager | 离线契约测试（真 HTTP 层 + 假 SDK 注入） |
| dsh-runtime | 分层 849 个 spec + 逐文件 100% 覆盖率门 + 快照测试 |
| 全栈 | 黑盒 e2e 9 用例（上传 → SAST 全链 → 报告 → 通知 → 控制台 → AI 链路/诚实降级） |

## 📚 文档导航

| 内容 | 入口 |
|------|------|
| 总体架构 / 八层设计 / 五模式 | [engine/01_总体架构设计.md](engine/01_总体架构设计.md) |
| 数据契约唯一事实源 | [engine/codeaudit_common.proto](engine/codeaudit_common.proto) |
| 接口规范 | [engine/03_接口规范.md](engine/03_接口规范.md) |
| 工作流 / 状态机 / Saga | [engine/04_工作流设计.md](engine/04_工作流设计.md) |
| 沙箱安全模型 | [engine/06_OpenShell集成设计.md](engine/06_OpenShell集成设计.md) |
| 性能与 SLO 基线 | [engine/07_非功能指标基线.md](engine/07_非功能指标基线.md) |
| 测试计划 | [engine/10_测试计划.md](engine/10_测试计划.md) |
| 部署与运维 | [deploy/README.md](deploy/README.md) |
| 开发 / 测试 / 生产全景速查 | [docs/dev-prod-map.md](docs/dev-prod-map.md) |

## 🗺️ Roadmap

- [ ] Kubernetes + Helm 交付形态
- [ ] IntelliJ 平台插件、CLI 与 CI Webhook 入口
- [ ] 第二梯队语言（C/C++/Rust/Ruby/PHP）支持完善
- [ ] 增量扫描落地（变更文件级复扫）

## 🤝 参与贡献

欢迎 Issue 与 PR。本仓库同时是 AI 协作工作区：AI 会话开工请先读 [AGENTS.md](AGENTS.md)（红线与并行认领协议），历史问题档案见 [LESSONS.md](LESSONS.md)，步骤级操作手册见 [docs/playbooks/](docs/playbooks/)。

提交均经 pre-commit 敏感信息门禁（`make hooks` 安装，`make sanitize-check` 手动触发）——内网地址与密钥不入库。

## 📄 License

各组件目录当前尚未附带开源许可证，正式对外发布与二次分发前请先确认许可条款。

---

<details>
<summary><b>仓库维护（内部协作速查）</b></summary>

伞仓 = 并行开发工作区：每个子目录都是完整独立仓库，日常开发直接在子目录进行。

```bash
make status                        # 各子仓 分支/领先落后/未提交 一览（开工先看）
make pull                          # 全部子仓 pull --ff-only（日常同步入口）
make update                        # 新 clone 引导：submodule init + 检出
make pin MSG="..."                 # 可选：记录部署/对账版本锚点
make hooks                         # 安装 pre-commit 敏感信息门禁（新 clone 必跑）
make sanitize                      # 工作区内网地址 → 占位符（幂等）
make sanitize-check                # 提交前门禁：硬违例阻断 + 人工确认清单
make deploy-sim / test-sim         # 模拟栈部署 / e2e
make down-sim / destroy-sim / logs-sim   # 停栈 / 彻底重置 / 排障
```

- 部署纪律与四步自检：[deploy/README.md](deploy/README.md)；
- 测试部署一律走容器，宿主机裸栈已退役；
- 端口/SSOT/入口命令口径以 [docs/dev-prod-map.md](docs/dev-prod-map.md) 为准。

</details>

<div align="center">

如果 CodeAudit 对你有帮助，欢迎点一个 Star ⭐

</div>

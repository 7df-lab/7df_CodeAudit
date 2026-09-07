# CodeAudit

**SAST × AI 智能体的代码审计平台**——规则引擎的确定性与大模型分析的纵深理解运行在同一平台：
从代码接入、双引擎扫描、发现裁决到补丁修复、报告输出的完整审计闭环。

- **一句话**：上传代码，同时获得 SAST 的规则级确定发现与 AI 的污点链级深度分析，在 Web 控制台
  或 VS Code 里完成裁决与一键修复。
- **一图**：

```
 浏览器 ──► web 控制台 ──/v1 反代──► engine 网关 ◄── REST + WS ── VS Code 插件
                                      (REST+WS,对外唯一入口,JWT)
                                        │
                 ┌──────────────────────┼─────────────────────┐
                 ▼                      ▼                     ▼
           gRPC 微服务 × 5        中间件 × 4             dsh-runtime(沙箱通道)
        project / task / result   PostgreSQL · Redis         │ HTTP/JSON + Bearer
        storage / sast-adapter    MinIO · Kafka              ▼
                                                     manager(沙箱管理服务)
                                                          │ gRPC
                                                          ▼
                                                 openshell-gateway(DooD)
                                                          │ 按需拉起
                                                          ▼
                                                 沙箱容器(AI 智能体 + 安全工具)
                                                          └─ 唯一出网 → 用户注册的 LLM 服务
```

## 核心特性

### 双引擎扫描，五种模式

- 规则侧：容器镜像内置 bandit 与 opengrep（Semgrep 兼容引擎）；适配器框架已实现
  bandit / Semgrep 系 / CodeQL / ESLint / SpotBugs / 通用 JSON 六类适配器，接入即扫。
- AI 侧：沙箱内智能体对代码做深度分析，产出带 Source→Sink 污点链与机器可应用补丁
  （apply_patch 语法）的结构化结论。
- 五种扫描模式自由组合（见下表），缺省推荐的融合模式一次任务同时收获两引擎的互补发现。

| 模式 | 名称 | 说明 |
|------|------|------|
| A | 纯 SAST | 只跑规则引擎，输出确定、速度最快 |
| B | 纯 AI | 只跑沙箱智能体，深度分析与修复建议 |
| C | SAST+AI 融合（**缺省推荐**） | 双引擎发现融合去重，配套融合视图综合裁决 |
| D | AI 增强 SAST | 规则发现交 AI 逐条深化分析与补全 |
| E | SAST+AI 对比 | 双引擎结果三分桶对比（仅 SAST / 仅 AI / 双方一致）+ 指标脚注 |

### AI 全程沙箱化，代码不出栈

- AI 智能体运行在管控网关按需拉起的隔离容器中；沙箱预置 sqlmap、testssl、nuclei 模板、
  Playwright 等安全工具供智能体按需调用。
- 上传代码经网关零落盘直转对象存储（MinIO），任务源只在栈内共享卷流转。
- 运行期唯一出网方向是用户自行注册的 LLM 服务——代码与密钥都不经过任何第三方。

### 过程可观测，失败诚实

- 任务详情页 WebSocket 实时推帧，断线自动回退轮询续订（游标不重发）；执行日志与
  AI 交互时间线全程可见。
- 沙箱不可达或 LLM 故障时明确降级：兜底走规则扫描并标注"待人工"，或完整报错落终态——
  绝不静默吞错，绝不产出误导性的"零发现"白审。

### 发现裁决（triage）工作台

- 发现按严重级与 AI 置信度分级；AI 结论的 Source→Sink 链路逐跳还原为可点选跳转，
  点击即定位代码行。
- 人工裁决（结论 + 理由）回写平台与 AI 结论同档留存；融合/对比视图辅助横向判断。

### 编辑器内修复闭环（VS Code 插件）

- 编辑器内一键扫描工作区：打包上传、建任务、实时跟踪，结果进 Problems 面板与侧栏树。
- AI 机器补丁一键应用：内容锚定不信任声明行号（四级容差，模糊锚定透明标注），任一块
  锚定失败即整体拒绝——绝不部分应用、绝不静默错切。
- 落盘前自动 checkpoint：按发现回滚、回滚最近一次批量修复；低风险修复（低严重级 +
  高置信度 + 带机器补丁）可多选批量应用，由人触发，插件绝不自动改盘。

### 一键部署，零 DNS 依赖

- 任意 Docker 主机一条命令拉起全栈：密钥首跑自动随机生成，端口/网段交互确认（可 `--yes`
  跳过），联动配置自动重算——改一个端口，全链跟随。
- 全新安装不需要任何 DNS：服务互访走容器内嵌 DNS 与 hosts 别名，用户以 IP+端口直接
  访问控制台与 API；沙箱路由域是纯字符串路由键，全程不发生解析。
- 沙箱构建素材按 SBOM+sha256 清单获取：在位即离线复用（`--verify` 复核），缺失才拉取，
  构建可复现。
- Windows 同一入口：引导脚本（Git Bash 优先，无则 WSL2 + Docker Desktop 兜底）后复用
  同一套 bash 部署命令，不维护第二套部署事实源。

### 企业级底座

- JWT 认证（access/refresh 双令牌、401 单飞刷新）与分链限流；API 网关是后端唯一对外入口。
- PostgreSQL / Redis / MinIO / Kafka 中间件自携带；任务完成等事件经消息队列进通知中心
  （控制台未读角标）。
- 报告在线查看、下载与失败重生成；管理面统一治理用户与 LLM provider/路由（admin 门禁）。

## 端到端工作流

1. **接入代码**：控制台或 VS Code 插件上传 zip/tar.gz 压缩包（≤25MB），或登记 git 仓库
   地址由平台拉取。
2. **发起扫描**：五种模式任选，SAST 工具手动多选或按项目语言自动选择；创建即启动，
   全程支持暂停/恢复/取消。
3. **实时观测**：任务详情双栏——AI 交互时间线常驻左侧，阶段进度、执行日志、发现列表
   随任务演进实时刷新。
4. **裁决发现**：发现详情即 triage 工作台——代码上下文全文、Source→Sink 链路点选定位、
   AI 分析与修复建议；模式 C/E 另有融合/对比视图。
5. **应用修复**：VS Code 插件一键应用机器补丁（锚定校验、checkpoint、可回滚），
   或在控制台查阅补丁建议后人工处置。
6. **输出报告**：报告中心在线查看/下载（JSON），失败可重新生成；完成通知自动送达
   通知中心。

## 仓库结构

伞仓（umbrella）以 submodule 编排七个独立子仓，外加统一部署编排 `deploy/`：

| 目录 | 角色 | 技术栈 |
|------|------|--------|
| `engine/` | 平台引擎：API 网关 + 6 个 gRPC 微服务（project / task / result / storage / sast-adapter / dsh-runtime），proto 契约与全套设计文档 | Go |
| `web/` | Web 控制台（SPA + `/v1` 反代，浏览器只与同源通信） | React 18 · TypeScript · antd 5 · Vite |
| `vscode-plugin/` | VS Code 插件（扫描 / 结果展示 / 修复 / 回滚） | TypeScript |
| `manager/` | 沙箱管理服务（纯管道，不持业务状态，不挂 docker.sock） | Python · FastAPI |
| `openshell-gateway/` | 沙箱管控网关部署事实源（gRPC，DooD 按需拉起沙箱） | 配置仓 |
| `dsh-pentest-sse/` | 沙箱镜像配方（两阶段 Dockerfile + SBOM 可复现构建输入） | Docker |
| `dsh-runtime/` | 沙箱内智能体运行时（上游 [deepseek-harness](https://github.com/deepseek-ai/deepseek-harness) fork + 补丁） | Node · TypeScript |
| `deploy/` | 一键部署与生产编排（`production-deploy.sh` 等） | bash |

## 快速开始

### 环境要求

- Linux：任意装有 Docker 的服务器（物理机/虚拟机/云主机均可）。
- Windows：Git Bash（随 Git for Windows 安装），或 WSL2 + Docker Desktop（引导脚本
  自动分诊，实验性）。

### 部署（Linux）

```bash
git clone --recurse-submodules <仓库地址> && cd codeaudit-umbrella
bash deploy/production-deploy.sh configure   # 可选：交互确认访问 IP/端口/网段，只落盘不部署
bash deploy/production-deploy.sh deploy      # 预检→密钥→镜像→五段式部署，幂等可重跑
bash deploy/production-deploy.sh status      # 栈状态；另有 stop / down [-v]
```

- 非交互环境（CI/管道）加 `--yes`（或环境变量 `PROD_DEPLOY_ASSUME_YES=1`）按现值执行。
- 部署完成横幅会打印实际访问地址，缺省：

| 入口 | 地址（缺省端口） |
|------|------|
| Web 控制台 | `http://<宿主IP>:8088` |
| 平台 API（REST + WS） | `http://<宿主IP>:8090` |

- 缺省账号 `admin / admin`：**仅用于首次登录，登录后请立即修改口令**。
- LLM 接入：AI 扫描依赖你自己的大模型服务。部署完成后以管理员身份经平台管理 API
  （`/v1/inference`，admin 门禁）注册 provider 并设置模型路由——部署完成横幅附具体指引；
  密钥只存于你的部署内。

### 部署（Windows，实验性）

```powershell
powershell -ExecutionPolicy Bypass -File deploy\windows\bootstrap.ps1 -RepoUrl <仓库地址>
# 引导完成后自动进入同一 bash 部署入口（-Action 透传 configure/deploy/status/stop/down）
powershell -ExecutionPolicy Bypass -File deploy\windows\expose-lan.ps1   # 可选：向局域网开放访问
```

## 安全设计

| 面 | 机制 |
|----|------|
| AI 隔离 | 智能体只在按需拉起的沙箱容器内运行；沙箱内桥接组件零出站连接；运行期唯一出网 = 用户注册的 LLM endpoint |
| 代码边界 | 上传件经网关零落盘直转对象存储；任务源栈内共享卷流转，不出部署栈 |
| 认证限流 | JWT 登录/刷新/登出；免认证链与保护链分别限流，限流键绑定身份 |
| 上传约束 | 仅 zip/tar.gz，≤25MB，multipart 直传 |
| 补丁安全 | 服务端先做一道补丁规范化校验；插件侧工作区禁闭（拒绝绝对路径与 `..` 逃逸）+ 锚定失败整体拒绝 + 应用前 checkpoint 可回滚 |
| 密钥治理 | 一键部署首跑自动生成随机 JWT 密钥与内部服务 token，无缺省弱密钥旁门 |

## 质量工程

- **单仓门禁**：每个子仓自带交付门禁——引擎仓 11 项检查（SSOT 红线/单测/契约/守门/变异自检），
  前端仓类型门禁 + 静态守卫 + 变异检验，插件仓结构守卫（命令/配置/注册互为全集），沙箱镜像仓
  生命周期/线协议/静态一致性用例。
- **契约先行**：各仓维护外部接口/内部接口/数据流三件套文档，测试与文档锚点双向追溯；
  数据契约以 proto 为单一事实源。
- **端到端**：10 场景 e2e 套件（健康/认证/项目/上传→SAST 全链/控制台/通知/AI 链路/
  GUI 用户路径/可观测面/推理管理面），黑盒只走用户可见面，样本漏洞自带。
- **GUI 黑盒门禁**：真实浏览器驱动登录→上传→流式观测→裁决→报告→通知全流程闭环。
- **变异检验**：对历史缺陷注入等价变异，证明回归锁真的会红——"测试全绿"必须可信。

## 已知边界（如实声明）

| 边界 | 说明 |
|------|------|
| 压缩包上传 ≤25MB | 更大代码库请用 git 仓库地址方式接入 |
| 用户体系 V1 | 暂无自助注册与用户列表；管理员按 ID 查询与停/启用账号 |
| VS Code 插件 | 单根工作区（多根只认第一个）；AI 正文以纯文本流渲染 |
| Windows 部署 | 引导脚本为实验性路径，优先推荐 Linux 主机部署 |
| AI 外部依赖 | AI 链路需要可达的 LLM 服务；不可达时规则扫描兜底或明确失败，不静默 |

## 致谢

- 沙箱智能体运行时基于开源项目 [deepseek-harness](https://github.com/deepseek-ai/deepseek-harness)（fork + 补丁）。
- 沙箱管控使用 OpenShell 网关（`ghcr.io/nvidia/openshell`）。
- VS Code 插件的补丁解析器移植自 Cline PatchParser。
- SAST 能力使用 bandit 与 opengrep。

---

> **维护注记（伞仓内部，发布时删除本段）**：本文是伞仓对外 README 的定稿候选，
> 发布施工时按 [release-sanitize-map.md](release-sanitize-map.md) §5 步骤 2 整体替换
> 发布分支根 `README.md`（并删除本注记与指向内部文档的链接）。内容纪律：严禁写入
> 内网标识与内部地址——收口 grep 字典见 sanitize-map §1；对外事实必须与实际交付一致，
> 变更时同 commit 更新本文（U7 纪律）。

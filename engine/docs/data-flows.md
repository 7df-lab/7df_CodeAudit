# engine 数据流转与代码交互 — 接口之外的链路

> 事实源（以代码为准，逐项核对于 2026-09-07，基线 commit `5bc736fd`）。覆盖两份接口文档
> （docs/api-external.md、docs/api-internal.md）之外的全部端到端数据流：任务生命周期、
> 文件管道、沙箱执行、SAST 融合、报告/通知、存储布局、配置体系、后台任务。
> 端口总表由守门测试 `tests/test_guardrails.py` 与 `configs/codeaudit.yaml` 比对看护。

## 1. 任务全生命周期（task-service 为中枢）

```
CreateScanTask(幂等键=task_id) ──发──> Kafka task.created ──> storage(通知 created_by)
   │
StartTask ──解析源码来源(§1.1)──> RUNNING ──go runOrchestration──> 五模式编排(§1.2)
   │                                                        │
   │                                          成功: TaskContext+finalize+task.completed
   │                                          失败: FAILED→QUEUED(≤2次)→…→DEAD(仍发 completed)
   │                                          └─途中: compensateFindings 删已落盘 findings(S8)
   ▼
Kafka task.completed ──> result-service(自动 GenerateReport, 幂等键 kafka_<task>)
                    └──> storage-service(通知 COMPLETED/FAILED/DEAD→收件人 created_by)
```

### 1.1 源码来源解析链（ADR-209 顺序，低档不得覆盖高档）

1. 任务 `config.project_path` 直用；
2. 任务 `config.upload_file_id` → storage 拉包闭包；
3. 项目级 upload_file_id（GetProjectConfig 兜底，快照回写任务 config）；
4. 项目 repo_url → 编排协程 git clone（`--depth 1 --single-branch` → `<repos_dir>/<task_id>`）；
5. 全部缺失 → 明确 FAILED（不空跑）。

### 1.2 五模式编排（orchestrator.Execute 分派）

| 模式 | 步骤 |
|---|---|
| A SAST_ONLY | RunMultipleScans → FuseResults(AI=∅) → 报告 |
| B AI_ONLY | AnalyzeCode → RunAIAnalysis → 报告 |
| C PARALLEL（默认） | SAST ∥ AI 并行 → FuseResults → 报告 |
| D AI_ENHANCED_SAST | SAST → VerifySASTResults → BatchUpdateVerdict → Fuse → 报告 |
| E COMPARE | 并行后 CompareResults（不合并） |
| 旧 B/D（TRADITIONAL_FIRST/SAST_REVIEW） | 历史兼容路径 |

### 1.3 阶段上报与进度流

- 启动按模式预注册 stages（Metadata 就地初始化——ADR-212① nil map panic 教训）。
- 编排器事件 → stageRecorder 实时置 COMPLETED；外部经 ReportStageComplete/Failed（幂等三态）。
- 进度 = 完成阶段/总阶段；快照推送 = StreamTaskSnapshot（hub 订阅者 + proto.Equal 变化检测 +
  2s 兜底 ticker；终态帧 settled=true 关流）。日志环形缓存 500 条/任务，log_id 全局单调。

### 1.4 暂停/恢复与超时对账

- Pause：先 dsh PauseAnalysis（回合闸门先扣）再 RUNNING→PAUSED；Resume 反序。
  模型流不可中途暂停，回合边界生效（当前回合跑完才挂起）。
- 超时对账 reconciler：1h ticker 扫 RUNNING，30m 阈值；**活跃度 = updated_at ∪ AI 交互日志
  mtime**（ADR-196：成功结果落地前 3 秒曾因纯 updated_at 被误杀）——依赖"同宿主同 CWD 部署"
  假设直接读 dsh-runtime 的落盘文件。

## 2. 文件上传与任务源树管道

```
前端 ──multipart──> gateway(零落盘,25MB 白名单) ──64KiB 客户端流──> storage UploadFile
                                                                     │ MinIO uploads 桶
StartTask(config.upload_file_id) ──> task-service FetchUploadArchive:
  GetFileInfo(取扩展名) → DownloadFile 流 → scratch → 解包到 <repos_dir>/uploads-<task_id>/unpacked/
  → 删归档原件 → ResolveProjectRoot 剥壳(唯一子目录逐层降入,封顶3层)
```

- 解包防护：safeJoin 穿越/软链拒绝、200MB/3000 文件上限；解压失败 → 任务 FAILED
  （错误文案带"压缩包下载/解压失败"阶段语义）。
- **共享卷 `agent_repos` → `/data/repos`**：task-service（写：解包/clone）、gateway
  （读：source-file）、dsh-runtime（读：沙箱上传前）三方共用同一任务源树。
- **剥壳双实现**：task-service `archive.go ResolveProjectRoot` 与 gateway `sourcefile.go
  resolveProjectRoot` 是跨 Go module 复制件，语义必须同步（口径漂移会让 fixpatch 校验与
  source-file 解析再度根错位——gw-f6a3523）。
- source-file 读取链四流：①`repos_dir/<task_id>` ①b `repos_dir/uploads-<task_id>/unpacked`+剥壳
  ②上传链接文件 `.codeaudit-task-<task_id>`（CreateScanTask 后 gateway 写，仅当
  project_path 位于 uploads_dir 内）③project config project_path ④唯一内容回退（mtime 最新，
  覆盖无链接存量任务）。文件解析 exact > suffix > basename；2MiB/二进制/穿越/软链拒绝。

## 3. 扫描执行流（dsh-runtime ↔ openshell-manager 沙箱）

```
dsh-runtime ──HTTP/JSON──> openshell-manager(:18800, LXC107) ──> dsh-pentest-sse 沙箱容器
                                                                  └─ bridge.mjs(:8080)
                                                                       │ SSE /events 回推
                                                                       │ JSON-RPC 下行
                                                                       ▼
                                                              推理网关 inference.local(沙箱内 egress)
```

- **端点解析**：env `OPENSHELL_MANAGER_URL/TOKEN` > yaml `dsh_runtime.sandbox.manager_url/
  manager_token` > 共享 `../openshell-manager/config.json`（tokenFile）> 默认 127.0.0.1:18800。
- **沙箱生命周期**：沙箱名 `ca-<hex12>`（≤15 字符，网关路由限制）→ 创建（spec 含
  environment.DSH_TASK_ID/image/归属标签 openshell.io/managed-by=codeaudit-dsh-runtime）→
  wait-ready → exec 拉起 bridge（`DSH_MAX_TOKENS`=32768、`DSH_PERMISSION_MODE=danger-full-access`、
  `DEEPSEEK_BASE_URL=https://inference.local/v1`）→ ExposeService(bridge:8080) → 订阅 SSE。
  进程级 activeSandboxes 注册表：注册先于创建（防对账竞态）、创建失败必须注销（ADR-212⑪）、
  teardown 注销。
- **孤儿回收**：SandboxReconciler 启动 2min 后首轮、每 30min；判据=名字正则 `^(am|ca)-[hex12]`
  或归属标签，且不在活跃注册表（ADR-212⑫：标签键值曾混用致第二重圈定恒不命中）。
  `CODEAUDIT_SANDBOX_RECONCILE=off` 可关。
- **项目上传**：tar.gz（排除 .git/node_modules）→ manager files 端点（Content-Length 必带）→
  沙箱内 `/sandbox/project`；40MB 上限。
- **回合语义**：收敛只认主会话 idle（子任务 idle 不算，ADR-190）；瞬态断流续跑 ≤2 轮
  （ADR-192）；submit_findings 分批提交跨回合累积、按补丁体量分层（无 diff ≤4 条/批、含 diff
  ≤2 条/批、多文件大补丁 1 条/批，ADR-194/211）；空列表=合法零发现（干净审计不误判报废）。
- **沙箱服务路由**：沙箱内 bridge 经 `gateway_dial_addr`(gateway.internal:8080) 直拨 + Host 头
  路由（沙箱服务域无 DNS 通配）。
- **AI 交互日志**：人性化中文流 → 内存条目 + `.ai.log`；原始 SSE 帧 → 仅 `.sse.log`；回调接线
  必须先于 runner 构造（runner 拷贝 cfg，ADR-215 回归）；内存 LRU 双上限 64 条/256MB，单任务
  16MB 截断；淘汰后读路径落磁盘兜底。

## 4. SAST 工具集成与融合流（sast-adapter）

```
RunMultipleScans ──并行──> bandit/opengrep(opengrep 目录被 git 枚举吞时文件级分批回退)
   │ findings 必填校验 → 本地 store(fusion 同进程复用)
   ├─落盘──> result BatchCreateFindings(幂等键 <reqID>-multi)
   ▼
FuseResults 五阶段: ①FP过滤(ai_verdict=FP 且 conf>0.8) ②位置合并(file+start+end)
   ③去重对齐(组内共享 dedup_group,非 primary 移除) ④冲突解决(severity 采 SAST/verdict 采 AI)
   ⑤置信度融合(组内加权平均×(1+0.1×(n-1)) 封顶 1.0)
   ──白名单回写──> result BatchUpdateFindings(dedup_group/is_unique/matched_findings)
```

- conflict/confidence 两阶段成员索引**必须取全量 FilteredSAST+FilteredAI**（dedup 后
  FusedFindings 只剩 primary，AI 成员被移除——ADR-214：原实现索引建自 FusedFindings 致两阶段
  恒空转）。
- 融合失败 → buildFallbackResult 未融合降级（原始结果保留）；fusion 内部 panic → error
  （nil ctx 零值兜底，ADR-212③）。
- 工具执行映射在 yaml `sast_adapter.tools`（argv 占位符 {project}/{rules}；bandit 有
  python_module 兜底）。opengrep=semgrep 分叉替代（ADR-158），默认导出 dataflow_trace。

## 5. 结果、报告与通知流

- **findings 落盘**：三个写方汇聚 result-service——dsh-runtime（AI/沙箱/RuleScan 发现，键
  `<request_id>-store`）、sast-adapter（SAST 扫描，键 `<reqID>-multi/-<tool>`）、task 编排
  （BatchUpdateVerdict，键 `-verdict-<v>-<c>-`）。
- **报告双路径**：Kafka 主路径（task.completed → result 消费 → GenerateReport 键
  `kafka_<task_id>`）+ gRPC 降级路径（Kafka 不可达时 task 编排直调，失败不阻断任务）。
  报告归档 storage `reports/<report_id>.json` → MinIO reports 桶，URL `minio://reports/...`
  回写 PG；归档失败仅 WARN。导出 `exports/findings-<task_id>.json` 本地+storage 双落。
- **Kafka topic 真实拓扑**（5 topic 声明，3 个有生产者）：

| topic | 生产者 | 消费者 |
|---|---|---|
| task.created | task-service（消息带 event_type 头——ADR-212⑤：缺头=消费端全丢） | storage（通知） |
| task.completed | task-service（CompleteTask 链+DEAD 终态） | result（报告）+ storage（通知） |
| finding.verdict.updated | result-service（verdict 变化时） | storage（仅记录） |
| task.stage.completed | —（无生产者） | storage（仅记录） |
| finding.created | —（无生产者） | storage（高危通知映射，实际不触发） |

- 消费者 group：result=`result-service`、storage=`storage-service`（每 topic 独立 Reader，
  FirstOffset，指数退避 3s→60s）。`CODEAUDIT_KAFKA_OPTIONAL=1` 三处一致跳过消费。
- **通知映射**：task.created→TASK_CREATED；task.completed→COMPLETED/FAILED（按 payload.status）；
  收件人=created_by|user_id|updated_by 非空者，全空跳过不臆造。

## 6. 存储布局总表

### PostgreSQL（唯一持久化属 result-service；`CODEAUDIT_PG_DSN`，`CODEAUDIT_STORE=memory` 降级）

| 库 | 表 | 说明 |
|---|---|---|
| codeaudit_result | findings | UNIQUE(task_id,tool_name,rule_id,file_path,line_number) |
| codeaudit_result | finding_feedback | 反馈 |
| codeaudit_result | reports / report_templates | 报告+3 内置模板 |
| codeaudit_project / codeaudit_task | **无表** | project/task 均内存态（init-db.sql 只建库） |

### Redis（仅 storage-service；前缀 ca:，ADR-210 am:→ca:）

`ca:notif:<id>`=JSON（无 TTL）· `ca:notif:idx:<user_id>`=ZSET(score=UnixNano) ·
`ca:idem:<request_id>`=幂等体（TTL 24h）。project-service 幂等为内存 map。

### MinIO（仅 storage-service）

桶=默认桶 + reports/cpg/sast-raw/uploads（启动自动建）。键规范：数据 `files/<file_id>`（域桶
按 FilePath 前缀分派）；元数据 `meta/files/<file_id>`（恒默认桶）；上传原件 `uploads/up-<hex><ext>`；
报告 `reports/<report_id>.json`；导出 `exports/findings-<task_id>.json`。

### 本地文件系统（隐性跨服务契约，无 proto 承载）

| 路径 | 写方 | 读方 |
|---|---|---|
| `<repos_dir>/<task_id>/`（repo clone） | task | gateway(source-file)、dsh(沙箱上传前) |
| `<repos_dir>/uploads-<task_id>/unpacked/`（拉包解包+剥壳） | task | 同上 |
| `data/ai-interaction/<task>.ai.log/.sse.log` | dsh-runtime | gateway(ai-log 兜底)、**task reconciler（mtime 判活）** |
| `<project>/.codeaudit/cpg.json` | dsh-runtime(AnalyzeCode) | dsh-runtime(QueryCPG) |
| `.agent/evidence/exports/` | result(ExportFindings) | 运维 |

## 7. 配置体系与端口

- **唯一配置文件** `configs/codeaudit.yaml`（ADR-137）：全部可调值承载于此，代码无业务缺省，
  env `CODEAUDIT_*` 覆盖，缺键 fail-fast；密钥只记"读哪个 env"（ADR-115）。
- **加载机制**（libs/go-config）：`CODEAUDIT_CONFIG` 精确路径，否则 CWD 逐级向上找
  `configs/codeaudit.yaml`——**服务必须在仓库根或设 env 启动**。
- 各段消费方：ports.\*→全部服务；addresses.\*→gateway/task/dsh/sast 拨号；gateway.\*→网关；
  task.\*→task；result.\*→result（含 kafka/分页/导出）；sast_adapter.\*→sast（工具 argv）；
  dsh_runtime.\*→dsh（会话 TTL/超时/沙箱/交互日志目录）；fusion.\*→sast 融合参数；
  webhook.\*→配置承载（出站订阅现空）；auth.\*→project-service 注册策略。

### 端口总表

| 组件 | 端口 | 出处 |
|---|---|---|
| gateway（HTTP+WS） | 8080 | ports.gateway |
| sast-adapter（gRPC） | 50051 | ports.sast_adapter |
| project（gRPC） | 50052 | ports.project |
| task（gRPC） | 50054 | ports.task |
| storage（gRPC） | 50055 | ports.storage |
| dsh-runtime（gRPC） | 50057 | ports.dsh_runtime |
| result（gRPC） | 50058 | ports.result |
| PostgreSQL | 5432 | compose |
| Redis | 6379 | compose |
| MinIO API / Console | 9000 / 9001 | compose |
| Kafka PLAINTEXT / controller | 9092 / 9093 | compose（advertised 默认 gateway.internal） |
| openshell-manager | 18800 | dsh_runtime.sandbox.manager_url |
| 沙箱 bridge 服务路由 | 8080（gateway Host 头） | dsh_runtime.sandbox.gateway_dial_addr |

## 8. 后台任务 / 定时器 / 缓存

| 项 | 周期/上限 | 位置 |
|---|---|---|
| task 超时对账 | 1h ticker，30m 阈值 | task reconciler.go |
| task 快照流兜底 tick | 2s | task task_stream.go |
| 孤儿沙箱对账 | 2min 延迟首轮，30m 周期 | dsh sandbox/reconciler.go |
| dsh 会话 janitor | TTL 1800s/ratio 6=300s | dsh session/ |
| sharedAILogs LRU | 64 条 / 256MB / 单任务 16MB | dsh ai_interaction_log.go |
| gateway taskwatch | 合并窗 50ms / ping 20s / 轮询 250ms / 读闲 90s / 寿命 6h / AI 宽限 3s | gateway taskwatch.go |
| AI 流订阅等待 | 500ms | dsh dsh_agent.go |
| Kafka 消费退避 | storage 指数 3s→60s；result 固定 3s | 两 consumer |
| 优雅停机 | gateway Shutdown(15s)；task/result GracefulStop(10s)；sast/dsh/project/storage 无信号处理 | 各 main.go |

## 9. compose 拓扑与已知错位

- 依赖链：gateway→{project,task,storage}(started)；project→{postgres,redis}(healthy)；
  storage→{minio,kafka,redis}(healthy)；task→{result,dsh-runtime,sast-adapter}(started)；
  sast-adapter→result；result→{postgres,kafka}(healthy)；dsh-runtime 无应用依赖。
- 应用 healthcheck：仅 gateway 真实探活（wget /health）；其余占位恒过。
- **已知错位（以代码为准）**：engine 本仓 docker-compose.yml 给 storage 传
  `CODEAUDIT_MINIO_ENDPOINT/ACCESS_KEY/SECRET_KEY`，而代码读 `CODEAUDIT_S3_ENDPOINT/S3_BUCKET/
  S3_ACCESS_KEY/S3_SECRET_KEY/S3_SECURE` 且需 `CODEAUDIT_STORE=s3`——本仓 compose 直起时
  storage 实际跑 **memory 档**。生产/模拟栈由伞仓 deploy/ 的 compose 与 env 承担（另有
  `CODEAUDIT_STORE=s3` 注入，不受此错位影响）。project-service 的 `CODEAUDIT_DB_*` env 亦
  无代码消费（内存 store）。修复需同步伞仓 deploy/ 配方，未裁决前如实记录。

## 10. dsh-runtime 执行日志上报（dsh→task 尽力而为）

`emitTaskLog`：懒连接共享 gRPC conn 调 `TaskService.AppendTaskLog`（source=sandbox/dsh-runtime）；
失败吞错留 stdout——日志是观测辅助，不构成数据面（丢日志不影响任务结果）。

## 11. 推理 provider/路由管理流（ADR-217，/v1/inference/*）

```
浏览器(web) ──同源 /v1/inference/*──> gateway(requireAdmin)
   ──gRPC──> DSHRuntimeService 推理管理面 6 RPC
   ──HTTP/JSON+Bearer──> openshell-manager /api/v1/inference/{providers,route}
   ──gRPC──> OpenShell 网关（权威存储 gateway.db，凭据加密；网关按引用向沙箱注入）
```

- **纯管道链**：引擎零 provider 状态。workspace 由 dsh-runtime 从全局配置
  `dsh_runtime.sandbox.workspace` 注入（对外 REST 面不暴露 workspace 概念）。
- **凭据单向流**：credentials 仅存在于写请求（POST/PUT providers）经 manager →
  网关加密存储；一切读路径（list/get/route）不回流凭据（manager 按省略脱敏）。
- **生效路径不变**：provider/路由变更影响的是**下一个任务**的 AI 阶段——沙箱启动时
  网关按当前路由注入 `DEEPSEEK_BASE_URL=https://inference.local/v1` + 引用凭据，
  运行中任务不受影响。
- **连通性验证**：PUT /v1/inference/route（no_verify=false 缺省）→ 网关实测推理端点，
  回执 validation_performed/validated_endpoints 逐层透传至前端；验证失败按 manager
  错误原样透出（切换不生效，诚实失败）。

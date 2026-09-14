# LESSONS — 普适问题档案

> 2026-09-05 repo 体系重构与生产收敛中发现并修复的问题立档。"普适"= 剥离具体
> 文件名后仍会在同类工作中复发的模式。操作清单（怎么防）在
> [deploy/README.md「迁移/重构后自检清单」](deploy/README.md)，本文是问题本体
> （是什么、为什么）。每条含：现象 → 根因 → 普适教训 → 防线落点。

| # | 问题模式 | 修复锚点 | 防线 |
|---|---|---|---|
| 1 | 契约迁移后调用点残留，mock 测试测不出真实链路断裂 | engine `bf2d6570`(ADR-202)、`bd6affa3`(ADR-203，并行会话) | e2e 套 + fakeGateway 测试台 |
| 2 | 字符串引用不随 git 移动更新 | umbrella `34541c0`、manager `41fb1a3` | 自检清单第 1、2 步 |
| 3 | gitignored 单一事实源不跟仓走 | manager `26eff4a`；opengrep 恢复见 PROVENANCE.md | 自检清单第 3 步 |
| 4 | 部署配方落后源码一个大版本 | manager `41fb1a3`、dsh-pentest-sse `6841f06` | 配方与源码同原子变更纪律 |
| 5 | 文档与代码失真（未经实测的断言） | gateway `7326e27`、manager `41fb1a3` | README 验收=逐行核对代码+实测 |
| 6 | 手工运维残留使 compose 无法接管 | LXC 侧处置（2026-09-05 收敛） | 自检清单第 4 步 |
| 7 | 叠加式同步使部署目录单调膨胀 | LXC 侧换血（见下） | `check` 确定性哈希 |
| 8 | 结构化配置改动未经 parse 验证 | engine `a8a33c49` | 提交前 parse 一遍 |
| 9 | 多智能体会话并行写同一文件 | web 侧 ADR-202/203 期间互覆写，按裁决恢复 | 按文件归属分工 + 后发指令优先 |
| 10 | 测试架构盲区：部署接线缺陷对全部既有测试隐形 | 伞仓 235f1ce 前后一系列 GUI 实测修复（engine 8de1a4d9/c24fb917 等） | check-wiring 静态审计 + e2e 入口多样化（08/09）+ 真实栈必跑纪律 |
| 11 | 遇到新问题先发明新方案，不查往期实证；手工应急成功后不固化 | 智谱 provider `../` 绕过（往期 llm-config.json 原文在先，nginx shim 在后）；kafka 镜像逐个手试拉取 | 新问题先 grep 往期（ADR/账本/兄弟仓 runtime 配置）再动手；手工步骤成功即入工具链/文档 |
| 12 | 纸面修复/死代码：兜底机制不存在、接线缺失、索引恒空——组件写了但从未真正生效 | storage minio panic 宣称"gRPC recover 兜底"（grpc-go 无此机制）；engine session.StartJanitor 零调用方；fusion conflict/confidence 组员索引建在 dedup 后集合上（AI 成员恒查不到，阶段死代码）；对账器把标签 VALUE 当 KEY 查 | 评审必答"这段代码的调用方/命中路径在哪"；兜底机制必须有触发它的测试；假体测试要实现真实 RPC（既有 WS 假体未实现流式=流式路零覆盖三缺陷潜伏） |
| 13 | 客户端错误泄漏为上游失败口径；注释里的环境/库断言未实测 | manager 非法参数/spec 错误全报 502（上游按网关不可达重试降级）；gateway 注释"网关 4MiB 默认"实测为 1MiB（上传 >750KiB 恒败）；protobuf 新版 ParseError 已非 ValueError 子类 | 错误码当契约管（400/404/502 分类有测试）；注释中环境数值/库行为断言一律实测后落笔（ADR-212/213/214/215） |
| 14 | 重构迁移改了布局/契约，依赖旧布局的旁路读路径不进回归视野 | gw-f6a3523 三连实证（source-file 四流全落空/解包不剥壳/流式中间态无断言+sim-sync 漏 web） | 改布局时 grep 旧布局常量盘点全部读方；读路径测试数据用当前生产布局构造；断言覆盖被修缺陷的中间态；部署产物与验证产物同源 |
| 15 | 长时脚本经管道取尾，被后台监听子进程钉死不退出 | gateway tests/run.sh 监听器 accept 无超时持有 stdout（伞仓侧跑门禁踩雷两次） | 验证调用落盘（`> file 2>&1`）不套管道；读工具脚本先查后台驻留子进程（监听器/watcher） |
| 16 | compose 生命周期操作与部署落点的项目名错位，静默无效果 | production-deploy.sh down 对 manager 报"完成"但容器仍在（deploy 落 .manager-stage、down 指 manager/deploy，项目名对不上） | up/down/stop/ps 必须复用同一 compose 包装（同 project+env-file）；deploy 与 down 目录不同构的组件是高危位 |

## 10. 测试架构盲区：部署接线缺陷对全部既有测试隐形

现象（2026-09-05 一键重建 + GUI 实测一次暴露四类）：①「新建项目→上传→自动任务」
start 必 409（task→project/storage 服务间地址 env 缺覆盖，容器内回落 localhost 拨
自身）；②SAST 0 发现/沙箱 32 字节空包（任务源目录无共享卷，各容器各扫各的空目录）；
③sim overlay 四服务重复键，整组 env 被 lenient 解析器吞掉；④prod overlay 缺
storage 生产档位（通知恒空、文件不落 MinIO）。单元/契约测试全绿、e2e 07 用例
"通过"——四类缺陷对它们全部隐形。

根因四层叠加：
1. **e2e 写而未跑**：tests/ 套件提交时标注"容器执行待 docker 宿主"，此后 sim 栈
   因 overlay 重复键根本起不来——套件实际是死代码，"e2e 通过"从未发生过；
2. **测试入口单一**：04 用例任务自带 `config.upload_file_id`，永远走"幸运路径"，
   不触项目 config 兜底链（task→project/storage RPC）；GUI 用户的真实入口序列
   （建项目→上传→自动任务）无任何自动化覆盖；
3. **接线无契约**：compose env 覆盖、共享卷挂载、存储档位是"部署层契约"，
   既无单测也不属于任何人的检查清单——yaml 缺省回落 localhost 的错误由降级链
   静默吞掉（fail-silent），与 ADR-137 的 fail-loud 精神相悖；
4. **断言粒度太粗**："到达 COMPLETED"对"SAST 实际扫到东西"零证明力——空目录
   扫描照样能走完状态机。

普适教训：**mock 与真实栈之间的地带（部署接线）必须有它自己的测试层**；测试
入口必须覆盖用户真实路径而非实现方便的路径；"从未真实跑过的测试"比没有测试
更危险（绿灯是假的）。防线落点：`deploy/check-wiring.py`（接线契约静态审计，
挂 sandbox-deploy check 与 production-deploy 预检）+ e2e 08/09（用户路径回归，
断言发现数与可观测面非空）+ 纪律：tests/ 套件入库的同一个变更原子必须含一次
真实执行的证据。

追加实录（2026-09-14 空卷完全重启，两例同族——健康面绿不等于供给链活）：
⑤ task/result 于 compose 拉起时即连业务库，而业务库由 `sim.sh up` 末尾的 seed
建；healthcheck 只验 PG 进程不验业务库，两服务 `restart=no` 崩后躺尸——增量
up（库已存在）永远暴露不了，destroy 后首次 up/全新安装必崩且 gateway 拨 task
报 DNS no such host。修复：seed 后自愈回拉（sim.sh，已跑者 no-op）。
⑥ storage 的 Kafka consumer 日志打印 "consumer started" 但消费组从未完成分区
分配（describe 空输出），通知恒空、e2e 09 通知锚挂——错误日志为零（ReadMessage
阻塞在组 join 内部，err 路径的退避重试根本不触发）。重启 storage 立即恢复并从头
回放。教训：**"started/healthy 日志"是自述不是证明**；消费侧的活性判据只能来自
broker 侧（消费组分区分配+LAG），engine 侧容错盲区（join 卡死无 err 不重试）
待子仓修复。

## 11. 先查往期实证，再发明新方案；手工应急成功即固化

现象（2026-09-05 同日两例）：①网关推理 provider 对智谱端点 404/400，在未查
往期的情况下搭了 nginx 路径改写 shim——而往期实证（four-direction-pentest-engine/
runtime/llm-config.json）早已给出正解：base_url 写 `https://open.bigmodel.cn/
v1/../../api/coding/paas/v4`，用 `/v1/../` 前缀抵消网关对 openai 型追加的
`/v1` 段（服务器端归一化归位），shim 属于多余基础设施；②bitnami/kafka 拉取
失败时逐个手动试镜像源，被叫停后才发现该把多源回退写成 pull-images.sh。

根因：外部服务适配类问题（URL 拼接/鉴权/网络出口）几乎必然在历史上出现过——
本工作区跨 7 仓 + 伞仓账本 + 兄弟仓 runtime 配置，实证散落面大；手工应急的
"成功知识"只存在于操作者脑中，不沉淀则下次复发。

普适教训：**动第一步之前先 grep 往期**（关键词：问题域名 + provider/配置名 +
报错原文片段；检索面=伞仓账本、engine decisions/status 归档、兄弟仓的
runtime/*.json 与 README），有实证照抄实证，无实证才设计新方案；手工应急一旦
验证成功，同一变更原子内把它固化成脚本/文档（本例最终形态：manual-test-guide
「推理 provider 配置方法」+ production-deploy 横幅指引）。

## 1. 契约迁移后调用点残留，mock 测试测不出断裂

现象：ADR-148→ADR-200 接口迁移后，`/tasks/new` 上传路径不再自动填入、
ProjectsPage 上传链路断裂——而前端单测全绿。根因：单测 mock 在 HTTP 边界，
只证明"代码符合 mock"，不证明"链路可用"；迁移时未 grep 全部调用点。
普适教训：**契约/接口迁移的验收 = 全量调用点 grep + 至少一条真实端到端流**；
mock 数字的绿灯对迁移类破坏零证明力。防线：deploy/tests e2e 套（黑盒走
HTTP 面）+ web fakeGateway 测试台（并行会话引入）。

## 2. 字符串引用不随 git 移动更新

现象（一次审计三连）：deploy.sh 默认 `SRC` 指向已归档路径；分发器默认读
`deploy.toml`（实际文件已改名 `sandbox-deploy.toml`——默认值是旧世界说明该链
自迁移后从未运行）；清单四个 `dir` 全部指向不存在目录。根因：移动/改名只改
了文件本体，字符串引用（脚本默认值行、注释、清单）不跟走，且无任何门禁。
普适教训：脚本默认值行是**最高发**位点——跑不到的默认值烂得最久；注释里的
旧仓名次之。防线：自检清单 grep 步 + 只读 `check` 全链。

## 3. gitignored 单一事实源不跟仓走

现象：`manager/deploy/env`（token 单一事实源）在重构迁移中未交接，fresh
clone 即缺，deploy 在 `pct push` 处必败；同类：`opengrep` 二进制（只入库
PROVENANCE+sha256，实物 gitignore）在新检出缺失，sast-adapter 镜像必构建
失败。根因：设计上"密钥/大件不入 git"与运维上"脚本硬依赖该文件"叠加，
缺一个"如何重建"的显式出口。普适教训：**每个 gitignore 的被依赖文件必须有
配套的重建命令写在仓里**（fail-loud 提示 + README/PROVENANCE），不是只写
"不入库"。防线：自检清单第 3 步（含两条重建命令）。

## 4. 部署配方落后源码一个大版本

现象：源码 ADR-174 已 FastAPI 化，`deploy/Dockerfile.manager` 仍停在
stdlib 1.0.0 配方——照此部署构建出起不来的容器；同构问题：compose 镜像
tag 落后（1.0.0 vs 现役 2.0.0）、dsh-pentest-sse 默认 `IMAGE` 落后两个版本。
根因：当时源码仓与部署 overlay 分属两仓，改源码的提交"够不着"另一仓的
配方。普适教训：**配方与源码必须同原子变更**——改运行时形态（依赖/入口/
端口/版本）的提交不得绕过同目录部署配方；结构性解药是同居一仓（本次重构
已完成），纪律防的是同居之后的手滑。防线：评审纪律 + `check` 漂移比对。

## 5. 文档与代码失真（未经实测的断言）

现象：gateway README 称"发布 8081 health"，实测容器内仅绑 loopback、健康
端点无桥接监听，宿主侧不可达；manager README API 表写 `{id}`，实际路由参数
是沙箱名（ADR-173 name→UUID 内解析），且漏掉 `GET providers/{name}` 路由。
根因：文档写于实现之前或转手转述，从未对照代码与运行时验证。普适教训：
**README 的验收标准 = 逐行核对代码 + 关键断言实测**（curl 一下就知道 8081
通不通）；"发布即可用"这类断言最容易想当然。防线：本文档模式——重写即实测
（gateway/manager README 均按此法重做过一轮）。

## 6. 手工运维残留使 compose 无法接管

现象：manager 容器被绕过 deploy.sh 手工 compose 起过（标签 workdir 不符）、
kafka/redis 是无标签 `docker run` 产物——正式 deploy 时容器名冲突或拒绝
接管，链路中断在最后一公里。根因：应急手工操作不留痕、不复位。普适教训：
**生产容器只允许经 deploy.sh 变更**；确需手工应急，事后必须补一次正规
deploy 收敛。处置顺序：先 `compose build` 确认新镜像可建 → `docker rm -f`
旧容器 → 正规 `up`（中断秒级，状态在卷与 .env 不丢）。

## 7. 叠加式同步使部署目录单调膨胀

现象：生产构建上下文 `/root/os-deploy/deploy/codeaudit` 积到 **18,467 个
文件**（本地仓 374）——历次 tar 叠加同步从不删除，旧版已删文件、evidence
截图、误产物全在里面，漂移检查永远报漂移。根因：同步语义是"覆盖+新增"，
无对账删除；且曾长期无人运行 `check`。普适教训：漂移检查"永不过"时优先
怀疑**远端残留**而非漏同步；构建上下文目录是纯派生物（容器跑镜像、状态在
卷），可安全换血——备 `.env` → 目录改名退役 → 验证后删除 → 全新同步。
防线：`check` 的确定性哈希比对（本次正是它把问题逼出来的）。

## 8. 结构化配置改动未经 parse 验证

现象：engine compose 的 redis 服务带两个 `ports` 键（ADR-199 加 6379 发布
时旧同值块未删），compose 严格解析直接失败，生产重部署在 parse 阶段退出。
根因：YAML/TOML 是"看起来对"就能提交的语言，无编译器兜底；该缺陷随 ADR-199
入库多日，直到下一次真实部署才爆。普适教训：**结构化配置（YAML/TOML/JSON）
改动提交前必须 parse 一遍**（compose config / python yaml 查重键 / toml
解析），一行命令的成本低于一次部署失败。

## 9. 多智能体会话并行写同一文件

现象：本会话与并行会话在 ADR-202/203 期间对 ProjectsPage/TaskNewPage 及其
测试互相覆写，一度出现"我恢复的版本又被并行会话重写"的循环。根因：两个
会话各自持有完整工作副本，无文件锁；裁决靠人类后发指令。普适教训：**并行
会话按文件归属分工**（一方认领的文件另一方只审不改）；冲突已发生时以时间
上更后的人类指令为准，机械回滚对方改动会破坏已授权工作；提交前 `git status`
核对文件归属。防线：本档案 + 会话记忆（裁决先例）。

## 12. 纸面修复/死代码：兜底机制不存在、接线缺失、索引恒空

现象（2026-09-06 ADR-212/213/214 三批复查一次暴露六处）：①storage panic 注释宣称
"由 gRPC recover 语义兜底"——grpc-go 根本没有内建 recover，任一 handler panic 即
杀进程；②`session.Manager.StartJanitor`（ADR-134 的内存泄漏修复）零调用方——
修复从未接线，泄漏照旧；③fusion 冲突/置信度两阶段在 dedup 后的
`FusedFindings` 上建组员索引，AI 成员恒查不到——`Conflicts` 恒空、加权恒 1.0，
04 §3.3 的融合语义对合并组从未生效；④SandboxReconciler 归属标签把 VALUE 当
KEY 查，名字正则之外的第二重圈定从未命中。

根因：修复只交付了"组件"，没有交付"生效路径"——接线点、索引键、触发机制无人
验证；且测试假体没实现真实 RPC（gateway WS 用例的假体未实现
`StreamTaskSnapshot`，全部用例落入轮询路，流式三缺陷零覆盖长期潜伏）。

普适教训：**声称存在的机制必须实证被触发过一次**。写兜底先问"谁调它"；写索
引先问"这个集合在此时还含不含我要的东西"；测试假体实现到真实契约的深度，决定
了测试能看见什么。

防线落点：引擎 verify（G2 单测+G4 契约）+ 评审三问（调用方/触发路径/假体保真
度）；ADR-212① 的 grpcrecover 拦截器与 ADR-213② 的流式假体测试是本条的固化。

## 13. 客户端错误泄漏为上游失败口径；注释里的环境断言未实测

现象：①manager 把非法数值参数、坏 base64、未知 spec 字段一律报 502——上游把
502 当"网关不可达"做重试/降级，掩盖真实原因（客户端格式错误）；②gateway 上传
分块注释按"gRPC 默认 4MiB"设 2MiB 分块，实测网关收包上限是 1MiB——任何
>~750KiB 文件上传从来不可能成功，注释里的假设从未被实测。

根因：异常兜底一锅端（不分类），环境数值抄默认值不实测。

普适教训：**错误码是契约**（400=改请求重试、502=上游降级——语义污染直接改变
调用方行为）；**注释里的环境/库行为断言一律实测后落笔**（4MiB 是 gRPC 默认值
不等于网关的实际配置；protobuf 新版 ParseError 已不是 ValueError 子类，同类
"库行为随版本漂移"还有 examples）。

防线落点：每服务统一错误分类（ADR-212③ 的 400 映射 + 错误映射测试）；环境
约束类断言在 ADR 记录实测方法与数值（manager UPLOAD_CHUNK_BYTES 注释即样例）。

## 14. 重构迁移改了布局/契约，依赖旧布局的旁路读路径不进回归视野（gw-f6a3523 三连实证）

现象：①ADR-200/203/209 把上传流任务源从 gateway uploads_dir 迁到
`repos_dir/uploads-<task_id>/unpacked` 后，ADR-195 的 source-file 解析链四流
（repo 目录/链接文件/project_path/唯一内容回退）对该布局**全部落空**——发现详情
"源码全文不可用、Sink 链路不可用"，而端点单元测试全绿（测试造的数据还是旧布局）；
②解包不剥压缩包顶层壳目录（`<repo>-master/`），fixpatch 校验、沙箱模型视角、
source-file 三方根错位 → 7/7 补丁静默误杀 + 17 分钟 fixretry 白跑；③GUI 全流程
对实时性的断言只有"等待期执行日志有增长 + AI 日志终态非空"，AI 阶段中途增量到达
从未被断言——WS 30min 硬断×token 竞态的流式回归直通交付；④sim-sync 只同步
engine 不同步 web，console 容器永远用残留旧树重建——前端修复不进部署产物。

根因：重构只迁移了"写路径"（谁生产数据），没有盘点"读路径"（谁按旧布局消费
数据）；旁路读路径（源码回查/补丁校验/GUI 断言）各自有单元测试，测试数据沿用
旧布局，绿一片而链路已断。

普适教训：**改布局/契约时，盘点动作必须覆盖"全部按旧假设读盘的一方"**——grep
旧布局常量（uploads_dir/链接文件名/unpacked）逐个裁决；每个读路径的测试数据
必须用**当前生产布局**构造；前端验证断言必须覆盖被修缺陷的**中间态**（流式=
运行中采样增长），终态断言抓不住渐进类回归；**部署产物必须与验证产物同源**
（sim-sync 同步 web 与 engine 同批）。

防线落点：source-file ①b 流回归锁（uploads-unpacked 布局+剥壳）+
ResolveProjectRoot 单测（task/gateway 双份同语义）；e2e 用例04 增
source-file 200 冒烟；deploy/tests/gui_streaming_check.py（AI 阶段运行中
增量到达断言）；sim-sync.sh 纳入 web 同步。

## 15. 管道跑 openshell-gateway tests/run.sh 会挂死（监听器持管道写端）

症状：`bash tests/run.sh | tail` 永不返回；后台任务无输出、无退出，`tail` 独活。

根因：tests/run.sh 的 start_listener 用后台 python3 起 TCP 监听器供 /dev/tcp 探测，
`accept()` 无超时、deadline 在 accept 阻塞下永远检查不到——探测完成后监听器最长
滞留 300s 且持有 stdout；`| tail` 等"全部管道写端 EOF"，被滞留监听器钉死。
仓内正常用法（裸跑终端/pre-commit）不等待后台任务，从无此现象——**坑只在使用者
给套了管道时引爆**。

普适教训：**长时脚本的验证调用不要经管道取尾**——直接落盘（`> file 2>&1; echo $?`）
或裸跑；读别人的测试/工具脚本时，先看有没有后台驻留子进程（监听器/watcher），
它们会把"管道 EOF 语义"变成"最长寿命语义"。

防线落点：gateway tests/run.sh 的监听器可加 `settimeout`（上游代码，未改）；伞仓侧
跑该门禁一律落盘方式（本账本行即为凭证，2026-09-08 踩雷两次后固化）。

## 16. production-deploy.sh down/stop/status 对 manager 项目名错位（漏删运行容器）

症状：`production-deploy.sh down` 报"down（卷保留）完成"，但 `openshell-manager`
容器仍在运行占着 18800；下次部署 compose 撞容器名 `Conflict. The container name
"/openshell-manager" is already in use`。

根因：manager 的部署落点与下电落点不是同一 compose 项目——`deploy_manager` 装配到
`deploy/.manager-stage`（项目名归一为 manager-stage），而 `compose_manager`（被
status/stop/down 复用）指向 `manager/deploy`（项目名 deploy）。项目名对不上，
compose 对着空项目做 stop/down，静默无效果且退出码 0。engine/web/gateway 三家
部署目录=下电目录，无此病；**坑只在 deploy 与 down 目录不同构的组件上引爆**。

普适教训：**compose 生命周期操作（up/down/stop/ps）必须用与当初部署完全相同的
project 定位（目录+env-file）**——容器归属看 `docker inspect` 的
`com.docker.compose.project` 标签，不看脚本里"应该在哪"；写部署脚本时，deploy 与
down 若不能共享同一个 compose 包装函数，就必然漂移。

防线落点：production-deploy.sh `compose_manager` 已改指运行态 stage 目录并对未
部署态静默跳过（2026-09-08，107 实测 down 幂等空跑 exit=0）。附：107 上沙箱网关
是 sim 与生产共用的（项目 openshell-gateway，checkout 在 /root/ca-umbrella）——
在 107 跑 `production-deploy.sh down` 会连网关一起下电，sim 沙箱链随断，须重跑
`gateway_lifecycle.sh ensure`（8080/8081）复位。

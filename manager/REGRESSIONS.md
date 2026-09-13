# REGRESSIONS.md — 缺陷档案与防回归索引

> 本档案是 manager 防回归机制的索引层：**每条已修复缺陷一行，绑定具名锁定测试**。
> 门禁 `make verify` 会校验本档案引用的测试真实存在（档案不腐烂）。

## 修复流程纪律（修 bug 必走，违反 = 交付无效）

1. **先写失败测试**：任何缺陷修复前，先在 `tests/` 写出能复现缺陷的用例
   （跑红），证明测试能发现问题；
2. **修复转绿**：修复后门禁 `make verify` 全绿；
3. **同 commit 记档**：在本档案追加一行（编号/日期/症状/根因/锁定测试），
   锁定测试删除或改名时必须同 commit 更新本档案。

三层防线分工：
- **第一层（锁定测试）**：下表每行的 `test_*`——行为级，锁单条缺陷；
- **第二层（守门测试）**：`tests/test_guardrails.py`——结构级，锁不变量
  （鉴权覆盖/路由快照/文档一致/分块上限/常量时间比较/SDK 在库/档案完整）；
- **第三层（门禁流程）**：`make verify`（契约+守门双模式全绿才可交付）+
  本档案的记档纪律（同类错误第二次出现 = 流程事故，不是测试事故）。

## 缺陷档案

| ID | 日期 | 症状 | 根因 | 锁定测试 |
|---|---|---|---|---|
| R1 | 2026-09-06 | 上传到不存在父目录时第一个分块就写盘失败（mkdir 形同虚设） | mkdir 排在分块写之后而非之前 | `test_sandbox_file_upload`（4 次 exec 顺序断言）、`test_upload_spaced_parent_dir_quoted_and_created_first` |
| R2 | 2026-09-06 | 含空格/通配符的目录被词拆分，mkdir 造错目录、mv 失败 | `$(dirname …)` 未加引号，弹 glob | `test_upload_spaced_parent_dir_quoted_and_created_first` |
| R3 | 2026-09-01 | 大文件上传在沙箱内截断/损坏 | 分块未做 3 字节对齐，各段 base64 各带 padding，沙箱内单条 `base64 -d` 流式解码在段中遇 padding 中断 | `test_sandbox_file_upload_chunking`、`test_upload_large_streaming_file` |
| R4 | 2026-09-06 | 大分块被网关拒收（OUT_OF_RANGE "limit is 1048576"），上传全链失败 | 误按 gRPC 默认 4MiB 假设；网关实测拒收 >1MiB 的 ExecSandbox 消息 | `test_upload_chunk_within_gateway_receive_ceiling`（守门）、`test_upload_large_streaming_file`（上限断言） |
| R5 | 2026-09-06 | exec 传裸字符串 command 被拆成单字符数组，沙箱内**静默执行垃圾命令** | 接口层未校验 command 类型即 `list()` | `test_exec_rejects_non_list_command_and_bad_stdin` |
| R6 | 2026-09-06 | 非法数值参数（limit/lines/offset/timeout_seconds/target_port）、spec/policy 未知字段、坏 stdin_b64 一律泄漏为 502，上游按"网关不可达"重试/降级 | 客户端格式错误未在接口层拦截，透传到 SDK/proto 层炸出未捕获异常 | `test_malformed_numeric_params_map_400`、`test_invalid_spec_and_policy_map_400`、`test_exec_rejects_non_list_command_and_bad_stdin` |
| R7 | 2026-09-06 | token 比较非常量时间（时序侧信道） | 直接字符串比较而非 `hmac.compare_digest` | `test_token_comparison_is_constant_time`（守门） |
| R8 | 2026-09-06 | `openshell_lib_path()` 残留旧引擎检出回退路径，指向已消亡位置 | 世界模型变更后死路径未清（LESSONS #2 同族） | 部署纪律（代码已删回退链）；守门 `test_vendored_sdk_tree_present` 锁定现役树 |
| R9 | 2026-09-05 | fresh clone 后 manager 镜像构建必败 | vendored SDK python 子树是嵌套上游仓整树被 gitignore，Dockerfile COPY 输入缺失（1e02c20 纳管修复） | `test_vendored_sdk_tree_present`（守门） |
| R10 | 2026-09-05 | 按 deploy/Dockerfile.manager 部署容器起不来 | 源码已 FastAPI 化而配方停留 stdlib（缺 fastapi/uvicorn）——源码与部署配方漂移 | 部署链纪律：`deploy/deploy.sh check` + 伞仓 check-wiring；离线测试不覆盖（缺口已标记） |
| R11 | 2026-09-07 | 字符串字段收非字符串（workspace/name/env/workdir/provider/…）透传进 SDK/proto 抛 TypeError → 502 泄漏（上游误判网关不可达而重试/降级）；exec timeout 非数值同样泄漏 | R6 修复只覆盖了数值参数与 proto 解析路径，字符串字段族未收口 | `test_non_string_fields_map_400_not_502`、`test_exec_rejects_bad_env_workdir_and_timeout` |
| R12 | 2026-09-07 | README 承诺的直跑模式 `python3 tests/test_contract.py` ModuleNotFoundError | `from openshell_manager import …` 在 sys.path.insert 之前执行 | 门禁双模式（verify.sh 同时跑 pytest 与直跑） |
| R13 | 2026-09-11 | 布尔字段（services `domain`、route `no_verify`）收字符串 `"false"` 被恒真打开（行为与字面相反）；数字等垃圾类型同样恒真 | `bool(body.get(...))` 的 Python 真值语义，非严格解析 | `test_boolean_string_fields_strict` |
| R14 | 2026-09-11 | `Transfer-Encoding: chunked` 的 JSON 请求体被整包丢弃（静默当空对象 → 400 missing field） | 无 `Content-Length` 头时 `length==0` 短路，永远读不到 body | `test_chunked_json_body_not_dropped` |
| R15 | 2026-09-11 | `/logs` 把路由里的沙箱名原样当 `sandbox_id` 查 `GetSandboxLogs`（该 RPC 与 /exec 同为 UUID 口径），网关侧必 NOT_FOUND | 接口层缺 name→UUID 解析，违反 ADR-173"REST 路径参数一律接口层解析"总则（docs/api-external 旧 3.9"网关按名解析"说法无实据——proto 字段命名即 `sandbox_id`；线上裁定不可得：现役实例 token 不在本仓，探测 401） | `test_logs_resolve_name_to_uuid` |
| R16 | 2026-09-11 | 上传收流中途异常（客户端断连）泄漏 spool 临时文件（大文件直落磁盘） | 接收循环在 try/finally 保护之外，仅 `run_in_threadpool(work)` 段有 finally | `test_upload_spool_closed_on_stream_error` |
| R17 | 2026-09-11 | 七个 async 端点裸调同步 facade（阻塞 gRPC），慢南向调用在途时 /healthz 探活与全部并发请求被劫持（实测排队 1.5s+，gRPC timeout=60s 兜底）；wait_ready 的 timeout_seconds 无服务端上限（客户端可传 1e9 无限占用南向连接） | async def 路由把同步阻塞调用直接跑在事件循环上；缺超时上限校验 | `test_slow_exec_does_not_block_healthz`、`test_wait_ready_timeout_server_side_cap` |
| R18 | 2026-09-11 | tokenFile 已配置但读失败（EACCES/EIO）被吞异常当空 token——读失败瞬间整面鉴权失效（fail-open）；且每请求同步读盘阻塞事件循环 | `_token_from_file` 对一切异常返回 ""；require_token 无 fail-closed 分支 | `test_token_file_auth_and_fail_closed`、`test_manager_token_file_result_cached` |
| R19 | 2026-09-11 | 未捕获异常兜底 502 + `ExcType: msg`——上游把服务端缺陷按"网关不可达"误重试/降级，且异常细节直接泄漏给客户端 | 兜底处理器沿用上游失败口径 502 且拼入异常细节 | `test_unhandled_exception_maps_500_generic`、`test_upstream_error_mapping` |
| R20 | 2026-09-11 | `maxUploadBytes` 缺省 0（不限），纯防误操作的上限形同虚设 | 缺省值与坏值回落都是 0 | `test_max_upload_bytes_defaults_to_2gib` |
| R21 | 2026-09-11 | `_int_field` 用 `int()` 强转放过垃圾类型：target_port 收 "8123"（字符串数字）、8123.5（float）、True（bool 恒真陷阱）均被静默接受 | 宽松类型强转而非严格类型校验（`_opt_bool` 同族缺陷） | `test_target_port_strict_integer` |
| R22 | 2026-09-12 | 沙箱不存在 → 404 的文档契约在生产路径从未生效：GET/DELETE `sandboxes/{name}`、/logs、/files、wait-ready、/exec 错 id 全落兜底 500 | 真实 SDK 对网关 NOT_FOUND 裸抛 `grpc.RpcError`（非 LookupError 子类，MRO 实测），而 404 映射只挂 LookupError；测试假件恰抛 LookupError 喂给映射，绿灯掩盖生产 500 | `test_missing_sandbox_maps_404_not_500`、`test_exec_unknown_uuid_maps_404`、`test_upstream_non_not_found_stays_500`（修复批；facade 层 `_map_not_found` 只映射 NOT_FOUND，其余 RpcError 维持 500 兜底） |
| R23 | 2026-09-12 | exec `timeout_seconds` 无服务端上限：1e9 畅通（gRPC deadline 被抬至 1e9+10s，占线程池令牌+南向连接，40 并发饿死全部同步端点）；负值直通 proto | 只给 wait_ready 加了 cap，同族 exec 漏修；`int()` 强转放行 bool/float/字符串数字 | `test_exec_timeout_server_side_cap`（修复批；0..600 上限 + 严格 int，engine 现役实参 60/20 兼容性已核） |
| R24 | 2026-09-12 | tokenFile 存在但内容为空 → 整面 fail-open（非 loopback bind 轮换窗口即裸奔；只对"读失败"fail-closed 的残余缺口） | `_token_from_file` 只区分"不存在=未配置"，空内容静默当无 token；`validate()` 仅启动检查一次，运行时变空不再复查 | `test_empty_token_file_fail_closed`（修复批；`TokenFileEmptyError`：运行时一律 503、validate() loopback 放行启动但 /api/* 持续 503/非 loopback 拒启、异常不落缓存——写入真值至多一个缓存 TTL（5s）后自愈） |
| R25 | 2026-09-12 | multipart 前置阶段（首 boundary 扫描 / part 头块）内存无界——2GiB 缺省上限内发永不含 boundary 的 body 可把全量字节缓冲进 RAM，OOM 管理面 | `_expect_first_boundary`/`_read_headers` 命中终止符前 `_fill()` 无限累积，"内存恒定"只对 file part 成立 | `test_multipart_preamble_capped`、`test_multipart_part_headers_capped`（修复批；前导 1 MiB / 头块 64 KiB 上限 → 400） |
| R26 | 2026-09-12 | 数值与类型纪律收尾批：负数/超 int32（limit/lines/offset/since_ms）与越界 target_port 直通 proto 构造炸 ValueError → 500；wait_ready timeout 收 "300"/True/NaN（`nan > cap` 恒 False 绕 上限）与负值 → 500；upsert 的 credentials/config 非法类型炸 500、并发双 Create 败者收 ALREADY_EXISTS → 500；finalize mv/chmod 失败不走 `.part` 清理（违背 docstring 承诺）；兜底日志 path 可被 %0A 注入伪日志行 | R21 严格化只覆盖了 target_port 一个调用点；数值域无范围校验；provider 的 map 形态字段是 R11 字符串族漏网；mv/chmod 位于清理 try 块之外；日志直拼百分号解码后的路径 | `test_query_int_range_maps_400`、`test_target_port_range`、`test_wait_ready_strict_number`、`test_provider_credentials_strict`、`test_upsert_race_converges_to_update`、`test_upload_finalize_failure_cleans_part`、`test_log_safe_strips_control_chars`、`test_unhandled_log_line_sanitized`（修复批；后者为 09-13 代码审查补全——兜底日志对异常文本同样中和，南向异常 detail 常回显解码后的路径参数） |
| R27 | 2026-09-12 | exec 输出无界累积：`cat /dev/zero` 类命令 60s 内以线速攒 stdout/stderr → OOM 管理面（殃及全部租户的沙箱管理） | SDK `exec` 内部对输出无上限攒流；facade 整段消费（上传路径有 `capped()`，唯用户 exec 无界） | `test_exec_output_cap_maps_413`（修复批；facade 改走 `exec_stream` 流式消费 + 64 MiB 累计上限 → 413；生成器惰性拉取使 SDK 侧同步受界，vendored 上游树零改动） |
| R28 | 2026-09-12 | deploy.sh：`stop` 失败回落 `compose up -d`（停不住反拉起且静默"成功"）；`pct push` 绕过 REMOTE 契约（REMOTE=""=本机执行时 LXC 内无 pct 必炸；ssh 形态时 env 被打到错误宿主）；token 提取 `cut` 作用于整个 env 文件（按模板补全成多行后 Authorization 头跨行垃圾）；env 缺失时 check 对 .env 项假失败 | `start\|stop\|restart` 共用 `\|\| compose up -d` 回落；pct 硬编码不看 REMOTE 形态（openshell-gateway 同族漏做）；只修行内 padding 截断未修"行 selection" | `tests/deploy_cases.sh` 五用例（修复批；PATH 桩；变异自检：回退 stop 回落/pct 无条件/cut 全文件/守卫永远-skip 四组逐一变红） |
| R29 | 2026-09-12 | 根 Dockerfile：`USER root` 装依赖后漏切回（产物 root 运行，与 deploy/Dockerfile.manager 的 65534 纪律相悖）；基础镜像 1.0.0 内烘焙 .token 随产物扩散风险未警示；COPY build/wheels 输入不入 git（fresh clone 必败，R9 同族）未如实标注 | 离线叠加路径只顾装依赖，非 root 收尾与秘密/重建输入的文档纪律缺失（REGRESSIONS「已知未覆盖缺口」早已预告静态断言） | `test_root_dockerfile_discipline`（修复批守门；最终 USER=65534:65534 + .token 禁外发警示 + pip download 指引） |
| R30 | 2026-09-13 | inference 路由面错误契约缺口（dind 生产栈七战 e2e 实证）：全新部署 `GET /api/v1/inference/route` 南向 NOT_FOUND（workspace 未配置路由）裸 500——R22 的 NOT_FOUND→404 映射只给了沙箱面；`PUT` 实核失败南向 FAILED_PRECONDITION 裸 500，客户端可修正条件（换 provider/修 base_url/改 no_verify）落进不可行动的 internal error | `_map_not_found` 引入时只挂 get/set 沙箱方法；set_route 的 FAILED_PRECONDITION 从未进入映射面（exec 413 前同形态） | `test_route_unconfigured_404_and_verification_failed_400`（NOT_FOUND→404 / FAILED_PRECONDITION→400+可行动文案 / UNAVAILABLE 维持 500 不扩大映射面；api 层新增 InferenceVerificationFailed 处理器） |

## 已知未覆盖缺口（如实记录，非缺陷）

- 上传 spool 收流中途超过声明 Content-Length 的 413 分支：需要伪造传输层
  分帧，依赖 uvicorn/h11 内部行为，未做（`maxUploadBytes` 头部预检已锁）；
- `GatewayFacade` 懒加载的并发线程安全（`_lock`）：无并发压力场景，引擎为
  串行调用，未做压测；
- R10 类"源码↔部署配方"漂移目前只有 deploy.sh check 兜底，离线门禁测不到
  镜像内依赖——根 Dockerfile 的 USER/.token 警示/wheels 指引已由
  `test_root_dockerfile_discipline` 静态锁定（R29），pip 依赖清单仍靠
  deploy.sh check 兜底；
- R27 后 exec 输出内存 = facade 侧累计（≤64 MiB）+ SDK 生成器惰性拉取的
  等量缓冲，放弃消费即释放；SDK 树内部实现零改动（dsh-runtime 上游 fork
  内部零改动纪律），若上游提供有界消费 API 可再收敛。

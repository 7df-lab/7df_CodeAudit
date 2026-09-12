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
| R13 | 2026-09-11 | 布尔字段（services `domain`、route `no_verify`）收字符串 `"false"` 被恒真打开（行为与字面相反）；数字等垃圾类型同样恒真 | `bool(body.get(...))` 的 Python 真值语义，非严格解析 | `test_boolean_string_fields_strict`（B1-12 审计批；`_opt_bool` 只收布尔或 "true"/"false" 字符串，其余 400） |
| R14 | 2026-09-11 | `Transfer-Encoding: chunked` 的 JSON 请求体被整包丢弃（静默当空对象 → 400 missing field） | 无 `Content-Length` 头时 `length==0` 短路，永远读不到 body | `test_chunked_json_body_not_dropped`（B1-13 审计批；chunked 走流式读取，同受 20 MiB 上限） |
| R15 | 2026-09-11 | `/logs` 把路由里的沙箱名原样当 `sandbox_id` 查 `GetSandboxLogs`（该 RPC 与 /exec 同为 UUID 口径），网关侧必 NOT_FOUND | 接口层缺 name→UUID 解析，违反 ADR-173"REST 路径参数一律接口层解析"总则（docs/api-external 旧 3.9"网关按名解析"说法无实据——proto 字段命名即 `sandbox_id`；线上裁定不可得：现役实例 token 不在本仓，探测 401） | `test_logs_resolve_name_to_uuid`（B1-15 审计批；与 /files 同口径走 `resolve_sandbox_id`） |
| R16 | 2026-09-11 | 上传收流中途异常（客户端断连）泄漏 spool 临时文件（大文件直落磁盘） | 接收循环在 try/finally 保护之外，仅 `run_in_threadpool(work)` 段有 finally | `test_upload_spool_closed_on_stream_error`（B1-12 审计批；spool 全程 try/finally） |
| R17 | 2026-09-11 | 七个 async 端点裸调同步 facade（阻塞 gRPC），慢南向调用在途时 /healthz 探活与全部并发请求被劫持（实测排队 1.5s+，gRPC timeout=60s 兜底）；wait_ready 的 timeout_seconds 无服务端上限（客户端可传 1e9 无限占用南向连接） | async def 路由把同步阻塞调用直接跑在事件循环上；缺超时上限校验 | `test_slow_exec_does_not_block_healthz`、`test_wait_ready_timeout_server_side_cap`（B3-1 审计批；统一 `run_in_threadpool` + 缺省 300/硬上限 600） |
| R18 | 2026-09-11 | tokenFile 已配置但读失败（EACCES/EIO）被吞异常当空 token——读失败瞬间整面鉴权失效（fail-open）；且每请求同步读盘阻塞事件循环 | `_token_from_file` 对一切异常返回 ""；require_token 无 fail-closed 分支 | `test_token_file_auth_and_fail_closed`、`test_manager_token_file_result_cached`（B3-2 审计批；`TokenFileError`→503+stderr、文件结果 5s 缓存） |
| R19 | 2026-09-11 | 未捕获异常兜底 502 + `ExcType: msg`——上游把服务端缺陷按"网关不可达"误重试/降级，且异常细节直接泄漏给客户端 | 兜底处理器沿用上游失败口径 502 且拼入异常细节 | `test_unhandled_exception_maps_500_generic`、`test_upstream_error_mapping`（B3-3 审计批；500 + `internal error`，细节进服务端 stderr） |
| R20 | 2026-09-11 | `maxUploadBytes` 缺省 0（不限），纯防误操作的上限形同虚设 | 缺省值与坏值回落都是 0 | `test_max_upload_bytes_defaults_to_2gib`（B3-3 审计批；缺省/回落 2 GiB） |
| R21 | 2026-09-11 | `_int_field` 用 `int()` 强转放过垃圾类型：target_port 收 "8123"（字符串数字）、8123.5（float）、True（bool 恒真陷阱）均被静默接受 | 宽松类型强转而非严格类型校验（`_opt_bool` 同族缺陷） | `test_target_port_strict_integer`（B3-3 审计批；只收 int） |

## 已知未覆盖缺口（如实记录，非缺陷）

- 上传 spool 收流中途超过声明 Content-Length 的 413 分支：需要伪造传输层
  分帧，依赖 uvicorn/h11 内部行为，未做（`maxUploadBytes` 头部预检已锁）；
- `GatewayFacade` 懒加载的并发线程安全（`_lock`）：无并发压力场景，引擎为
  串行调用，未做压测；
- R10 类"源码↔部署配方"漂移目前只有 deploy.sh check 兜底，离线门禁测不到
  镜像内依赖——若再次发生，考虑在 guardrails 加 Dockerfile pip 行静态断言。

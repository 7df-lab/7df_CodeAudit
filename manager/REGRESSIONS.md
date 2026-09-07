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

## 已知未覆盖缺口（如实记录，非缺陷）

- 上传 spool 收流中途超过声明 Content-Length 的 413 分支：需要伪造传输层
  分帧，依赖 uvicorn/h11 内部行为，未做（`maxUploadBytes` 头部预检已锁）；
- `GatewayFacade` 懒加载的并发线程安全（`_lock`）：无并发压力场景，引擎为
  串行调用，未做压测；
- R10 类"源码↔部署配方"漂移目前只有 deploy.sh check 兜底，离线门禁测不到
  镜像内依赖——若再次发生，考虑在 guardrails 加 Dockerfile pip 行静态断言。

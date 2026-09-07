# 内部接口契约：模块间预期输入与预期输出

> 事实源文档：`extension.ts` 是薄胶水层，业务逻辑全部在可单测的纯模块中。本文逐模块
> 描述导出符号的输入/输出/错误语义/不变量，并标注锁定测试。契约变更纪律见
> [regressions.md](regressions.md)。模块依赖方向：`extension.ts → 各纯模块 → types.ts`；
> 纯模块之间仅 `applyPatch.ts → diffParse.ts`（canonicalize）与
> `treeModel/aiContextView → diagnosticsMapper/progressModel`（标签与格式化）两处横向引用。

---

## 1. `types.ts` — 平台 DTO（数据总源）

- `TERMINAL_STATUSES = [COMPLETED, CANCELLED, TIMEOUT, DEAD]`；
  `isTerminalTaskStatus(status)`：预期输入为枚举名字符串，输出布尔。终态是 watcher 关停、
  互斥释放、UI 收尾的共同判据。
- 其余为接口文档 §3 所述 DTO 声明，无行为。

## 2. `apiClient.ts` — REST 客户端

| 符号 | 预期输入 | 预期输出 | 错误语义 | 锁定测试 |
|---|---|---|---|---|
| `encodeQuery(params)` | `Record<string, unknown>` | `?k=v&…` 串（空集回 `''`）；对象值 JSON、标量 String、`undefined/null/''` 跳过 | 不抛 | `apiClient.test.ts › encodeQuery` |
| `backoffMs(retryAfterS, nowMs)` | 秒数（可 undefined）+ 当前 ms | `now + clamp(s??15, 5, 60)*1000` | 不抛 | `apiClient.test.ts › 429…钳位`（间接） |
| `ApiError` | status/message/body | Error 子类，`name` 未改、`status`/`body` 可读 | — | 各处 rejects(ApiError) |
| `CodeAuditClient` 全部方法 | 见 external-interfaces.md §1 | 见 §1 | 非 2xx → ApiError；401 → 单飞刷新重放一次 | 同 §1 |

不变量：`refreshInFlight` 并发共享一次刷新，finally 置空；`rateLimitUntil` 只写不清（由时间流逝自然过期）。

## 3. `taskWatcher.ts` — 双通道任务跟踪

- **构造输入**（全可注入，测试依赖此契约）：`taskId`、`client`、`wsUrl(taskId,token)`、
  `getAccessToken`、`setWsLive`、`onWsEvent`、`onTaskGone`、`cursors()`、
  `setTimeoutFn/clearTimeoutFn/makeSocket/now`。
- **输出事件**（EventEmitter）：`snapshot(TaskSnapshot)` 每次有效快照；`terminal(status, snap)` 恰好一次后自关。
- **不变量**：
  1. `start()` = 连 WS + 立即兜底轮询一次（WS 建立前也有状态）；
  2. 终态 → `terminal` → `close()`：清全部定时器 + 关 socket，重连/续轮均被 `closed` 拦截；
  3. WS reason 或轮询错误消息同时匹配 `/404/`+`/not found/i` → `onTaskGone` 终止（防死循环）；
  4. `client.rateLimitUntil > now()` 时轮询轮跳过。
- 锁定测试：`taskWatcher.test.ts` 全部 6 例。

## 4. `progressModel.ts` — 四路帧归并（纯逻辑核心）

| 符号 | 预期输入 → 预期输出 | 边界/错误语义 | 锁定测试 |
|---|---|---|---|
| `asNumber(v)` | string/number/undefined/null → number | `Number(v??0)` 非有限回 0 | `parseTsMs/fmt` 用例覆盖 |
| `parseTsMs(ts)` | ISO 串（可 9 位小数秒）→ epoch ms | 空/非法 → `null`；>23 字符截前 23 位加 `Z` | `› parseTsMs 容忍 9 位小数秒` |
| `createProgressState(taskId)` | 任务 ID | 全零值初始态（version=0, wsLive=false） | 各用例基底 |
| `appendAiChunk(text, cursor, chunkText, nextCursor)` | 当前文本/字节游标/新块/新游标 | `{text, cursor}` 或 `null`=丢弃。规则：① `next<=cur && cur!==0` → null；② chunkStart>cur 或 cur=0 → **整体重置**为 chunk；③ 衔接 → 按 utf-8 字符边界跳过已见前缀只拼新增尾 | `› ai chunk 增量追加`、`› 全量重发…重置`、`› 多字节字符不撕裂`、`› 跳段整体重置`、`› cursor=0 首帧直通` |
| `estimatePercent(stages)` | TaskStage[] | COMPLETED/SKIPPED=1、RUNNING=0.5 → 四舍五入 min 100；空 → 0 | `› 无 progress 帧按阶段完成度估算` |
| `sandboxPackCheck(logs, expectedUploadBytes)` | 日志数组 + 上传字节数 | `null`=尚无「打包完成」行；否则 `{received, tooSmall}`，floor=max(64, ⌊expected/100⌋)，取**最近一条**命中行；无字节数 → received=0 判废 | `› 沙箱收包校验` 4 例 |
| `logIdAfter(a, b)` | log_id 串 | 双方纯数字 → 数值比较（`"10">"9"` 回归锁）；否则字典序 | `› logs 按 log_id 数值序增量去重` |
| `applyFrame(state, frame)` | 可变态 + TaskSnapshot 帧 | 就地更新并返回 state。语义：status 缺省保旧；stages 取 progress（非空）否则 task；percent 取 overall_percent（>0 时钳 0~100 四舍五入）否则估算；logs 增量去重（容量 500 截尾）；ai 走 appendAiChunk + total_bytes/complete 吸收；**前后签名（status\|percent\|阶段\|lastLogId\|aiCursor\|aiComplete）变化才 version++ 并刷新 updatedAt** | `› applyFrame` 系列、`› version 只在内容演进时递增` |
| `buildProgressItems(state)` | ProgressState | 节点序 = 任务头 → 各阶段 → AI 入口 → （DEAD/TIMEOUT 时）失败摘要。任务头 desc=`{中文状态} · {pct}% · {WS 实时\|轮询}`；阶段 desc=`{中文状态} · {耗时}`（运行中按 now 计算）、失败阶段 contextValue=stageFailed；AI 入口 desc=字节量+流式/收束/暂停态；失败摘要含最近日志前 60 字符 | `› buildProgressItems` 3 例 |
| `stageLabel / taskStatusLabel / fmtDuration / fmtBytes` | 见代码映射表 | 未知枚举回原值；fmtDuration 负数 → `—` | `› fmtDuration / fmtBytes / 标签映射` |

## 5. 补丁引擎（`applyPatch.ts` 主路径 + `diffParse.ts` 兜底）

共享语义（两引擎同源 Cline）：`canonicalize`（NFC + 智能标点归一 + 转义还原）；四级容错
锚定 fuzz 0（精确）/ 1（trimEnd）/ 100（trim）/ 1000（相似度 ≥0.66）；顺序游标消歧；
**任一 hunk 失配 → 整体拒绝，绝不部分应用**。

### 5.1 `applyPatch.ts`（apply_patch 语法，`diff_patch` 主路径）

| 符号 | 预期输入 → 预期输出 | 关键语义 | 锁定测试 |
|---|---|---|---|
| `listUpdatedFiles(patchText)` | 补丁全文 → 路径数组 | 仅 Update/Delete 段（调用方预载内容用）；Add 段不含；哨兵归一后提取去重 | `applyPatch.test.ts › 列出 Update/Delete 段路径` |
| `computePatchChanges(patchText, currentFiles)` | 补丁全文 + `{路径: 当前内容}`（Update/Delete 路径必须存在） | `{changes: Record<路径, PatchFileChange>, fuzz}`；`PatchFileChange = {type, oldContent?, newContent?, movePath?}` | 主用例群 |
| （内部）PatchParser | 行数组 + currentFiles | Update：`@@ defStr` 三级匹配（canonTrim 全等 → canonExact 全等 → trim 全等，仅第三级 fuzz+1）后 peek+findContext 锚定；**跳跃 hunk（有 defStr 且整体失配或 fuzz≥1000）走 `tryLineByLineAnchor` 逐行顺序锚定**，全逐字命中按绝对行号重组且 fuzz 不增；未逐字命中 → warning 拒绝，绝不相似度错位 | `› @@ 定义行…`、`› 跳跃 hunk…`、`› 逐行锚定任一行未命中 → 整体拒绝` |
| （内部）peek | 补丁行 | 产出 [上下文块(上下文+删除行), chunks, 停止位, eof]；无前缀行补空格容错；`***` 单行终止、`***x` 非法行 DiffError；`*** End of File` → eof | `› 无前缀行按上下文行容错`、`› End of File…` |
| （内部）findContext(applyPatch 版) | 文件行/上下文/起点/eof | `[锚点\|-1, fuzz, 最高相似度]`；eof 先末尾锚定、未命中回退全文扫描并 **fuzz+10000**（透明标注） | `› *** End of File 未在末尾命中 → fuzz+10000` |
| （内部）applyChunks | 内容 + chunks | origIndex 越界 / currentIndex 重叠 → DiffError（防御） | 间接覆盖 |
| `formatPatchWarnings(warnings)` | 失配列表 → 用户可读多行文本 | 头行「…已整体拒绝应用：」+ `路径: hunk N: 上下文未命中（相似度 X）` + ≤200 预览 | `› 上下文失配 message…` |
| 哨兵/包装归一 | Begin/End 齐全 → 取区间；只其一或乱序 → DiffError incomplete sentinels；自由格式剥 bash 包装后补哨兵 | `› 哨兵不完整…`、`› 遗留 bash 包装…`、`› 自由格式…` |  |
| 段级校验 | Update 文件必须存在 / Add 必须不存在 / 同文件重复段 / 未知指令行 → DiffError 整体拒绝 | `› Add 目标已存在…` 等 4 例 |  |
| EOL 语义 | 块运算在 LF 空间；输出按文件自身 EOL 还原（CRLF 不被改写）；Add 新文件 win32 取 CRLF；补丁文本自身带 \r 先归一 | `› CRLF 文件 + LF 补丁…` 等 3 例 |  |

### 5.2 `diffParse.ts`（unified diff，`ai_fix_suggestion` 兜底）

| 符号 | 预期输入 → 预期输出 | 关键语义 | 锁定测试 |
|---|---|---|---|
| `extractDiffBlock(suggestion)` | 建议全文 | 首个 ```` ```diff ```` 围栏内容或 `null` | `diffParse.test.ts › 提取 ```diff 围栏块` |
| `parseUnifiedDiff(diff)` | diff 文本 → FilePatch[] | `---`/`+++`/`@@ -N[,M]`；`\\ No newline` 跳过；尾部 split 空串不当作上下文行（防吞行）；其余裸空串仍按空上下文行；a/ b/ 前缀与反斜杠归一 | `› 解析 hunk…`、`› 末尾换行…`、`› \ No newline…`、`› a/ b/ 前缀…` |
| `canonicalize(s)` | 任意串 | NFC + 标点归一 + `\\' " `` ` 转义还原 | `applyPatch.test.ts › canonicalize 转义还原` |
| `findContext(lines, context, start, insertHint)` | 文件行/上下文块/游标/声明行 | `ContextHit{index,fuzz,similarity?}` 或 `ContextMiss{index:-1,bestSimilarity}`；四级级联；空 context（纯插入）锚定 max(start, insertHint)；重复块取首个命中（顺序消歧） | `› 级 1~4…`、`› 重复块…`、`› 空 context…` |
| `applyResolvedChunks(fileLines, chunks, filePath)` | 行数组 + 升序 chunks | `{lines}` 或 `{error}`（起点越界/区间重叠） | `› 升序 chunks 正确拼接` 等 3 例 |
| `applyPatchToLines(fileLines, patch)` | 行数组 + FilePatch | `PatchResult{lines\|null, applied[], failures[], fuzz}`；任一失败 → lines=null + applied 清空（整体拒绝）；拼接出错也归入 failures | `› 精确锚定应用…`、`› 任一 hunk 未命中 → 整体拒绝`、`› 多 hunk fuzz 汇总` |
| `formatPatchFailures(oldPath, failures)` | 失败列表 | 逐 hunk 可读原因 | `› formatPatchFailures…` |
| `buildFilePatch(before, after)` | 修复前后全文 | `{hunks, shifts}`（LCS 差异，带上下文行）或 null——登记表 patches 数据源（外科回滚/行号迁移） | `applyPatch.test.ts › buildFilePatch…` 系列 |
| `invertFilePatch(p)` | FilePatch | 交换 old/new 的逆补丁——外科回滚走与正向应用同一套锚定/整体拒绝 | `applyPatch.test.ts › 乱序回滚回归锁…` |
| `shiftLine(line, shifts)` | 原始行号 + 偏移表 | 迁移后行号（多块坐标系，判据恒用原始行号） | 同上 |

## 6. 视图渲染（纯函数，不 import vscode）

| 符号 | 预期输入 → 预期输出 | 关键语义 | 锁定测试 |
|---|---|---|---|
| `aiContextView.escapeHtml` | 串 | `& < > " '` 五元全转义 | `aiContextView.test.ts › 五个 HTML 元字符全转义` |
| `buildViewUpdate(state)` | ProgressState → `AiViewUpdate` | `{type:'update', h1Html, percent(钳 0~100), chipsHtml, logsHtml, aiHtml}`；日志取尾 200；AI 正文超 256KB 保尾；空态给说明文案不空白 | `› buildViewUpdate…`、`› 超长 AI 正文…` |
| `renderAiContextHtml({state,title?})` | state=null → 空态页；否则整页 HTML | CSP `default-src 'none'`；日志窗在上 AI 主区在下；页内脚本：贴底跟随（nearBottom<48px）、增量 message 按 type 消费 | `› 无任务空态…`、`› 分区布局…` |
| `renderFindingDetailHtml(data)` | `{finding\|null, fixed}` | null → 引导空态；头部严重级徽章/标题/✔徽章 + 元信息表 + 操作按钮（fixed→回滚，否则修复）+ 描述/AI 分析/修复建议/补丁分区（空内容区块不渲染）；全部字段转义 | `findingDetailView.test.ts` 4 例 |
| `AiContextViewProvider.resolveWebviewView(view)` | WebviewLike（html/options/postMessage/…） | **先置 `enableScripts:true` 再赋 html**（回归锁）；onRendered 回调；可见性恢复→postUpdate；dispose→view=null | `aiContextViewProvider.test.ts` 2 例 |
| `AiContextViewProvider.postUpdate()` | — | 有 state 且 view 存在才 postMessage；否则静默 | 同上 |

## 7. 展示模型（findings → 树/诊断/候选）

| 符号 | 预期输入 → 预期输出 | 关键语义 | 锁定测试 |
|---|---|---|---|
| `treeModel.buildTree(findings)` | UnifiedFinding[] → TreeNode[]（扁平：file 后跟其 findings） | 按 file_path（反斜杠归一）分组；路径 localeCompare 升序；组内 severityRank 降序；无位置 → 末尾「(无位置)」组 | `treeModel.test.ts` 3 例 |
| `treeModel.findingLabel / findingDescription` | finding | label=`{严重级}{ [CWE]} {title\|file:line 兜底}`；desc=`{工具}{ · AI:结论}` | `› findingLabel/Description…` |
| `diagnosticsMapper.mapFinding(f)` | finding → MappedDiagnostic\|null | 无 file_path 或 start_line → null（不进 Problems）；1-based → 0-based；end=max(start, ⌊end_line??start⌋-1)；code 优先 cwe_id | `diagnosticsMapper.test.ts` 5 例 |
| `severityRank(sev)` | 枚举名 | CRITICAL/HIGH=3、MEDIUM=2、LOW=1（未知回退）、INFO=0、UNSPECIFIED=1 | `› severity 枚举名映射…` |
| `groupFindingsByFile(findings)` | findings → Map | 空路径跳过；组内 severity 降序 + title 升序 | `› groupFindingsByFile…` |
| `lowRiskApply.selectLowRiskFixCandidates(findings, excludeIds)` | findings + 登记表已知 ID 集 | 同时满足：severity∈{LOW,INFO} ∧ ai_confidence≥0.9 ∧ 有 diff_patch ∧ 未登记过；阈值含边界（≥0.9 精确命中） | `lowRiskApply.test.ts` 3 例 |

## 8. 存储抽象（fs 注入，可单测）

| 符号 | 预期输入 → 预期输出 | 关键语义 | 锁定测试 |
|---|---|---|---|
| `CheckpointStore.save(files)` | `{绝对路径: 内容\|null}` | 空集 → `null`；id=`cp-<ts>-<seq++>`（同毫秒不碰撞）；null=修复前不存在的文件；内容文件名=路径非字母数字全替换 `_`；manifest.json 落盘 | `checkpoint.test.ts › save…` 8 例 |
| `CheckpointStore.list/latest/restoreLatest/restore(id)` | id | list=按 (时间戳,序号) **数值序**倒序（seq 跨位数 9→10 时字典序会取错 latest，回归锁见 regressions.md #19）；restore：id 空/manifest 缺失/损坏 → `null` 不抛；**读文件必须显式 'utf-8'（Buffer 回归锁）** | `› restoreLatest 的快照必须是 utf-8 字符串…`、`› 多个 checkpoint 时 latest 取最新` |
| `FixRecord` | `{findingId, label, checkpointId, files[], appliedAt, state: applied\|rolledback}` | files = 本次触及的绝对路径全集（Update/Delete/Move 源 + Add/Move 目标） | — |
| `FixRegistry` 构造 | 文件路径 + fs | 文件缺失/损坏 → 视为无记录（不抛，checkpoint 内容仍在可重新应用） | `fixRegistry.test.ts › 损坏的登记文件…` |
| `recordApplied(rec)` | FixRecord | 覆盖同 findingId 旧记录（重新应用以最近为准）+ 持久化 | `› 持久化往返…` |
| `markRolledback(findingId)` | 发现 ID | 无记录或非 applied → `null`；否则翻状态 + 持久化 + 返回记录 | `› markRolledback 翻状态…` |
| `appliedFindingIds / appliedRecords / knownFindingIds / byFinding` | — | applied 集合（徽章）/ applied 记录（同文件更晚覆盖告警、最近批量回滚）/ 全部登记过（低风险候选排除，回滚不翻案） | `› knownFindingIds…`、`› appliedRecords…` |
| `workspaceZip.zipFiles(files, readFile)` | `{relPath, absPath}[]` + 读取器 → Blob | 单文件读取失败跳过不阻塞整包；relPath 保持 zip 内结构 | `workspaceZip.test.ts` |

## 9. `extension.ts` 胶水层内部契约（内存桩行为测试）

| 内部函数 | 预期输入 → 预期输出 | 关键语义 | 锁定测试 |
|---|---|---|---|
| `resolveWsPath(rel)` | 补丁相对路径 → `{abs, uri}\|null` | null 条件：无工作区 / 空串 / posix 绝对路径 / 含 `..` 段；反斜杠归一 | `extension.test.ts › 路径禁闭` 系列 |
| `applyMachinePatch(f, label)` | finding（diff_patch 非空） | 成功 `{ok:true, fileCount, fuzz, saveFailures, saveTotal, diffs[]}`；失败 `{ok:false, reason}`。序：预载 Update/Delete 文档 → computePatchChanges（DiffError→拒绝）→ 变更非空 → 逐条禁闭/覆盖校验 → **checkpoint（Add/Move 目标记 null）** → 应用（Update=WorkspaceEdit+save；Add/Move 写 fs；Delete=rm）→ 登记 → 产出 diff 审阅数据。任何一步失败整体放弃、不改盘 | `extension.test.ts › applyMachinePatch` 系列 |
| `writeRestored(restored)` | `{绝对路径: 内容\|null}` → `null\|摘要` | null→删除文件；**文件已缺失（Delete/Move 源被补丁移除）→ fs 直写重建**；applyEdit=false 或缓冲区与预期不一致→重试一次再失败显式报错+null（**checkpoint 未消耗**）；保存后**磁盘终验**（workspace.fs.readFile 逐字节比对，save 报 true 不代表落盘）；保存部分失败→摘要中注明 | `extension.test.ts › 回滚` 系列 |
| `rollbackRecord(rec)` | FixRecord | 同文件更晚 applied 修复会被波及 → 警告确认（「仍要回滚」）；checkpoint 缺失/损坏→错误+登记不变；成功→markRolledback+徽章解除+详情刷新 | `extension.test.ts › 按发现回滚…` |
| `watchTask(taskId, expectedUploadBytes, resumeState)` | 任务 ID/上传字节/是否恢复 | 关旧 watcher；resumeState=true 保留已重建历史仅续订（bindTask 非终态任务用）；快照回调：applyFrame + taskPaused 上下文 + **一次性沙箱收包校验**（tooSmall→cancelTask+错误通知，progress.uploadSizeChecked 置位）+ refreshProgressUi；terminal 回调：互斥释放、取消/失败/完成三分支 | `extension.test.ts › watchTask…` 系列 |
| `bindTask(taskId, {silent})` | 历史/任意任务 ID | snapshot→新 progress→applyFrame→UI 重建→findings 拉取；**非终态任务自动 watchTask(resumeState=true) 续订**；404 not found→清 lastTaskId（防下次启动再恢复死任务）；其余失败→通知（silent 时仅日志） | `extension.test.ts › 恢复链路` 系列 |
| `doScan()` | — | 互斥（scanning）→ requireReady（登录+绑定）→ findFiles→minPackFiles→zip→upload→**config.upload_file_id‖dir 二选一**→createTask→startTask→watchTask；失败→互斥释放+错误通知 | `extension.test.ts › doScan` 系列 |
| `restoreLastTask` | 启动时（boot 后） | lastTaskId 优先；否则登录+绑定项目→最近完成任务；silent，失败仅日志 | `extension.test.ts › 恢复链路` |
| `doRefresh` | — | 有 lastTaskId 仅重拉 findings；否则兜底绑定最近完成任务 | `extension.test.ts › 刷新兜底` |
| `doOpenConsole` | — | `consoleUrl‖serverUrl:端口→:4173` + `/tasks/{lastTaskId}` → openExternal | external-interfaces.md §4.1 |
| `FindingDetailProvider.set(data)` | FindingDetailData | view 已解析→重渲染 html；未解析→focus 命令触发解析 | `extension.test.ts › 漏洞详情按钮动作回传` |

# 回归防线（regression-guard）——测试体系 × 防回归机制总纲

> 目标：**测试能发现问题**（覆盖契约），且**同类 bug 不三番两次复发**（每类历史缺陷
> 被「用例 + 静态守卫 + 变异证明」三重锁定，锁的存活性可机器验证）。
> 本文档是机制的**操作手册与缺陷模式档案**；接口事实源见同目录三份接口文档
> （external-interfaces / internal-interfaces / dataflows-other）。

---

## 1. 三道防线总览

```
第 1 道  契约锚定测试     docs/external-interfaces.md 每条 [E-nn] ↔ 测试用例双向追溯
                         docs/internal-interfaces.md 每条 [I-nn] ↔ 测试用例双向追溯
                         —— 契约漂移（请求形状/响应形状/错误语义）当次提交即红。
第 2 道  静态守卫         npm run guard（scripts/guard.sh）
                         —— 历史"禁区模式"直接 grep 拦截：整模块 mock、页面手写响应形状、
                            死契约复活、拦截器关键锚点被删。秒级，每次提交必跑。
第 3 道  变异检验         npm run mutation-check（scripts/mutation-check.sh）
                         —— 对每类历史 bug 的根因代码注入等价变异，证明对应测试**会红**
                            （假绿检验自动化）。锁失效（测试被删/弱化）时变异存活 → 红灯。
```

三道防线的关系：第 1 道保证「现在写得对」，第 2 道保证「旧坑不再踩」，第 3 道保证
「锁本身没锈」——**测试被删、断言被弱化、vitest 配置漂移**等让防线悄悄失效的变更，
只有第 3 道能发现。

## 2. 门禁集成（何时跑什么）

| 时机 | 命令 | 通过判据 |
|------|------|----------|
| 每次交付（同 `npm test`） | `npm test` | vitest 全绿 |
| 每次交付（同 `npm run build`） | `npm run build` | tsc -b 零错 + vite 构建零告警 |
| 每次交付（提交前，秒级） | `npm run guard` | 全部 PASS |
| 触碰防御要地后 / 里程碑出口 / 怀疑假绿 | `npm run mutation-check` | 全部变异被杀（对应测试红） |

mutation-check **要求工作区干净**（git status 无未提交改动）——它临时改源码注入变异，
靠 git 还原；脏区运行会拒绝。全量约 2-4 分钟，不必每次交付跑。

## 3. 缺陷模式档案（历史 bug → 三重锁映射）

> 新增行规则见 §5。每行 = 一类**已实际复发过或高危**的缺陷模式。
> 「变异」列编号对应 scripts/mutation-check.sh 清单（M-*），是本行锁存活的机器证明。

| # | 模式 | 症状（历史实证） | 根因要害 | 锁定用例 | 守卫 | 变异 |
|---|------|------------------|----------|----------|------|------|
| P-01 | 查询参数序列化漂移 | 列表翻页/加载更多恒回第一页（ADR-155，GUI 实测 20 行重复） | axios 默认 bracket 序列化，网关 decodeQuery 只认 JSON 风格 | clientParams.test.ts（4 例端到端） | G-04 锚点 | M1 |
| P-02 | 整模块 mock 假绿 | ADR-200 改上传响应形状无一测试报红；mock 缺具名导出、错误被 react-query 吞，数据从未加载仍全绿（ADR-203 实证） | `vi.mock('../api/client')` 绕过全部真实客户端行为 | （测试台纪律本身）fakeGateway 响亮失败 | **G-01** | — |
| P-03 | 页面手写响应形状 | `res.dir` 死链路存活三个版本；20 处 as-cast 臆造空间（ADR-203） | 形状锚定点扩散，tsc 无法在消费点报红 | clientContract.test.ts（形状锚定） | **G-02a**（死形状实证形态；as-cast 全量收敛为渐进纪律） | — |
| P-04 | 401 刷新风暴/递归 | 并发 401 各自刷新；刷新请求自身再触发拦截器 | 单飞 Promise 丢失；刷新误走 axios 实例 | client.test.ts（单飞+裸 fetch 由实现保证） | G-03b 锚点 | M2 |
| P-05 | WS/定时器泄漏 | 反复进出任务页连接累积，旧 onmessage 持续写缓存（2026-09-06 修复） | 卸载只置标志不 close | TaskDetailPage.test.tsx「卸载关闭」 | G-04a 锚点 | M4 |
| P-06 | 增量吸收重复/丢数 | 轮询与 WS 交叠日志重复行、AI 文本重复拼接 | log_id 去重与游标单调被改动 | TaskDetailSnapshot.test.tsx（交叠帧吸收） | — | M5 |
| P-07 | 事件处理死链路 | 结论筛选选任何具体结论恒被清空+filter 参数不契约被网关丢弃，整条链路从未生效（2026-09-06 修复） | onChange 分支死值 + 参数形状不契约 | FindingsPage.test.tsx（筛选两例+无 filter 参数） | — | M6 |
| P-08 | protojson int64 当 number | 日志时间 NaN（ADR-167） | ts_ms/next_cursor/total_bytes 是字符串 | TaskLogPanel.test.tsx；TaskDetailSnapshot 游标断言 | — | — |
| P-09 | 缓存失效键不配对 | 重新生成报告后摘要卡片仍旧值；triage 后列表行停留旧标签（ADR-152/2026-09-06） | invalidate 的 key 与写入 key 前缀不覆盖 | TaskDetailPage.test.tsx（task-reports 失效）；FindingDetail 断言 | — | — |
| P-10 | 死契约复活 | `['tasks-infinite']` 死键 invalidate（no-op）被复制回新代码（2026-09-06 清理） | 复制粘贴旧代码 | — | **G-02b** | — |
| P-11 | 滚底 ref 抢占 | Modal 开关后内联自动滚底永久失效（2026-09-06 修复） | 内联与 Modal 共享 boxRef | AIInteractionLogPanel.test.tsx（ref 归属） | — | M8 |
| P-12 | 二进制/多字节解码 | source_raw 中文乱码（2026-08-30 会话#41） | atob 当 UTF-8 用 | codeContext.test.tsx（中文原文断言） | — | — |
| P-13 | 下载扩展名恒定 | 报告下载恒 `.bin` 无法关联格式（2026-09-06 修复） | reportFileExt 丢失 | dict.test.ts | — | M7 |
| P-14 | 会话请求体不契约 | logout 空 body 恒 400 被清会话掩盖；register 邀请码空串误传 | 请求体形状无锚 | session.test.tsx（logout 携带 token / invite 剔除） | — | M9 / M10 |
| P-15 | 状态机镜像漂移 | 按钮可见性与后端权威脱节（如 RUNNING 丢暂停） | ALLOWED_ACTIONS 手改 | stateMachine.test.ts（全分支） | — | M3 |
| P-16 | 上传优先级矛盾态 | 「输入框置灰但列表有文件」，任务仍走 storage 通道（ADR-202） | onRemove 未同步清 file_id | TaskNewPage.test.tsx（移除恢复手填） | — | — |
| P-17 | 参数跨步丢失 | 创建 POST body 为 sast_tools:[]/config:{}，任务必然失败（ADR-154，GUI 实测） | Step 卸载注销 Form 字段、确认页 validateFields 取空 | TaskNewPage.test.tsx（请求体矩阵①②） | — | — |
| P-18 | 静默失败无反馈 | 创建失败用户停在确认页无任何提示（ADR-154） | mutation 无 onError | TaskNewPage/ProjectsPage 断言 + 响亮失败测试台 | — | — |
| P-19 | 客户端直连微服务 | 浏览器绕过同源容器直连网关/服务（破坏 14号 P1 拓扑与 nginx 安全边界） | 绝对 URL 进入 api/fetch 调用 | —（拓扑纪律） | **G-05** | — |
| P-20 | WS 断流观测空白 | 长任务非收束断线后面板空白直到重连/下一轮询拍（服务端游标已越过 WS pend 内容，只能经快照回填；gw-f6a3523 实证，116af13 修复） | onclose 只等 5s 重连不补拉；401 断线由单飞刷新自愈 token 竞态 | TaskDetailPage.test.tsx（「断线立即补拉」「AI 帧逐帧到达」两锁，116af13） | — | M12 |

## 4. 锁的三种形态（写法规范）

1. **契约锚定用例**：文件头注释标 `锚点: [E-nn]/[I-nn]`，用例名含契约号或行为语义
   （如「updateProjectConfig 双层包装——扁平体被 protojson 丢字段（E-16）」）。
   断言**形状本身**（方法/URL/包装层/字段域），不只断言"不抛错"。
2. **静态守卫条目**（guard.sh）：每条 = 一个可 grep 的正则 + 违例说明 + 对应档案行。
   守卫的锚点字符串**必须**在实际代码中存在（守卫自检，防锚点漂移假 PASS）。
3. **变异条目**（mutation-check.sh）：每条 = 唯一锚字符串的等价变异 + 目标测试文件。
   变异必须对应 §3 档案行；目标测试不红的变异 = 防线失效，脚本整体退出非零。

## 5. 缺陷修复工作流（防回归闭环——本机制的核心）

```
① 复现：先写红测试（能在未修复代码上失败），禁止不写复现直接修（engine test-gates §2 同规）。
② 修复：最小改动转绿；若要害属于 §3 某模式 → 更新该行锁定用例（如已有则强化）。
③ 建档：新缺陷模式 → §3 追加一行（模式/症状/根因/用例/守卫/变异四列按需）；
        需要 grep 禁区 → guard.sh 加 G-xx；可注入变异 → mutation-check.sh 加 M-xx。
④ 证明：npm run mutation-check（涉及防御要地时）确认新锁会红、旧锁未锈。
⑤ 记账：commit 信息写明「修复回归:…+变异 Mxx 锁定」（对齐 0ac71b7 风格）；
        跨会话协同见伞仓 .agent/status.md 回写。
```

**判据**：任何 bug 修复 PR，若其模式在 §3 中没有对应行、或对应行的变异跑不红，交付无效——
这就是"测试体系通过后不再犯同类错误"的机制化表达。

## 6. 维护注意

- guard.sh / mutation-check.sh 与 §3 档案**必须同 commit 演进**（改测试名/移动文件时同步）；
- 变异锚字符串要求在源码中唯一（脚本用 perl 精确替换，多命中会报错退出）；
- 新增测试文件默认被 vitest 发现（`src/__tests__/*.test.*`），被 tsc strict 检查（tsconfig include src）；
- 假绿检验的人类通道仍保留：手工还原修复观察全红（2026-09-06 六缺陷会话先例），
  mutation-check 只是把该纪律对存量锁自动化。

# CodeAudit GUI 人工测试指引(模拟栈口径)

> 2026-09-05 自 engine/.agent/MANUAL_TEST_GUIDE.md 迁移重写(原文件为宿主机 dev 裸栈时代
> 文档,随裸栈退役作废,ADR-207 归档)。本文口径=**生产模拟栈**(deploy/README 三层环境之②)。
> **生产栈同流程可用**:入口换 http://<服务器IP>:8088(console)/:8090(网关),2026-09-05 已在
> LXC 107 生产栈按本流程 GUI 实测通过(登录/建项目+上传/自动任务全链/模式B向导/发现/报告/通知;
> 证据=伞仓 .agent/evidence/gui-20260905/)。AI 全链需网关侧推理 provider 已配置——未配置时
> 任务在 AI 阶段走诚实降级(RuleScan 兜底,发现标注 NEEDS_MANUAL),属设计行为非缺陷。
> 依据:ADR-168(bridge 通道+AI 交互日志)、ADR-181(时间线/人性化渲染)、ADR-173/175(沙箱 DSH)。

## 0. 前置条件

| 依赖 | 核对方式 | 预期 |
|---|---|---|
| 模拟栈已起 | 伞仓 `make deploy-sim`(或 `bash deploy/sim.sh status`) | gateway 健康(18080)、console 健康(18088) |
| openshell-manager(LXC 107) | `curl http://gateway.internal:18800/healthz` | `{"ok": true}` |
| DSH 沙箱镜像 | `pct exec 107 -- docker images \| grep dsh-pentest-sse` | `dsh-pentest-sse:latest` 在列 |
| 推理路由(网关侧) | `curl -H "Authorization: Bearer $(cut -d= -f2 manager/deploy/env)" "http://gateway.internal:18800/api/v1/inference/route?workspace=default"` | 有 provider/model |

**推理 provider 配置方法**（2026-09-05 智谱 BigModel 实证；provider 存网关容器
`/var/lib/openshell/gateway.db`——清空即丢，须重配）：

```bash
TOK=$(cut -d= -f2 manager/deploy/env); BASE="http://gateway.internal:18800/api/v1"
# 关键：base_url 的 /v1/../ 前缀绕过网关对 openai 型 provider 追加的 /v1 段
# （网关拼接规则=base+/v1/chat/completions，../ 由服务器规范化归位到真实路径）
curl -X PUT "$BASE/inference/providers" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{
 "workspace":"default","name":"zhipu-bigmodel","type":"openai",
 "credentials":{"OPENAI_API_KEY":"<智谱key>"},
 "config":{"OPENAI_BASE_URL":"https://open.bigmodel.cn/v1/../../api/coding/paas/v4"}}'
curl -X PUT "$BASE/inference/route" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{
 "workspace":"default","provider":"zhipu-bigmodel","model":"glm-5.3-flash"}'
```

注意：①provider type 不可变更，换 type=删了重建；②智谱 openai 直拼 404 的根因
是网关写死追加 /v1（官方 anthropic 兼容端点存在但经网关协议转换会被 400 拒，
勿用）；③模型用编码计划的 glm-5.3-flash。

模拟栈为 PG 持久存储(与退役裸栈的内存存储不同):`make down-sim` 保卷、`make destroy-sim` 才清数据。

## 1. 访问入口

- 控制台:http://localhost:18088(生产 8090 网关同理,console 由 CD 单独发布)
- 网关 API:http://localhost:18080(/v1/*,JWT Bearer)
- 登录:`admin` / `admin`

## 2. GUI 全流程(约 3 分钟 + AI 审计 ~2 分钟)

1. **登录** → 项目页。
2. **新建项目**:填名称 → 上传 zip/tar.gz 代码包(≤25MB;可用 engine `tests/samples/python_flask`
   打包,内含注毒样例必出发现)→ toast 三连(项目已创建/已关联代码目录/已自动创建扫描任务)。
3. **任务 → 新建任务**:选项目 → 选模式(**模式A 纯AI** 或 **模式B SAST→AI 增强**——两者的
   4a/4b 现均走沙箱审查,都有 AI 交互日志)→ 勾选"创建后立即启动" → 创建。
4. **运行中观察**(任务详情页,WS 秒级推送;断网自动回退 3s 轮询):
   - 执行日志卡:沙箱创建(image=dsh-pentest-sse:latest)→ 就绪 → bridge 已就绪(maxTokens=32768,
     ADR-193 口径)→ bridge 已暴露(URL 形如 `http://default--am-xxxx--bridge.openshell.internal:8080/`)
     → 提交审计任务;
   - AI 交互日志卡(ADR-181):默认折叠,展开为整页时间线(加载更早 400 条/显示全部);中文人性化
     交互流——任务下发全文、思考流、输出、子任务骨架/回报、回合结束;徽标"实时接收中"→收束后
     "已收束"+KB 定格;原始 SSE 帧由 dsh-runtime 落盘(engine `data/ai-interaction/`)供机器调试;
   - 阶段时间线卡:阶段实时流转,不再静止到结束统一盖章。
5. **终态核验**(约 2 分钟):发现页签应有数条发现,结论列带 AI 输出徽标(悬停看判定理由);
   AI 交互日志"下载完整日志"尾部应为 `── 回合结束: completed ──` → `■ 会话空闲(收束)`;
   (可选)报告页判断的发现总数应与发现列表一致。

## 3. API 通道复核(可选)

```bash
BASE=http://localhost:18080
TOK=$(curl -s -X POST $BASE/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['access_token'])")
curl -s "$BASE/v1/tasks" -H "Authorization: Bearer $TOK" | python3 -m json.tool | head
curl -s "$BASE/v1/tasks/<taskID>/ai-log" -H "Authorization: Bearer $TOK" \
  | python3 -c "import sys,json,base64; d=json.load(sys.stdin); \
    print('total:', d['total_bytes'], 'complete:', d['complete']); \
    print(base64.b64decode(d['chunk']).decode('utf-8','replace')[:800])"
# 注意:/v1/findings?task_id=,不是 /v1/tasks/{id}/findings
curl -s "$BASE/v1/findings?task_id=<taskID>" -H "Authorization: Bearer $TOK" | python3 -m json.tool | head
```

## 4. 常见问题

| 现象 | 原因 | 处置 |
|---|---|---|
| 任务失败/发现为 0,执行日志见"沙箱创建失败" | 107 镜像不在 / manager 不可达 | 核对 §0;镜像重建 `bash dsh-pentest-sse/deploy.sh`(伞仓根) |
| 模式B 无 AI 交互日志 | 异常(旧口径已废——4a/4b 均走沙箱) | 按"深度排查"处理;ai-inference 已删(ADR-175),无"直连无日志"的合法情形 |
| AI 日志不增长,见 max_tokens 报错 | 推理路由 provider 变更且上限低于配置 | 经 manager inference 路由 API 切换/调上限 |
| 429 rate limit | 限流 50 req/min(按登录令牌,07 §7) | 关多余重度标签页或等 60s |
| 控制台行为像旧版本 | console 容器是构建产物 | 伞仓 web/ 子仓 `npm run build` 后 `make deploy-sim` 重建 console |
| 重启后数据还在/丢了? | 模拟栈 PG 数据在卷 | `down-sim` 保卷重启数据仍在;`destroy-sim` 清卷才丢 |
| 深度排查 | 容器日志 | `make logs-sim <svc>`(gateway/task/dsh-runtime/sast-adapter) |

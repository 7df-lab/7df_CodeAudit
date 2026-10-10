package handler

// 任务详情 WebSocket 实时推送。
//   - ADR-172: GET /v1/tasks/{id}/ws?token=<JWT>，帧 JSON 与 snapshot 完全同构
//     （task/progress/logs/ai，snake_case）+ "type":"snapshot" 包头——前端零字段迁移；
//   - ADR-188: 轮询节拍 1s→250ms（延迟真因是上游一元 RPC 的轮询节拍，非 WS 本身）；
//   - ADR-189: 上游 proto 新增 StreamTaskSnapshot / StreamAIInteractionLog 订阅流，
//     网关优先走流式订阅（数据到达即推帧，50ms 合并窗口防 token 流风暴），流式
//     不可用（旧二进制/连接失败/中途断流且未收束）自动回退轮询聚合（pollWatch，
//     ADR-172/188 语义）。
//
// 通用口径：游标在连接内累进（logs_after/ai_cursor），有变化才推帧；任务终态且
// AI 日志收束 → 推最终帧后服务端主动关闭（连接寿命=任务活跃期）；静默期 ping
// 保活（浏览器自动 pong），pong 超时或写失败即拆连接。
//
// 鉴权特例：浏览器 WebSocket API 无法自定义 Authorization 头，升级请求允许以
// token=<JWT> 查询参数携带（middleware/jwt.go 仅对 Upgrade: websocket 放行）。

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/codeaudit/proto-gen"
)

const (
	// pollWatchInterval — 回退轮询节拍（ADR-188；流式在线时本间隔不生效）。
	pollWatchInterval = 250 * time.Millisecond
	// pollMaxTransientTicks — ADR-213: 轮询路瞬时错误容忍拍数（约 2s）；超限才拆线
	pollMaxTransientTicks = 8
	wsFlushInterval       = 50 * time.Millisecond // 流式推帧合并窗口（防 token 流风暴逐帧刷）
	wsWriteTimeout        = 5 * time.Second       // 单帧写超时（超时视为对端失联）
	// wsPingEvery — 无条件保活 ping 周期（ADR-189 修复：此前仅在"静默期"ping，持续
	// 推流期间反而不 ping → 客户端无 pong → 读限期 90s 必然拆线；改无条件周期 ping，
	// 自动 pong 的对端（浏览器/python）以此证明存活并续期）。
	wsPingEvery     = 20 * time.Second
	wsReadIdleLimit = 90 * time.Second // 未收到任何客户端帧（含 pong）即拆线
	// 连接硬上限（防极端泄漏的最后防线）：读限 90s+无条件 ping/pong 已保证半开连接
	// 必被拆除，前端对其他关闭路径自带 5s 重连续订。gw-f6a3523 实证：32.5 分钟的
	// AI 审计撞上 30min 旧值 → 强制重连窗口与 30min access token TTL 竞态，长任务
	// 观测页中途断流。上调至 6h（> 最长审计任务），仅作泄漏兜底而非活性机制。
	wsMaxLifetime = 6 * time.Hour
	// aiExpectGrace — ADR-189：任务终态后 AI 侧收束宽限窗（在途分帧的到达余量）。
	// 该窗过后仍无任何 AI 字节 → 判定"该任务不会有 AI 交互日志"（终态前 AI 阶段
	// 未产出任何内容，最终日志=空，complete=true 是如实陈述而非猜测）。
	aiExpectGrace = 3 * time.Second
	// aiRearmDelay — R132（TP20-T3）。依据: ADR-254 §决策2（量级论证：纪元间隔=
	// 编排三段转换毫秒级，1-2 周期接上；等待期每周期一次廉价 RPC）：AI 流收束（complete 终帧）而任务未终态时，
	// 距重开上游订阅的延时。多纪元任务（pause/resume、切换 handoff-retry、
	// auto-retry）每纪元 Run 收束都会发终帧关流；旧实现 aiCh=nil 永久阻塞=纪元
	// N+1 字节永不转发（ui_check 切换全路径实证面板冻结）。延时取 2s：纪元间隔
	// （编排三段转换）毫秒级，1-2 个重臂周期即接上；等待期无新字节时每周期仅
	// 一次廉价 RPC（服务端零字节即 parked，纯元数据帧不轰炸客户端）。
	aiRearmDelay = 2 * time.Second
)

// wsIdleFrameEvery — TP20 缺损批 A3：静默期保活数据帧节拍。依据:
// ADR-189 的 20s ping 是 WS 控制帧——浏览器 JS 层不可见，客户端看门狗（45s 无
// 数据帧即拆线）在 TURN 方言整回合静默（实证 350s+）/长挂起期误杀重连循环。
// 本帧以 30s 节拍独立 ticker 补发当前快照（内容无增量，游标幂等）：既喂看门狗，
// 也证明服务端→网关→客户端全链存活（ping 只证 TCP）。var 而非 const=单测可缩拍。
var wsIdleFrameEvery = 30 * time.Second

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 64 * 1024, // 首帧携带全量日志/AI 正文，避免多帧拆装
	// 同源校验：控制台与网关同源经代理访问，跨源场景鉴权已由 JWT 保证
	CheckOrigin: func(*http.Request) bool { return true },
}

// watchCursors — 单连接内的增量游标与推送状态（推流循环协程访问，无需加锁）。
type watchCursors struct {
	logsAfter   string
	aiCursor    int64
	nodeCursors map[string]int64 // graph-ui-channels 批2：逐节点通道游标（连接内累进）
	nodeTotals  map[string]int64 // 批2 审查 P2-2：轮询路收束排干校验（游标≥total 才 settled）
	lastTask    *pb.ScanTask
	lastProg    *pb.TaskProgress
	first       bool // 尚未推过任何帧（首帧必推，携带全量）
	// ADR-189 流式路径：待推增量（flush 后清空）
	pendLogs *pb.GetTaskLogsResponse
	pendAI   *pb.GetAIInteractionLogResponse
}

// taskWatch — GET /v1/tasks/{id}/ws?token=&logs_after=&ai_cursor=
// 可选 logs_after/ai_cursor 为前端已吸收位置（轮询先行/重连场景），服务端游标自此
// 起算，首帧即增量、不重发已见内容。
func (t *Transcoder) taskWatch(w http.ResponseWriter, r *http.Request, taskID string) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade 已写 HTTP 错误响应
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		// 读循环：浏览器对 ping 自动 pong（顺带重置读限期）；数据帧一律丢弃（单向推
		// 送），但同样续期——持续推流期间客户端虽然不发业务帧，任何入站帧都证明对端
		// 存活。读限期超时/对端关闭 → cancel 结束推流循环。
		defer cancel()
		_ = conn.SetReadDeadline(time.Now().Add(wsReadIdleLimit))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(wsReadIdleLimit))
		})
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(wsReadIdleLimit))
		}
	}()

	cur := &watchCursors{first: true}
	if v := r.URL.Query().Get("logs_after"); v != "" {
		cur.logsAfter = v
	}
	if v := r.URL.Query().Get("ai_cursor"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			cur.aiCursor = n
		}
	}
	if nc := parseNodeCursors(r.URL.Query()["node_cursor"]); len(nc) > 0 {
		cur.nodeCursors = nc // graph-ui-channels 批2：重连续订位（节点缺省=0）
	}
	cur.nodeTotals = map[string]int64{}
	writeClose := func(code int, msg string) {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, msg), time.Now().Add(wsWriteTimeout))
	}

	// ADR-189：流式订阅优先；不可用（旧二进制/连接失败/中途断流且未收束）回退轮询。
	if t.streamWatch(ctx, conn, taskID, cur, writeClose) {
		return
	}
	t.pollWatch(ctx, conn, taskID, cur, writeClose)
}

// pushFrame — 组帧并推送（流式/轮询两路共用）：帧=当前任务态+进度+待推增量。
func (t *Transcoder) pushFrame(conn *websocket.Conn, cur *watchCursors) error {
	task := cur.lastTask
	if task == nil {
		task = &pb.ScanTask{}
	}
	logs := cur.pendLogs
	if logs == nil {
		logs = &pb.GetTaskLogsResponse{}
	}
	ai := cur.pendAI
	if ai == nil {
		ai = &pb.GetAIInteractionLogResponse{}
	}
	marshal := func(m proto.Message) json.RawMessage {
		b, err := (protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}).Marshal(m)
		if err != nil {
			return json.RawMessage("null")
		}
		return b
	}
	frame := map[string]json.RawMessage{
		"type": json.RawMessage(`"snapshot"`),
		"task": marshal(task), "progress": marshal(progOrEmpty(cur.lastProg)),
		"logs": marshal(logs), "ai": marshal(ai),
	}
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return conn.WriteJSON(frame)
}

// streamWatch — ADR-189 流式订阅主路径。返回 true=本路径已终结连接（正常收束/
// 客户端断开/写失败/寿命上限）；false=流式不可用（调用方回退 pollWatch，cur 保留
// 已吸收状态续跑，游标不重不漏）。
func (t *Transcoder) streamWatch(ctx context.Context, conn *websocket.Conn, taskID string, cur *watchCursors, writeClose func(int, string)) bool {
	// ADR-213: 流生命周期挂独立 ctx——本函数任何退出路径（回退轮询/收束/客户端
	// 断开）都撤泵，泵的 Recv 立即解锁退出。原实现流随 ctx 之外被弃置，
	// Recv 挂死的泵 goroutine 与上游流逐次泄漏。
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	// R132（TP20-T3）：AI 订阅可重臂——多纪元任务每纪元收束都关流（runtime 侧
	// complete 终帧语义），非终态下必须重开才能续接纪元 N+1。armAI 从当前游标
	// （含通道游标）重订阅；失败沿用既有回退（轮询兜底）。
	var taskEnded, aiEnded, sawAIComplete bool
	var aiCh chan *pb.GetAIInteractionLogResponse // nil=未订阅（select case 永久阻塞）
	armAI := func() bool {
		c, cerr := pb.NewRuntimeServiceClient(t.dshConn).StreamAIInteractionLog(streamCtx, &pb.StreamAIInteractionLogRequest{
			TaskId: taskID, Cursor: cur.aiCursor, NodeCursors: cur.nodeCursors,
		})
		if cerr != nil {
			return false
		}
		ch := make(chan *pb.GetAIInteractionLogResponse, 256)
		aiCh = ch
		aiEnded = false
		go func() {
			defer close(ch)
			for {
				a, err := c.Recv()
				if err != nil {
					return
				}
				select {
				case ch <- a:
				case <-streamCtx.Done():
					return
				}
			}
		}()
		return true
	}
	if t.dshConn != nil {
		if !armAI() {
			cancelStream()
			return false // AI 流订阅失败：整路回退轮询（AI 收束判定依赖该流）
		}
	}

	tsc, err := pb.NewTaskServiceClient(t.taskConn).StreamTaskSnapshot(streamCtx, &pb.StreamTaskSnapshotRequest{
		TaskId: taskID, LogsAfter: cur.logsAfter,
	})
	if err != nil {
		cancelStream()
		return false // 旧二进制（Unimplemented）或 task-service 不可达
	}

	taskCh := make(chan *pb.TaskSnapshotDelta, 32)
	go func() {
		defer close(taskCh)
		for {
			d, err := tsc.Recv()
			if err != nil {
				return
			}
			select {
			case taskCh <- d:
			case <-streamCtx.Done(): // ADR-213: 消费端退出后缓冲打满不再挂死泵
				return
			}
		}
	}()
	// t.dshConn==nil：不订阅 AI 流（aiCh 恒 nil），aiSettled 按 true（与轮询路同口径）。

	var terminalFirst time.Time
	absorbTask := func(d *pb.TaskSnapshotDelta) {
		if d.GetTask() != nil {
			cur.lastTask = d.GetTask()
		}
		if d.GetProgress() != nil {
			cur.lastProg = d.GetProgress()
		}
		if ls := d.GetLogs(); len(ls.GetLogs()) > 0 {
			if cur.pendLogs == nil {
				cur.pendLogs = &pb.GetTaskLogsResponse{}
			}
			cur.pendLogs.Logs = append(cur.pendLogs.Logs, ls.GetLogs()...)
			cur.logsAfter = ls.GetLogs()[len(ls.GetLogs())-1].GetLogId()
		}
	}
	absorbAI := func(a *pb.GetAIInteractionLogResponse) {
		if cur.pendAI == nil {
			cur.pendAI = &pb.GetAIInteractionLogResponse{}
		}
		if len(a.GetChunk()) > 0 {
			cur.pendAI.Chunk = append(cur.pendAI.Chunk, a.GetChunk()...)
			cur.aiCursor = a.GetNextCursor()
		}
		if a.GetTotalBytes() > 0 {
			// complete 终帧 chunk 为空但 total 仍是权威值（此前被跳过导致收束帧 total 归零）
			cur.pendAI.TotalBytes = a.GetTotalBytes()
		}
		cur.pendAI.NextCursor = a.GetNextCursor()
		mergeAIChannels(cur.pendAI, a.GetChannels(), cur.nodeCursors) // 批2：通道增量并入待推面
		// R132：complete 逐帧如实（原只在 true 时置位永不回落——纪元 N 终帧后
		// 纪元 N+1 续写帧被谎报 complete，前端"已收束"横幅跨纪元失真）。
		cur.pendAI.Complete = a.GetComplete()
		if a.GetComplete() {
			sawAIComplete = true
		}
	}
	taskSettled := func() bool { return cur.lastTask != nil && isTerminalScanTask(cur.lastTask) }
	// aiNeverExpected — 该任务不会有任何 AI 交互日志（ADR-189，诚实收束判定）：
	// ①纯 SAST 模式无 AI 阶段；②任务已终态、宽限窗过后仍零 AI 字节（终态前 AI
	// 未产出任何内容——FAILED/DEAD 于 AI 阶段开始前的任务属此类）。
	neverExpected := func(terminalFirst time.Time) bool {
		if cur.lastTask == nil {
			return false
		}
		if cur.lastTask.GetScanMode() == pb.ScanMode_SCAN_MODE_SAST_ONLY {
			return true
		}
		return cur.aiCursor == 0 && !terminalFirst.IsZero() && time.Since(terminalFirst) >= aiExpectGrace
	}
	aiSettled := func(terminalFirst time.Time) bool {
		return t.dshConn == nil || sawAIComplete || neverExpected(terminalFirst)
	}

	lifetime := time.Now().Add(wsMaxLifetime)
	flushC := time.NewTimer(wsFlushInterval)
	defer flushC.Stop()
	keepalive := time.NewTicker(wsPingEvery) // ADR-189：无条件周期 ping（对端 pong 续读限期）
	defer keepalive.Stop()
	// R132：AI 重臂计时器（收束关流而任务未终态时排程；未排程的到点忽略）。
	rearmC := time.NewTimer(time.Hour)
	defer rearmC.Stop()
	rearmPending := false
	lastPushAt := time.Now() // A3：静默保活帧节拍基准（push 处更新）
	// A3：静默保活帧独立 ticker（与 ping 拍解耦——ping 只证 TCP，本帧喂对端
	// 数据帧看门狗+证全链存活；内容无增量，客户端游标幂等吸收）。
	idleC := time.NewTicker(wsIdleFrameEvery)
	defer idleC.Stop()
	armFlush := func() {
		if !flushC.Stop() {
			select {
			case <-flushC.C:
			default:
			}
		}
		flushC.Reset(wsFlushInterval)
	}
	push := func() bool { // 推帧；失败=对端失联（连接由 defer Close 兜底）
		if err := t.pushFrame(conn, cur); err != nil {
			return false
		}
		lastPushAt = time.Now()
		cur.pendLogs = nil
		if cur.pendAI != nil && sawAIComplete {
			// R132：complete 终帧的**旗面**（complete/total）保留至终态收束的最终帧
			// （原整对象保留——正文面 chunk 随纪元 N+1 重臂续写被重复 append=已送
			// 字节重发，客户端游标推进后追加重复文本）。
			cur.pendAI = &pb.GetAIInteractionLogResponse{
				Complete: cur.pendAI.GetComplete(), TotalBytes: cur.pendAI.GetTotalBytes(),
			}
		} else {
			cur.pendAI = nil
		}
		cur.first = false
		return true
	}
	dirty := false

	for {
		select {
		case d, ok := <-taskCh:
			if !ok {
				taskEnded = true
				taskCh = nil // nil 通道使本 case 永久阻塞（后续不再触发）
				continue
			}
			absorbTask(d)
			dirty = true
			armFlush()
		case a, ok := <-aiCh:
			if !ok {
				aiEnded = true
				aiCh = nil
				// R132：收束关流而任务未终态=纪元边界（多纪元任务常态）——排程重臂；
				// 未 complete 的断流不在此列（落入下方 ADR-213 回退分支）。
				if sawAIComplete && !taskSettled() {
					rearmPending = true
					rearmC.Reset(aiRearmDelay)
				}

				continue
			}
			absorbAI(a)
			dirty = true
			armFlush()
		case <-rearmC.C:
			if !rearmPending {
				continue // 未排程的到点（终态已收束等）：忽略
			}
			rearmPending = false
			if sawAIComplete && !taskSettled() && !armAI() {
				// ADR-213: 回退前冲刷（同下——游标已越过，不冲即丢）
				if dirty {
					if !push() {
						return true
					}
					dirty = false
				}
				return false // 重订阅失败：轮询兜底续跑
			}
			continue
		case <-flushC.C:
			if dirty {
				dirty = false
				if !push() {
					return true
				}
			}
		case <-keepalive.C:
			if time.Now().After(lifetime) {
				writeClose(websocket.CloseNormalClosure, "watch lifetime exceeded")
				return true
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return true
			}
		case <-idleC.C:
			if !dirty && time.Since(lastPushAt) >= wsIdleFrameEvery {
				// F2：保活帧不得携带 pendAI 保留旗面（上一次
				// 真帧的 complete=true/旧 total）——否则静默期客户端 meta.complete 闪真
				// （「已收束」标签/思考归档折叠抖动，TURN 静默帧日志实证）。帧省略 ai 键：
				// onmessage 仍触发（喂看门狗），A2 守护跳过无信号帧。旗面恢复供终帧携带。
				saved := cur.pendAI
				cur.pendAI = nil
				ok := push()
				cur.pendAI = saved
				if !ok {
					return true
				}
			}
		case <-ctx.Done():
			return true
		}

		// 收束判定：任务流已 ended 才看终态（避免与 settled 帧竞态误判）
		if taskEnded {
			if taskSettled() && terminalFirst.IsZero() {
				terminalFirst = time.Now() // 宽限窗起点（在途 AI 分帧到达余量）
			}
			// A5：收束三条件=任务终态∧AI 收束∧主面游标已排干——
			// sawAIComplete 跨纪元残留（纪元1 已真后纪元2 在途）时终态即关=末纪元
			// 尾部截断（客户端 onclose 快照兜底不雅）。
			aiDrained := cur.pendAI == nil || cur.pendAI.GetTotalBytes() == 0 ||
				cur.aiCursor >= cur.pendAI.GetTotalBytes()
			unsettled := !taskSettled() || !aiSettled(terminalFirst) || !aiDrained
			if unsettled && taskSettled() && !terminalFirst.IsZero() {
				// 宽限窗计时中：终态帧已到、等 AI 侧收束/排干/宽限到点。A5：有界
				// 等待后**原地重判**——taskCh/aiCh 双静默期 select 无唤醒源，continue
				// 回 select 会 park 到 keepalive(20s)（宽限 4s 语义失效，三连跑实证）。
				if remaining := aiExpectGrace + time.Second - time.Since(terminalFirst); remaining > 0 {
					select {
					case <-time.After(remaining):
					case <-ctx.Done():
						return true
					}
					aiDrained = cur.pendAI == nil || cur.pendAI.GetTotalBytes() == 0 ||
						cur.aiCursor >= cur.pendAI.GetTotalBytes()
					unsettled = !taskSettled() || !aiSettled(terminalFirst) || !aiDrained
				}
			}
			if unsettled {
				// ADR-213: 回退前冲刷——游标已越过 pend 内容，轮询路取不到，不冲即丢
				// （违反"游标不重不漏"；丢的恰是流断前一窗，多为最关键的报错行）
				if dirty {
					if !push() {
						return true
					}
					dirty = false
				}
				return false // 断流未收束（含未排干/AI 流断而未 complete）→ 轮询兜底续跑
			}
			if cur.pendAI == nil {
				cur.pendAI = &pb.GetAIInteractionLogResponse{} // 无 AI 内容：帧内如实置 complete
			}
			cur.pendAI.Complete = true
			if !push() { // 最终帧（task 终态 + ai complete 全量态）
				return true
			}
			writeClose(websocket.CloseNormalClosure, "task settled")
			return true
		}
		if t.dshConn != nil && aiEnded && !sawAIComplete && aiCh == nil { // ADR-213: 无 DSH（dshConn==nil）不构成"AI 流断"——原条件恒真使 SAST-only 部署的流式路成死路径；R132: aiCh==nil 排除重臂在途态
			// ADR-213: 回退前冲刷（同上——游标已越过，不冲即丢）
			if dirty {
				if !push() {
					return true
				}
				dirty = false
			}
			return false // AI 流断而未 complete：任务侧流式虽好，收束判定交轮询兜底
		}
	}
}

// pollWatch — ADR-172/188 轮询聚合回退路径（流式不可用时）：每 pollWatchInterval
// 聚合拉取四个只读 gRPC（内部直连，不经边缘限流），变化才推帧。
func (t *Transcoder) pollWatch(ctx context.Context, conn *websocket.Conn, taskID string, cur *watchCursors, writeClose func(int, string)) {
	lifetime := time.Now().Add(wsMaxLifetime)
	keepalive := time.NewTicker(wsPingEvery) // ADR-189：无条件周期 ping（对端 pong 续读限期）
	defer keepalive.Stop()
	var terminalFirst time.Time
	transientTicks := 0 // ADR-213: 连续瞬时错误计数（成功一拍即清零）
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(lifetime) {
			writeClose(websocket.CloseNormalClosure, "watch lifetime exceeded")
			return
		}

		tickCtx, tickCancel := context.WithTimeout(ctx, t.callTimeout)
		frame, terminal, aiDone, err := t.aggregateWatchFrame(tickCtx, taskID, cur)
		tickCancel()
		if err != nil {
			// 确定性错误：如实告知后拆线（前端回退轮询呈错误终态）
			switch status.Convert(err).Code() {
			case codes.NotFound, codes.InvalidArgument, codes.PermissionDenied:
				writeClose(websocket.CloseInternalServerErr, status.Convert(err).Message())
				return
			}
			// ADR-213: 瞬时错误（后端重启/慢拍）容忍连续 N 拍——游标未动，下拍
			// 幂等重拉；原实现任何错误即 1011，后端一次抖动=全员同时断线+重连风暴
			transientTicks++
			if transientTicks > pollMaxTransientTicks {
				writeClose(websocket.CloseInternalServerErr, "watch unstable: "+status.Convert(err).Message())
				return
			}
			frame, terminal, aiDone = nil, false, false
		} else {
			transientTicks = 0
		}
		// ADR-189 推断收束（与流式路同口径）：该任务不会有 AI 交互日志时不再等
		// complete（纯 SAST 无 AI 阶段；终态后宽限窗仍零 AI 字节=最终日志为空）。
		// 仅在推断翻转 aiDone 时补推 complete 帧（aggregate 本身已收束则帧已携带）。
		inferred := false
		if terminal && !aiDone && cur.lastTask != nil {
			if cur.lastTask.GetScanMode() == pb.ScanMode_SCAN_MODE_SAST_ONLY {
				aiDone, inferred = true, true
			} else if cur.aiCursor == 0 {
				if terminalFirst.IsZero() {
					terminalFirst = time.Now()
				} else if time.Since(terminalFirst) >= aiExpectGrace {
					aiDone, inferred = true, true
				}
			}
		}
		if frame != nil {
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteJSON(frame); err != nil {
				return
			}
		}
		if terminal && aiDone {
			if inferred {
				// 补推 ai.complete=true 的最终帧（前端据此定格"已收束"，不再重连）
				if cur.pendAI == nil {
					cur.pendAI = &pb.GetAIInteractionLogResponse{}
				}
				cur.pendAI.Complete = true
				_ = t.pushFrame(conn, cur)
			}
			writeClose(websocket.CloseNormalClosure, "task settled")
			return
		}
		select {
		case <-keepalive.C:
			if time.Now().After(lifetime) {
				writeClose(websocket.CloseNormalClosure, "watch lifetime exceeded")
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-time.After(pollWatchInterval):
		}
	}
}

// aggregateWatchFrame — 一个 tick 的四路聚合拉取（并发，超时受 tick ctx 约束）。
// 返回 nil 帧 = 无变化不推。终态判定取自 task 结构；AI 收束取自 GetAIInteractionLog，
// dsh 侧不可达时按 true 处理（尽力而为：终态判定不被 AI 日志面挂死）。
// 帧结构与 ADR-170 snapshot 同构：{"type":"snapshot","task":..,"progress":..,"logs":..,"ai":..}
func (t *Transcoder) aggregateWatchFrame(ctx context.Context, taskID string, cur *watchCursors) (map[string]json.RawMessage, bool, bool, error) {
	var (
		wg      sync.WaitGroup
		task    *pb.ScanTask
		prog    *pb.TaskProgress
		taskErr error
		logs    *pb.GetTaskLogsResponse
		ai      *pb.GetAIInteractionLogResponse
	)
	marshal := func(m proto.Message) json.RawMessage {
		b, err := (protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}).Marshal(m)
		if err != nil {
			return json.RawMessage("null")
		}
		return b
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := pb.NewTaskServiceClient(t.taskConn).GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: taskID})
		if err != nil {
			taskErr = err
			return
		}
		task = resp
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if resp, err := pb.NewTaskServiceClient(t.taskConn).GetTaskProgress(ctx, &pb.GetTaskProgressRequest{TaskId: taskID}); err == nil {
			prog = resp
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := &pb.GetTaskLogsRequest{TaskId: taskID, Limit: 500}
		if cur.logsAfter != "" {
			req.AfterLogId = cur.logsAfter
		}
		if resp, err := pb.NewTaskServiceClient(t.taskConn).GetTaskLogs(ctx, req); err == nil {
			logs = resp
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if t.dshConn == nil {
			return
		}
		resp, err := pb.NewRuntimeServiceClient(t.dshConn).GetAIInteractionLog(ctx, &pb.GetAIInteractionLogRequest{
			TaskId: taskID, Cursor: cur.aiCursor, NodeCursors: cur.nodeCursors,
		})
		if err == nil {
			ai = resp
			// 批2：轮询路帧内即原响应——只累进游标，禁止自合并（审查 P1-1：
			// mergeAIChannels(dst=ai, chans=ai.Channels) 会把 chunk 自拼接翻倍）。
			for _, c := range ai.GetChannels() {
				cur.nodeCursors[c.GetNodeId()] = c.GetNextCursor()
				cur.nodeTotals[c.GetNodeId()] = c.GetTotalBytes()
			}
		}
	}()
	wg.Wait()
	if taskErr != nil {
		return nil, false, false, taskErr
	}

	// 变化检测：task/progress 结构比对 + 日志/AI 增量非空（首帧必推）
	taskChanged := !proto.Equal(task, cur.lastTask)
	progChanged := !proto.Equal(prog, cur.lastProg)
	newLogs := logs != nil && len(logs.GetLogs()) > 0
	newAI := ai != nil && (len(ai.GetChunk()) > 0 || len(ai.GetChannels()) > 0) // 批2：通道增量亦算变化
	if !cur.first && !taskChanged && !progChanged && !newLogs && !newAI {
		return nil, isTerminalScanTask(task), aiSettled(ai), nil
	}

	cur.first = false
	if taskChanged {
		cur.lastTask = task
	}
	if progChanged {
		cur.lastProg = prog
	}
	if newLogs {
		cur.logsAfter = logs.Logs[len(logs.Logs)-1].GetLogId()
	} else if logs == nil {
		logs = &pb.GetTaskLogsResponse{}
	}
	if newAI {
		cur.aiCursor = ai.GetNextCursor()
	} else if ai == nil {
		ai = &pb.GetAIInteractionLogResponse{Complete: true}
	}
	frame := map[string]json.RawMessage{
		"type": json.RawMessage(`"snapshot"`),
		"task": marshal(task), "progress": marshal(progOrEmpty(prog)),
		"logs": marshal(logs), "ai": marshal(ai),
	}
	// 批2 审查 P2-2：轮询路收束=主面 complete 且全通道排干（游标≥total）——
	// 与流式路 settled 口径一致；未排干则继续 tick（REST/下一拍补齐尾部）。
	channelsDrained := true
	for n, total := range cur.nodeTotals {
		if cur.nodeCursors[n] < total {
			channelsDrained = false
			break
		}
	}
	return frame, isTerminalScanTask(task), aiSettled(ai) && channelsDrained, nil
}

// progOrEmpty — progress 拉取失败时帧内给空对象（与 snapshot 单口"尽力而为"口径一致）
func progOrEmpty(p *pb.TaskProgress) proto.Message {
	if p == nil {
		return &pb.TaskProgress{}
	}
	return p
}

// isTerminalScanTask — 终态四值（与 statemachine.IsTerminal / 前端 isTerminal 同源口径）
func isTerminalScanTask(t *pb.ScanTask) bool {
	switch t.GetStatus() {
	case pb.TaskStatus_TASK_STATUS_COMPLETED, pb.TaskStatus_TASK_STATUS_CANCELLED,
		pb.TaskStatus_TASK_STATUS_TIMEOUT, pb.TaskStatus_TASK_STATUS_DEAD:
		return true
	}
	return false
}

// aiSettled — AI 日志收束判定：读口可达看 complete；不可达按已收束（终态关连接不被挂死）
func aiSettled(ai *pb.GetAIInteractionLogResponse) bool {
	return ai == nil || ai.GetComplete()
}

// parseNodeCursors — node_cursor 查询参数解析（graph-ui-channels 批2）。
// 形态：可重复参数，每值 "nodeID:cursor" 或逗号分隔多组（"n001:12,n002:34"）；
// 非法片段跳过（连接级尽力而为，节点缺省=0 从头）。
func parseNodeCursors(vals []string) map[string]int64 {
	var out map[string]int64
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			i := strings.LastIndexByte(part, ':')
			if i <= 0 || i == len(part)-1 {
				continue
			}
			n, err := strconv.ParseInt(part[i+1:], 10, 64)
			if err != nil || n < 0 {
				continue
			}
			if out == nil {
				out = map[string]int64{}
			}
			out[part[:i]] = n
		}
	}
	return out
}

// mergeAIChannels — 通道增量并入目标响应（同节点 chunk 追加、游标/total 取新；
// cursors 非-nil 时同步连接级游标）。批2：流式/轮询两路共用。
func mergeAIChannels(dst *pb.GetAIInteractionLogResponse, chans []*pb.AILogChannel, cursors map[string]int64) {
	for _, c := range chans {
		var tgt *pb.AILogChannel
		for _, d := range dst.Channels {
			if d.GetNodeId() == c.GetNodeId() {
				tgt = d
				break
			}
		}
		if tgt == nil {
			tgt = &pb.AILogChannel{NodeId: c.GetNodeId()}
			dst.Channels = append(dst.Channels, tgt)
		}
		tgt.Chunk = append(tgt.Chunk, c.GetChunk()...)
		tgt.NextCursor = c.GetNextCursor()
		tgt.TotalBytes = c.GetTotalBytes()
		if c.GetComplete() {
			tgt.Complete = true
		}
		if cursors != nil {
			cursors[c.GetNodeId()] = c.GetNextCursor()
		}
	}
}

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
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/codeaudit/proto-gen"
)

const (
	// pollWatchInterval — 回退轮询节拍（ADR-188；流式在线时本间隔不生效）。
	pollWatchInterval = 250 * time.Millisecond
	wsFlushInterval   = 50 * time.Millisecond // 流式推帧合并窗口（防 token 流风暴逐帧刷）
	wsWriteTimeout    = 5 * time.Second       // 单帧写超时（超时视为对端失联）
	// wsPingEvery — 无条件保活 ping 周期（ADR-189 修复：此前仅在"静默期"ping，持续
	// 推流期间反而不 ping → 客户端无 pong → 读限期 90s 必然拆线；改无条件周期 ping，
	// 自动 pong 的对端（浏览器/python）以此证明存活并续期）。
	wsPingEvery     = 20 * time.Second
	wsReadIdleLimit = 90 * time.Second // 未收到任何客户端帧（含 pong）即拆线
	wsMaxLifetime   = 30 * time.Minute // 连接硬上限：防半开连接泄漏，超限由前端重连续订
	// aiExpectGrace — ADR-189：任务终态后 AI 侧收束宽限窗（在途分帧的到达余量）。
	// 该窗过后仍无任何 AI 字节 → 判定"该任务不会有 AI 交互日志"（终态前 AI 阶段
	// 未产出任何内容，最终日志=空，complete=true 是如实陈述而非猜测）。
	aiExpectGrace = 3 * time.Second
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 64 * 1024, // 首帧携带全量日志/AI 正文，避免多帧拆装
	// 同源校验：控制台与网关同源经代理访问，跨源场景鉴权已由 JWT 保证
	CheckOrigin: func(*http.Request) bool { return true },
}

// watchCursors — 单连接内的增量游标与推送状态（推流循环协程访问，无需加锁）。
type watchCursors struct {
	logsAfter string
	aiCursor  int64
	lastTask  *pb.ScanTask
	lastProg  *pb.TaskProgress
	first     bool // 尚未推过任何帧（首帧必推，携带全量）
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
	tsc, err := pb.NewTaskServiceClient(t.taskConn).StreamTaskSnapshot(ctx, &pb.StreamTaskSnapshotRequest{
		TaskId: taskID, LogsAfter: cur.logsAfter,
	})
	if err != nil {
		return false // 旧二进制（Unimplemented）或 task-service 不可达
	}
	var ais pb.DSHRuntimeService_StreamAIInteractionLogClient
	if t.dshConn != nil {
		if c, cerr := pb.NewDSHRuntimeServiceClient(t.dshConn).StreamAIInteractionLog(ctx, &pb.StreamAIInteractionLogRequest{
			TaskId: taskID, Cursor: cur.aiCursor,
		}); cerr == nil {
			ais = c
		} else {
			return false // AI 流订阅失败：整路回退轮询（AI 收束判定依赖该流）
		}
	}

	taskCh := make(chan *pb.TaskSnapshotDelta, 32)
	aiCh := make(chan *pb.GetAIInteractionLogResponse, 256)
	go func() {
		defer close(taskCh)
		for {
			d, err := tsc.Recv()
			if err != nil {
				return
			}
			taskCh <- d
		}
	}()
	go func() {
		defer close(aiCh)
		if ais == nil {
			return // t.dshConn==nil：AI 收束判定按 true（与轮询路同口径），通道即闭
		}
		for {
			a, err := ais.Recv()
			if err != nil {
				return
			}
			aiCh <- a
		}
	}()

	var taskEnded, aiEnded, sawAIComplete bool
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
		if a.GetComplete() {
			cur.pendAI.Complete = true
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
		cur.pendLogs = nil
		if !sawAIComplete {
			cur.pendAI = nil // complete 帧保留到终态收束的最终帧
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
				continue
			}
			absorbAI(a)
			dirty = true
			armFlush()
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
		case <-ctx.Done():
			return true
		}

		// 收束判定：任务流已 ended 才看终态（避免与 settled 帧竞态误判）
		if taskEnded {
			if taskSettled() && terminalFirst.IsZero() {
				terminalFirst = time.Now() // 宽限窗起点（在途 AI 分帧到达余量）
			}
			if !taskSettled() || !aiSettled(terminalFirst) {
				if taskSettled() && !terminalFirst.IsZero() {
					// 宽限窗计时中：终态帧已到、等 AI 侧收束/宽限到点（保持循环）
					if time.Since(terminalFirst) < aiExpectGrace+time.Second {
						continue
					}
				}
				return false // 断流未收束（含 AI 流断而未 complete）→ 轮询兜底续跑
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
		if aiEnded && !sawAIComplete {
			return false // AI 流断而未 complete：任务侧流式虽好，收束判定交轮询兜底
		}
	}
}

// pollWatch — ADR-172/188 轮询聚合回退路径（流式不可用时）：每 pollWatchInterval
// 聚合拉取四个只读 gRPC（内网直连，不经边缘限流），变化才推帧。
func (t *Transcoder) pollWatch(ctx context.Context, conn *websocket.Conn, taskID string, cur *watchCursors, writeClose func(int, string)) {
	lifetime := time.Now().Add(wsMaxLifetime)
	keepalive := time.NewTicker(wsPingEvery) // ADR-189：无条件周期 ping（对端 pong 续读限期）
	defer keepalive.Stop()
	var terminalFirst time.Time
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
			// 任务不存在等确定性错误：如实告知后拆线（前端回退轮询呈错误终态）
			writeClose(websocket.CloseInternalServerErr, status.Convert(err).Message())
			return
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
		resp, err := pb.NewDSHRuntimeServiceClient(t.dshConn).GetAIInteractionLog(ctx, &pb.GetAIInteractionLogRequest{
			TaskId: taskID, Cursor: cur.aiCursor,
		})
		if err == nil {
			ai = resp
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
	newAI := ai != nil && len(ai.GetChunk()) > 0
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
	return frame, isTerminalScanTask(task), aiSettled(ai), nil
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

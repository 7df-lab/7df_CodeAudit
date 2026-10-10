// Package model 定义 baton 中立事件（.tasklog/events.jsonl 单行）与投影文件结构。
// 结构与 schema/event.schema.json、spine.schema.json、workspace.schema.json 一一对应；
// json tag 即线格式，新增字段必须先改 schema 再改此处（schema 是契约 SSOT）。
package model

import "encoding/json"

// Pen 持笔人：换 agent 只换 pen，task 不变（共识决策 #4）。
type Pen struct {
	Agent string  `json:"agent"`
	Model *string `json:"model,omitempty"`
}

// Part 消息片段：v0 仅 text/reasoning 两类（认识论分层在 spine，不在消息）。
type Part struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Message model.message 载荷。大正文走 PartsRef（blob），Parts 为空。
type Message struct {
	Role     string `json:"role"`
	Parts    []Part `json:"parts,omitempty"`
	PartsRef string `json:"parts_ref,omitempty"`
}

// Approval 权限决策——记在 tool.call 上，免第十族（schema 注记）。
type Approval struct {
	Decision string `json:"decision"` // granted | denied | preset
	Policy   string `json:"policy,omitempty"`
}

// Call tool.call 载荷。DSH tool/call 无 call_id，由投影器合成（会话+轮次+序号）。
type Call struct {
	CallID   string          `json:"call_id"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args,omitempty"`
	ArgsRef  string          `json:"args_ref,omitempty"`
	Approval *Approval       `json:"approval,omitempty"`
	Via      string          `json:"via,omitempty"`
}

// Result tool.result 载荷。Outcome=indeterminate 是公理落点：缺失结果≠没跑过。
type Result struct {
	CallID    string          `json:"call_id"`
	Ok        bool            `json:"ok"`
	Outcome   string          `json:"outcome"` // completed | error | denied | indeterminate
	Result    json.RawMessage `json:"result,omitempty"`
	ResultRef string          `json:"result_ref,omitempty"`
	Bytes     int             `json:"bytes,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Usage usage.recorded 载荷。字段对齐 dsh-runtime TokenUsage（camelCase）；
// InputTokens 仅未缓存输入，计费输入 = input + cache_read + cache_write。
type Usage struct {
	Model            string `json:"model"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
}

// GraphAudit — v0.5 认领层/人机共编辑审计载荷（五事件共用：
// node_claimed/released/lease_expired/reopened/human_message；各事件取子集）。
type GraphAudit struct {
	NodeID     string `json:"node_id,omitempty"`
	Holder     string `json:"holder,omitempty"`
	LeaseUntil string `json:"lease_until,omitempty"`
	Reason     string `json:"reason,omitempty"`
	By         string `json:"by,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Bytes      int    `json:"bytes,omitempty"`
}

// Subagent task.subagent 载荷（subagent.started/finished 帧）。
type Subagent struct {
	ParentSession string `json:"parent_session"`
	ChildSession  string `json:"child_session"`
	Phase         string `json:"phase"` // started | finished
	Status        string `json:"status,omitempty"`
	StopReason    string `json:"stop_reason,omitempty"`
}

// Compaction context.compacted 载荷（M4 换手/压缩记账）。
type Compaction struct {
	By             string `json:"by"`
	Dropped        string `json:"dropped,omitempty"`
	SummaryRef     string `json:"summary_ref,omitempty"`
	SpineUnchanged bool   `json:"spine_unchanged"`
	TokensBefore   int    `json:"tokens_before,omitempty"`
	TokensAfter    int    `json:"tokens_after,omitempty"`
}

// HandoffFromTo / 以下 Handoff* 为 handoff.* 载荷（M4 使用，schema 先行）。
type HandoffFromTo struct {
	Agent string  `json:"agent"`
	Model *string `json:"model,omitempty"`
}

type HandoffRequested struct {
	ByUser string        `json:"by_user"`
	From   HandoffFromTo `json:"from"`
	To     HandoffFromTo `json:"to"`
	Reason string        `json:"reason"`
}

type HandoffCheckpoint struct {
	LogSeq    int             `json:"log_seq"`
	Workspace WorkspaceMarker `json:"workspace"`
}

type WorkspaceMarker struct {
	GitAnchor string   `json:"git_anchor,omitempty"`
	TreeRef   string   `json:"tree_ref,omitempty"`
	Dirty     []string `json:"dirty,omitempty"`
}

type HandoffSeed struct {
	Via      string          `json:"via"` // prompt | AGENTS.md | CLAUDE.md | task-card
	BriefRef string          `json:"brief_ref"`
	Tail     HandoffSeedTail `json:"tail"` // schema SSOT：嵌套 tail{from_seq,to_seq}（additionalProperties:false）
}

type HandoffSeedTail struct {
	FromSeq int `json:"from_seq"`
	ToSeq   int `json:"to_seq"`
}

type HandoffDone struct {
	Fidelity string   `json:"fidelity"`
	Holes    []string `json:"holes,omitempty"`
}

// WorkspaceRef task.created 里的工作区锚。
type WorkspaceRef struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	GitAnchor   string `json:"git_anchor,omitempty"`
	TreeRef     string `json:"tree_ref,omitempty"`
}

// Fact / Decision / Spine —— 脊柱（spine.json 与 spine.updated 载荷同构）。
type Fact struct {
	Text     string `json:"text"`
	Kind     string `json:"kind,omitempty"`    // lesson | observation | progress（graph-reseed 共识；缺省=observation 语义）
	Session  string `json:"session,omitempty"` // 产生该事实的会话（如图节点 g-<nodeId>）
	Evidence []int  `json:"evidence,omitempty"`
}

type Decision struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"` // proposed | adopted | rejected
}

type Spine struct {
	Task         string     `json:"task"`
	UpdatedAtSeq int        `json:"updated_at_seq"`
	Goal         string     `json:"goal,omitempty"`
	Constraints  []string   `json:"constraints,omitempty"`
	Facts        []Fact     `json:"facts,omitempty"`
	Decisions    []Decision `json:"decisions,omitempty"`
	Open         []string   `json:"open,omitempty"`
	Hypothesis   string     `json:"hypothesis,omitempty"`
	Next         string     `json:"next,omitempty"`
}

// WorkspaceFile .tasklog/WORKSPACE.json —— 身份标记，不符即硬闸（Maka 第三边界廉价版）。
type WorkspaceFile struct {
	WorkspaceID       string  `json:"workspace_id"`
	Task              string  `json:"task"`
	CreatedTs         string  `json:"created_ts"`
	GitAnchor         string  `json:"git_anchor,omitempty"`
	ParentWorkspaceID *string `json:"parent_workspace_id"`
	Vcs               string  `json:"vcs,omitempty"`
}

// GraphNodeDeclared 等 graph.* 六载荷（切片 5+6 ADR-001；域中立 C4——字段只有
// 控制面词表，intent/resultRef 为一句话/摘要文本）。
type GraphNodeDeclared struct {
	NodeID     string   `json:"nodeId"`
	Type       string   `json:"type"` // planner | executor | verifier
	Intent     string   `json:"intent"`
	DeclaredBy string   `json:"declaredBy"` // pack | planner 节点 id
	From       []string `json:"from"`
}

type GraphNodeStarted struct {
	NodeID    string `json:"nodeId"`
	SessionID string `json:"sessionId"` // g-<nodeId>
	TurnRef   string `json:"turnRef"`
}

type GraphUsageEstimate struct {
	Turns float64 `json:"turns"`
}

type GraphNodeResult struct {
	NodeID        string              `json:"nodeId"`
	Outcome       string              `json:"outcome"` // completed | failed | rejected | indeterminate（D1 分道）
	ResultRef     string              `json:"resultRef"`
	UsageEstimate *GraphUsageEstimate `json:"usageEstimate,omitempty"`
}

type GraphPlanTriggered struct {
	Trigger string `json:"trigger"` // initial | new_evidence | open_drained
	Detail  string `json:"detail"`
}

type GraphPaused struct {
	Reason      string   `json:"reason"` // handoff | crash_recover | task_pause
	OpenNodeIDs []string `json:"openNodeIds"`
}

type GraphConverged struct {
	Summary       string `json:"summary"`
	OpenRemaining int    `json:"openRemaining"`
}

// Event 中立事件单行（envelope + 按 type 生效的载荷字段，omitempty 收纳）。
type Event struct {
	Seq     int    `json:"seq"`
	Ts      string `json:"ts"`
	Task    string `json:"task"`
	Tenant  string `json:"tenant,omitempty"`
	User    string `json:"user,omitempty"`
	Session string `json:"session,omitempty"` // 省略即 main；子任务=child sessionId
	Pen     Pen    `json:"pen"`
	Type    string `json:"type"`

	Goal              string             `json:"goal,omitempty"`
	Constraints       []string           `json:"constraints,omitempty"`
	Workspace         *WorkspaceRef      `json:"workspace,omitempty"`
	Status            string             `json:"status,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	Subagent          *Subagent          `json:"subagent,omitempty"`
	Spine             *Spine             `json:"spine,omitempty"`
	Message           *Message           `json:"message,omitempty"`
	Call              *Call              `json:"call,omitempty"`
	Result            *Result            `json:"result,omitempty"`
	Usage             *Usage             `json:"usage,omitempty"`
	Compaction        *Compaction        `json:"compaction,omitempty"`
	HandoffRequested  *HandoffRequested  `json:"handoff_requested,omitempty"`
	HandoffCheckpoint *HandoffCheckpoint `json:"handoff_checkpoint,omitempty"`
	HandoffSeed       *HandoffSeed       `json:"handoff_seed,omitempty"`
	HandoffDone       *HandoffDone       `json:"handoff_done,omitempty"`
	Checkpoint        *WorkspaceMarker   `json:"checkpoint,omitempty"`

	GraphNodeDeclared  *GraphNodeDeclared  `json:"graph_node_declared,omitempty"`
	GraphNodeStarted   *GraphNodeStarted   `json:"graph_node_started,omitempty"`
	GraphNodeResult    *GraphNodeResult    `json:"graph_node_result,omitempty"`
	GraphPlanTriggered *GraphPlanTriggered `json:"graph_plan_triggered,omitempty"`
	GraphAudit         *GraphAudit         `json:"graph_audit,omitempty"` // v0.5 认领/重开/人消息审计
	GraphPaused        *GraphPaused        `json:"graph_paused,omitempty"`
	GraphConverged     *GraphConverged     `json:"graph_converged,omitempty"`
}

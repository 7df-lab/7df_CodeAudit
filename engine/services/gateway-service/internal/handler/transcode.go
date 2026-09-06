package handler

// REST /v1/* → 内部 gRPC 真实转码（ADR-132）。
// 依据: 03_接口规范.md §1.1 gateway 暴露接口清单：
//   登录/刷新、项目CRUD、任务CRUD、报告查询、结果查询（网关只转发，不定义新接口）。
// 此前实现是 HTTP 反向代理直打 gRPC 端口（协议错配必然失败），已删除；
// 本文件用 protojson + 生成 pb 客户端实现真实 JSON↔proto 转码。
// 未映射路由显式返回 501 JSON（诚实降级），不再 502 假转发。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/gateway-service/internal/middleware"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// BackendAddrs holds downstream gRPC endpoints (from config).
type BackendAddrs struct {
	ProjectAddr     string
	TaskAddr        string
	ResultAddr      string
	StorageAddr     string
	SastAdapterAddr string
	KnowledgeAddr   string
	DSHRuntimeAddr    string // DSHRuntimeService（ADR-168 /v1/tasks/{id}/ai-log）
	// CallTimeoutS — 网关→服务调用上界（秒），ADR-137：值在全局配置 gateway.grpc_call_timeout_s
	CallTimeoutS int
}

// Transcoder implements the REST→gRPC mapping.
type Transcoder struct {
	projectConn *grpc.ClientConn
	taskConn    *grpc.ClientConn
	resultConn  *grpc.ClientConn
	storageConn *grpc.ClientConn // NotificationService（TP12-T0 路由扩展）
	adapterConn *grpc.ClientConn // SASTAdapterService（/v1/tools）
	dshConn     *grpc.ClientConn // DSHRuntimeService（ADR-168）
	callTimeout time.Duration
}

// NewTranscoder dials backends (lazy connect; 失败在首次调用时以 Unavailable 体现→503).
func NewTranscoder(addrs BackendAddrs) *Transcoder {
	return &Transcoder{
		projectConn: dial(addrs.ProjectAddr),
		taskConn:    dial(addrs.TaskAddr),
		resultConn:  dial(addrs.ResultAddr),
		storageConn: dial(addrs.StorageAddr),
		adapterConn: dial(addrs.SastAdapterAddr),
		dshConn:     dial(addrs.DSHRuntimeAddr),
		callTimeout: time.Duration(addrs.CallTimeoutS) * time.Second, // ADR-137
	}
}

func dial(addr string) *grpc.ClientConn {
	if addr == "" {
		return nil
	}
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("[transcoder] dial %s: %v (调用时将返回 503)", addr, err)
		return nil
	}
	return conn
}

// Close releases backend connections.
func (t *Transcoder) Close() {
	for _, c := range []*grpc.ClientConn{t.projectConn, t.taskConn, t.resultConn, t.storageConn, t.adapterConn} {
		if c != nil {
			_ = c.Close()
		}
	}
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func writeJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	b, _ := json.Marshal(map[string]interface{}{"error": msg})
	writeJSON(w, code, b)
}

// grpcToHTTP maps gRPC status to HTTP (03 §3 错误码口径).
func grpcToHTTP(err error) int {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.FailedPrecondition, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		// 14号 §3.5 错误映射: 401=静默刷新/跳登录——登录失败(project user.go 返回
		// Unauthenticated)与过期令牌必须回 401, 与 PermissionDenied 混映射破坏前端 401 语义
		return http.StatusUnauthorized
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func (t *Transcoder) call(w http.ResponseWriter, conn *grpc.ClientConn, name string, invoke func(ctx context.Context) (proto.Message, error)) {
	if conn == nil {
		writeError(w, http.StatusServiceUnavailable, "backend connection not configured")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.callTimeout)
	defer cancel()
	resp, err := invoke(ctx)
	if err != nil {
		log.Printf("[transcoder] %s: %v", name, err)
		writeError(w, grpcToHTTP(err), status.Code(err).String()+": "+status.Convert(err).Message())
		return
	}
	// UseProtoNames: JSON 键与 proto 字段名一致（snake_case），对 API 消费者可预测
	b, merr := protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}.Marshal(resp)
	if merr != nil {
		writeError(w, http.StatusInternalServerError, "response marshal: "+merr.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// decodeBody — 把 REST 请求体 JSON 解码进 pb 请求消息。
func decodeBody(r *http.Request, msg proto.Message) error {
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(b) == 0 {
		return nil
	}
	// 容忍未知字段：向前兼容旧客户端（字段级校验仍由各服务执行）
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// newRequestID — 网关为写操作生成幂等键（REST 客户端不携带 proto metadata）。
func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "gw-" + hex.EncodeToString(b)
}

// ---- 路由（03 §1.1 暴露接口清单） ----

// Handler returns the /v1/* transcoder.
func (t *Transcoder) Handler() http.Handler {
	return http.HandlerFunc(t.serveHTTP)
}

func (t *Transcoder) serveHTTP(w http.ResponseWriter, r *http.Request) {
	segs := splitPath(r.URL.Path)
	// segs 形如 [v1 auth login] / [v1 projects {id}] / [v1 tasks {id} submit]
	// 14号 §7: /v1/tools、/v1/notifications 等两段路由合法（域下无子路径）
	if len(segs) < 2 || segs[0] != "v1" {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}
	domain, rest := segs[1], segs[2:]

	switch domain {
	case "auth":
		if r.Method != http.MethodPost || len(rest) != 1 {
			writeError(w, http.StatusMethodNotAllowed, "auth endpoints are POST /v1/auth/{login|refresh|logout}")
			return
		}
		switch rest[0] {
		case "login":
			t.login(w, r)
		case "register":
			// V2.1 (ADR-205): 自助注册（/v1/auth/* 链天然免 JWT；限流照旧）
			t.registerUser(w, r)
		case "refresh":
			t.refresh(w, r)
		case "logout":
			t.logout(w, r)
		default:
			writeError(w, http.StatusNotFound, "unknown auth endpoint")
		}
	case "users":
		t.users(w, r, rest)
	case "projects":
		t.projects(w, r, rest)
	case "tasks":
		t.tasks(w, r, rest)
	case "findings":
		if len(rest) == 1 && rest[0] == "verdict:batch" && r.Method == http.MethodPost {
			t.verdictBatch(w, r)
			return
		}
		t.findings(w, r, rest)
	case "reports":
		t.reports(w, r, rest)
	case "tools":
		t.tools(w, r, rest)
	case "notifications":
		t.notifications(w, r, rest)
	default:
		writeError(w, http.StatusNotImplemented,
			"route /v1/"+domain+" is not mapped to internal gRPC by gateway (03 §1.1); use gRPC directly")
	}
}

// ---- auth（UserService @ project-service，proto L904-L911） ----

func (t *Transcoder) login(w http.ResponseWriter, r *http.Request) {
	req := &pb.LoginRequest{}
	if err := decodeBody(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	client := pb.NewUserServiceClient(t.projectConn)
	t.call(w, t.projectConn, "UserService/Login", func(ctx context.Context) (proto.Message, error) {
		return client.Login(ctx, req)
	})
}

// registerUser — POST /v1/auth/register → UserService/RegisterUser（V2.1 ADR-205）。
// 幂等键网关生成；注册成功即返回令牌对（注册即登录）。
func (t *Transcoder) registerUser(w http.ResponseWriter, r *http.Request) {
	req := &pb.RegisterUserRequest{}
	if err := decodeBody(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
	client := pb.NewUserServiceClient(t.projectConn)
	t.call(w, t.projectConn, "UserService/RegisterUser", func(ctx context.Context) (proto.Message, error) {
		return client.RegisterUser(ctx, req)
	})
}

func (t *Transcoder) refresh(w http.ResponseWriter, r *http.Request) {
	req := &pb.RefreshTokenRequest{}
	if err := decodeBody(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	client := pb.NewUserServiceClient(t.projectConn)
	t.call(w, t.projectConn, "UserService/RefreshToken", func(ctx context.Context) (proto.Message, error) {
		return client.RefreshToken(ctx, req)
	})
}

func (t *Transcoder) logout(w http.ResponseWriter, r *http.Request) {
	req := &pb.LogoutRequest{}
	if err := decodeBody(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	client := pb.NewUserServiceClient(t.projectConn)
	t.call(w, t.projectConn, "UserService/Logout", func(ctx context.Context) (proto.Message, error) {
		return client.Logout(ctx, req)
	})
}

// ---- projects（ProjectService，proto L885-L902） ----

func (t *Transcoder) projects(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewProjectServiceClient(t.projectConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		// 注意顺序：protojson.Unmarshal 会重置消息，幂等键必须在解码后注入（TP12-T3 旅程回归）
		req := &pb.CreateProjectRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.projectConn, "ProjectService/CreateProject", func(ctx context.Context) (proto.Message, error) {
			return client.CreateProject(ctx, req)
		})
	case len(rest) == 0 && r.Method == http.MethodGet:
		req := &pb.ListProjectsRequest{}
		if err := decodeQuery(r, req, "pagination", "filter"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.projectConn, "ProjectService/ListProjects", func(ctx context.Context) (proto.Message, error) {
			return client.ListProjects(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		req := &pb.GetProjectRequest{ProjectId: rest[0]}
		t.call(w, t.projectConn, "ProjectService/GetProject", func(ctx context.Context) (proto.Message, error) {
			return client.GetProject(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodPut:
		req := &pb.UpdateProjectRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.GetProject() != nil && req.GetProject().GetProjectId() == "" {
			req.Project.ProjectId = rest[0]
		}
		t.call(w, t.projectConn, "ProjectService/UpdateProject", func(ctx context.Context) (proto.Message, error) {
			return client.UpdateProject(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodDelete:
		req := &pb.DeleteProjectRequest{ProjectId: rest[0]}
		t.call(w, t.projectConn, "ProjectService/DeleteProject", func(ctx context.Context) (proto.Message, error) {
			return client.DeleteProject(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "config" && r.Method == http.MethodGet:
		req := &pb.GetProjectConfigRequest{ProjectId: rest[0]}
		t.call(w, t.projectConn, "ProjectService/GetProjectConfig", func(ctx context.Context) (proto.Message, error) {
			return client.GetProjectConfig(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "config" && r.Method == http.MethodPut:
		req := &pb.UpdateProjectConfigRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.projectConn, "ProjectService/UpdateProjectConfig", func(ctx context.Context) (proto.Message, error) {
			return client.UpdateProjectConfig(ctx, req)
		})
	default:
		writeError(w, http.StatusNotFound, "unknown projects route")
	}
}

// decodeQuery — 把查询串按 protojson 解入消息字段。
// 标量值（task_id=t-77）自动加引号成 JSON 字符串；对象/数组/字面量按原样透传
// （如 pagination={"page_size":20}）。非 JSON 标量直传是调用方高频误区（TP12-T3 回归）。
func decodeQuery(r *http.Request, msg proto.Message, fields ...string) error {
	q := map[string]interface{}{}
	for _, f := range fields {
		vals, ok := r.URL.Query()[f]
		if !ok || len(vals) == 0 {
			continue
		}
		raw := vals[0]
		if !json.Valid([]byte(raw)) {
			quoted, merr := json.Marshal(raw)
			if merr != nil {
				return merr
			}
			raw = string(quoted)
		}
		q[f] = json.RawMessage(raw)
	}
	if len(q) == 0 {
		return nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg)
}

// ---- tasks（TaskService，proto L858-L883；04 §1 状态流转） ----

func (t *Transcoder) tasks(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewTaskServiceClient(t.taskConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		// 同上：解码后再注入幂等键
		req := &pb.CreateScanTaskRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		// ADR-199: 创建者自 JWT 注入（ScanTask.created_by → 事件通知收件人链）
		if uid, ok := r.Context().Value(middleware.UserIDKey).(string); ok && uid != "" {
			req.CreatedBy = uid
		}
		t.call(w, t.taskConn, "TaskService/CreateScanTask", func(ctx context.Context) (proto.Message, error) {
			return client.CreateScanTask(ctx, req)
		})
		// ADR-195: 任务→上传目录链接（task_id 为随机 gw-hex 不复用，创建失败至多
		// 留无害孤儿文件；写失败仅日志，source-file 解析回退③④不受影响）
		writeTaskLink(req.GetMetadata().GetRequestId(), req.GetConfig()["project_path"])
	case len(rest) == 0 && r.Method == http.MethodGet:
		req := &pb.ListScanTasksRequest{}
		// ADR-160: project_id/filter 过滤透传（契约 L1108-1112; task-service 已实现）
		if err := decodeQuery(r, req, "pagination", "project_id", "filter"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.taskConn, "TaskService/ListScanTasks", func(ctx context.Context) (proto.Message, error) {
			return client.ListScanTasks(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		req := &pb.GetScanTaskRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetScanTask", func(ctx context.Context) (proto.Message, error) {
			return client.GetScanTask(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "progress":
		req := &pb.GetTaskProgressRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetTaskProgress", func(ctx context.Context) (proto.Message, error) {
			return client.GetTaskProgress(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "logs":
		// ADR-167: 执行日志（增量游标 after_log_id + limit 经 query 透传）
		req := &pb.GetTaskLogsRequest{TaskId: rest[0]}
		if v := r.URL.Query().Get("after_log_id"); v != "" {
			req.AfterLogId = v
		}
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				req.Limit = int32(n)
			}
		}
		t.call(w, t.taskConn, "TaskService/GetTaskLogs", func(ctx context.Context) (proto.Message, error) {
			return client.GetTaskLogs(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "snapshot":
		// ADR-170: 详情页聚合快照——task+progress+logs+ai-log 单口轮询（3s=20/min），
		// 替代此前 4 个独立轮询器 ~90/min 触发 07 §7 单用户 50/min 限流（429 页面冻结）
		t.taskSnapshot(w, r, rest[0])
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "ws":
		// ADR-172: WebSocket 秒级推送（人类指令 2026-09-01）——升级后服务端 1s 聚合推帧，
		// 帧结构与 snapshot 同构；前端断线自动回退 snapshot 轮询
		t.taskWatch(w, r, rest[0])
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "ai-log":
		// ADR-168: AI 交互日志（沙箱 bridge SSE 原始帧；字节游标增量，任务终态后即最终日志）
		req := &pb.GetAIInteractionLogRequest{TaskId: rest[0]}
		if v := r.URL.Query().Get("cursor"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				req.Cursor = n
			}
		}
		if v := r.URL.Query().Get("max_bytes"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				req.MaxBytes = int32(n)
			}
		}
		t.call(w, t.dshConn, "DSHRuntimeService/GetAIInteractionLog", func(ctx context.Context) (proto.Message, error) {
			client := pb.NewDSHRuntimeServiceClient(t.dshConn)
			return client.GetAIInteractionLog(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "source-file":
		// ADR-195: 任务源码全文读取（发现详情代码上下文全文复核；gateway 本地源树）
		t.sourceFile(w, r, rest[0])
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "context":
		// 14号 §7: GetTaskContext（融合/审核视图数据源）
		req := &pb.GetTaskContextRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetTaskContext", func(ctx context.Context) (proto.Message, error) {
			return client.GetTaskContext(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "metrics":
		// 14号 §7: CalculateMetrics（对比视图；task 维度聚合，ADR-133 口径）
		fclient := pb.NewSASTFusionServiceClient(t.adapterConn)
		t.call(w, t.adapterConn, "SASTFusionService/CalculateMetrics", func(ctx context.Context) (proto.Message, error) {
			return fclient.CalculateMetrics(ctx, &pb.CalculateMetricsRequest{TaskId: rest[0]})
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "comparison-report":
		// 14号 §7: GenerateComparisonReport（模式C 对比报告）
		fclient := pb.NewSASTFusionServiceClient(t.adapterConn)
		t.call(w, t.adapterConn, "SASTFusionService/GenerateComparisonReport", func(ctx context.Context) (proto.Message, error) {
			return fclient.GenerateComparisonReport(ctx, &pb.GenerateComparisonReportRequest{TaskId: rest[0]})
		})
	case len(rest) == 2 && r.Method == http.MethodPost && rest[1] == "report":
		// 14号 §7: GenerateReport（10 §3.2 口径）；幂等键网关生成
		rclient := pb.NewReportServiceClient(t.resultConn)
		req := &pb.GenerateReportRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		if req.GetTaskId() == "" {
			req.TaskId = rest[0]
		}
		if req.GetFormat() == pb.ReportFormat_REPORT_FORMAT_UNSPECIFIED {
			req.Format = pb.ReportFormat_REPORT_FORMAT_JSON // 缺省 JSON（body 可显式传 HTML）
		}
		t.call(w, t.resultConn, "ReportService/GenerateReport", func(ctx context.Context) (proto.Message, error) {
			return rclient.GenerateReport(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodPost:
		id := rest[0]
		callByName := map[string]func(ctx context.Context) (proto.Message, error){
			// 2026-09-01 人类裁定：审批流（submit/approve/reject）废除，有创建权限即有启动权限
			"start": func(ctx context.Context) (proto.Message, error) {
				return client.StartTask(ctx, &pb.StartTaskRequest{TaskId: id})
			},
			"cancel": func(ctx context.Context) (proto.Message, error) {
				return client.CancelScanTask(ctx, &pb.CancelScanTaskRequest{TaskId: id})
			},
			"retry": func(ctx context.Context) (proto.Message, error) {
				return client.RetryScanTask(ctx, &pb.RetryScanTaskRequest{TaskId: id})
			},
			// ADR-200: 暂停/恢复（AI 交互会话回合闸门；前端按钮互切）
			"pause": func(ctx context.Context) (proto.Message, error) {
				return client.PauseTask(ctx, &pb.PauseTaskRequest{TaskId: id})
			},
			"resume": func(ctx context.Context) (proto.Message, error) {
				return client.ResumeTask(ctx, &pb.ResumeTaskRequest{TaskId: id})
			},
			"complete": func(ctx context.Context) (proto.Message, error) {
				return client.CompleteTask(ctx, &pb.CompleteTaskRequest{TaskId: id})
			},
		}
		fn, ok := callByName[rest[1]]
		if !ok {
			writeError(w, http.StatusNotFound, "unknown task action "+rest[1])
			return
		}
		t.call(w, t.taskConn, "TaskService/"+rest[1], fn)
	default:
		writeError(w, http.StatusNotFound, "unknown tasks route")
	}
}

// taskSnapshot — GET /v1/tasks/{id}/snapshot?logs_after=&log_limit=&ai_cursor=
// 聚合详情页四个读口（task 必达；progress/logs/ai 尽力而为），一次响应驱动整页，
// 替代多轮询器。JSON 键与单口一致（snake_case），前端零字段迁移。
func (t *Transcoder) taskSnapshot(w http.ResponseWriter, r *http.Request, taskID string) {
	ctx, cancel := context.WithTimeout(r.Context(), t.callTimeout)
	defer cancel()

	var (
		wg       sync.WaitGroup
		taskJSON json.RawMessage
		progJSON json.RawMessage
		logsJSON json.RawMessage = json.RawMessage(`{"logs":[]}`)
		aiJSON   json.RawMessage = json.RawMessage(`{"chunk":"","next_cursor":"0","complete":true,"total_bytes":"0"}`)
		taskErr  error
	)
	marshal := func(m proto.Message) json.RawMessage {
		b, err := protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}.Marshal(m)
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
		taskJSON = marshal(resp)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := pb.NewTaskServiceClient(t.taskConn).GetTaskProgress(ctx, &pb.GetTaskProgressRequest{TaskId: taskID})
		if err == nil {
			progJSON = marshal(resp)
		}
	}()
	logAfter := r.URL.Query().Get("logs_after")
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := &pb.GetTaskLogsRequest{TaskId: taskID, Limit: 500}
		if logAfter != "" {
			req.AfterLogId = logAfter
		}
		if resp, err := pb.NewTaskServiceClient(t.taskConn).GetTaskLogs(ctx, req); err == nil {
			logsJSON = marshal(resp)
		}
	}()
	aiCursor := int64(0)
	if v := r.URL.Query().Get("ai_cursor"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			aiCursor = n
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if t.dshConn == nil {
			return
		}
		req := &pb.GetAIInteractionLogRequest{TaskId: taskID, Cursor: aiCursor}
		if resp, err := pb.NewDSHRuntimeServiceClient(t.dshConn).GetAIInteractionLog(ctx, req); err == nil {
			aiJSON = marshal(resp)
		}
	}()
	wg.Wait()
	if taskErr != nil {
		writeError(w, grpcToHTTP(taskErr), status.Code(taskErr).String()+": "+status.Convert(taskErr).Message())
		return
	}
	b, err := json.Marshal(map[string]json.RawMessage{
		"task": taskJSON, "progress": progJSON, "logs": logsJSON, "ai": aiJSON,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot marshal: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ---- findings（ResultService，proto L920-L935） ----

func (t *Transcoder) findings(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewResultServiceClient(t.resultConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		req := &pb.ListFindingsRequest{}
		// task_id 为 proto L1220 顶层字段（10 §3.2 任务维度查询口径）
		if err := decodeQuery(r, req, "task_id", "pagination", "filter", "sort"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.resultConn, "ResultService/ListFindings", func(ctx context.Context) (proto.Message, error) {
			return client.ListFindings(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		req := &pb.GetFindingRequest{FindingId: rest[0]}
		t.call(w, t.resultConn, "ResultService/GetFinding", func(ctx context.Context) (proto.Message, error) {
			return client.GetFinding(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "verdict" && r.Method == http.MethodPut:
		// 14号 §7 / 10 §3.2: 人工 triage 回写（proto L1240 UpdateVerdict）
		req := &pb.UpdateVerdictRequest{FindingId: rest[0]}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.GetFindingId() == "" {
			req.FindingId = rest[0]
		}
		t.call(w, t.resultConn, "ResultService/UpdateVerdict", func(ctx context.Context) (proto.Message, error) {
			return client.UpdateVerdict(ctx, req)
		})
	default:
		writeError(w, http.StatusNotFound, "unknown findings route")
	}
}

// verdictBatch — POST /v1/findings/verdict:batch → BatchUpdateVerdict（批量 triage）。
func (t *Transcoder) verdictBatch(w http.ResponseWriter, r *http.Request) {
	client := pb.NewResultServiceClient(t.resultConn)
	req := &pb.BatchUpdateVerdictRequest{}
	if err := decodeBody(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
	t.call(w, t.resultConn, "ResultService/BatchUpdateVerdict", func(ctx context.Context) (proto.Message, error) {
		return client.BatchUpdateVerdict(ctx, req)
	})
}

// requireAdmin — 管理端路由门禁（V2.1 ADR-205）：JWT role claim 必须 ROLE_ADMIN。
// 旧令牌（A6 前 sign、无 role claim）一律 403，重新登录即获得新 claim；
// 服务端强制（gRPC metadata 贯通 caller 身份）为 V2.2 候选，当前网关是唯一管理入口。
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if role, _ := r.Context().Value(middleware.UserRoleKey).(string); role == "ROLE_ADMIN" {
		return true
	}
	writeError(w, http.StatusForbidden, "admin role required")
	return false
}

// users — UserService 路由（14号 §7）：me/get/update/permissions + V2.1 生命周期。
func (t *Transcoder) users(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewUserServiceClient(t.projectConn)
	switch {
	case len(rest) == 1 && rest[0] == "me" && r.Method == http.MethodGet:
		// 网关把 Bearer 填入 access_token 字段（proto L1206 GetCurrentUserRequest）
		req := &pb.GetCurrentUserRequest{AccessToken: bearerToken(r)}
		t.call(w, t.projectConn, "UserService/GetCurrentUser", func(ctx context.Context) (proto.Message, error) {
			return client.GetCurrentUser(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "permissions" && r.Method == http.MethodGet:
		req := &pb.GetUserPermissionsRequest{UserId: rest[0]}
		t.call(w, t.projectConn, "UserService/GetUserPermissions", func(ctx context.Context) (proto.Message, error) {
			return client.GetUserPermissions(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "password" && r.Method == http.MethodPost:
		// V2.1 (ADR-205): 自助改密——user_id 强制取自 JWT（self），不接受请求体指定
		req := &pb.ChangePasswordRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		req.UserId, _ = r.Context().Value(middleware.UserIDKey).(string)
		t.call(w, t.projectConn, "UserService/ChangePassword", func(ctx context.Context) (proto.Message, error) {
			return client.ChangePassword(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "password:reset" && r.Method == http.MethodPost:
		// V2.1 (ADR-205): 管理员重置为服务端生成的一次性临时密码
		if !requireAdmin(w, r) {
			return
		}
		req := &pb.ResetPasswordRequest{UserId: rest[0], Metadata: &pb.RequestMetadata{RequestId: newRequestID()}}
		t.call(w, t.projectConn, "UserService/ResetPassword", func(ctx context.Context) (proto.Message, error) {
			return client.ResetPassword(ctx, req)
		})
	case len(rest) == 0 && r.Method == http.MethodGet:
		// V2.1 (ADR-205): 管理端用户列表（admin 门禁；分页/过滤透传）
		if !requireAdmin(w, r) {
			return
		}
		req := &pb.ListUsersRequest{}
		if err := decodeQuery(r, req, "pagination", "state", "username_contains"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.projectConn, "UserService/ListUsers", func(ctx context.Context) (proto.Message, error) {
			return client.ListUsers(ctx, req)
		})
	case len(rest) == 0 && r.Method == http.MethodPost:
		// V2.1 (ADR-205): 管理员直建账号（admin 门禁；幂等键网关生成）
		if !requireAdmin(w, r) {
			return
		}
		req := &pb.CreateUserRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.projectConn, "UserService/CreateUser", func(ctx context.Context) (proto.Message, error) {
			return client.CreateUser(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		req := &pb.GetUserRequest{UserId: rest[0]}
		t.call(w, t.projectConn, "UserService/GetUser", func(ctx context.Context) (proto.Message, error) {
			return client.GetUser(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodPut:
		req := &pb.UpdateUserRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.GetUser() != nil && req.GetUser().GetUserId() == "" {
			req.User.UserId = rest[0]
		}
		t.call(w, t.projectConn, "UserService/UpdateUser", func(ctx context.Context) (proto.Message, error) {
			return client.UpdateUser(ctx, req)
		})
	default:
		writeError(w, http.StatusNotFound, "unknown users route")
	}
}

// bearerToken — 提取 Authorization: Bearer <token>。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ---- reports（ReportService，proto L940-L950） ----

func (t *Transcoder) reports(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewReportServiceClient(t.resultConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		req := &pb.ListReportsRequest{}
		// task_id 为 proto L1264 顶层字段（任务↔报告双向导航）
		if err := decodeQuery(r, req, "task_id", "pagination"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.resultConn, "ReportService/ListReports", func(ctx context.Context) (proto.Message, error) {
			return client.ListReports(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		req := &pb.GetReportRequest{ReportId: rest[0]}
		t.call(w, t.resultConn, "ReportService/GetReport", func(ctx context.Context) (proto.Message, error) {
			return client.GetReport(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "download" && r.Method == http.MethodGet:
		// 14号 §7 / §3.4: 下载=网关聚合 DownloadReport 服务端流（proto L1270-L1271）
		t.downloadReport(w, rest[0])
	default:
		writeError(w, http.StatusNotFound, "unknown reports route")
	}
}

// downloadReport — 聚合 ReportChunk 流为 HTTP 响应体（14号 §3.4 V1 口径）。
func (t *Transcoder) downloadReport(w http.ResponseWriter, reportID string) {
	if t.resultConn == nil {
		writeError(w, http.StatusServiceUnavailable, "backend connection not configured")
		return
	}
	client := pb.NewReportServiceClient(t.resultConn)
	ctx, cancel := context.WithTimeout(context.Background(), t.callTimeout)
	defer cancel()
	stream, err := client.DownloadReport(ctx, &pb.DownloadReportRequest{ReportId: reportID})
	if err != nil {
		writeError(w, grpcToHTTP(err), status.Code(err).String()+": "+status.Convert(err).Message())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", reportID+".bin"))
	w.WriteHeader(http.StatusOK)
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			// 头已发出：只能截断并在日志留痕（诚实降级：半途失败不可伪装完整）
			log.Printf("[transcoder] DownloadReport %s stream error: %v", reportID, err)
			return
		}
		if _, werr := w.Write(chunk.GetData()); werr != nil {
			return
		}
	}
}

// tools — SASTAdapterService 能力探测（14号 §7）：仅列可执行工具并附校验结果。
func (t *Transcoder) tools(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewSASTAdapterServiceClient(t.adapterConn)
	if len(rest) != 0 || r.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, "use GET /v1/tools")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.callTimeout)
	defer cancel()
	list, err := client.ListAvailableTools(ctx, &pb.ListAvailableToolsRequest{})
	if err != nil {
		writeError(w, grpcToHTTP(err), status.Code(err).String()+": "+status.Convert(err).Message())
		return
	}
	// 逐工具附 ValidateToolConfig 结论（14号 §3.3 ①：创建向导只展示可执行工具）
	out := make([]map[string]interface{}, 0, len(list.GetTools()))
	for _, tool := range list.GetTools() {
		vcfg, verr := client.ValidateToolConfig(ctx, &pb.ValidateToolConfigRequest{ToolId: tool.GetToolId()})
		entry := map[string]interface{}{
			"tool_id": tool.GetToolId(), "name": tool.GetName(),
			"supported_languages": tool.GetSupportedLanguages(), "output_format": tool.GetOutputFormat(),
		}
		if verr != nil {
			entry["valid"] = false
			entry["errors"] = []string{status.Convert(verr).Message()}
		} else {
			entry["valid"] = vcfg.GetValid()
			entry["errors"] = vcfg.GetErrors()
		}
		out = append(out, entry)
	}
	b, _ := json.Marshal(map[string]interface{}{"tools": out})
	writeJSON(w, http.StatusOK, b)
}

// notifications — NotificationService（storage-service，14号 §7 通知中心）。
func (t *Transcoder) notifications(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewNotificationServiceClient(t.storageConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		req := &pb.ListNotificationsRequest{UserId: r.URL.Query().Get("user_id")}
		if r.URL.Query().Get("unread_only") == "true" {
			req.UnreadOnly = true
		}
		t.call(w, t.storageConn, "NotificationService/ListNotifications", func(ctx context.Context) (proto.Message, error) {
			return client.ListNotifications(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "read" && r.Method == http.MethodPost:
		req := &pb.MarkNotificationReadRequest{NotificationId: rest[0]}
		t.call(w, t.storageConn, "NotificationService/MarkNotificationRead", func(ctx context.Context) (proto.Message, error) {
			return client.MarkNotificationRead(ctx, req)
		})
	default:
		writeError(w, http.StatusNotFound, "unknown notifications route")
	}
}

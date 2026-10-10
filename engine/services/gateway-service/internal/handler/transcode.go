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
	RuntimeAddr     string // RuntimeService（ADR-168 /v1/tasks/{id}/ai-log）
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
	dshConn     *grpc.ClientConn // RuntimeService（ADR-168）
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
		dshConn:     dial(addrs.RuntimeAddr),
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
	for _, c := range []*grpc.ClientConn{t.projectConn, t.taskConn, t.resultConn, t.storageConn, t.adapterConn, t.dshConn} {
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
	resp, err := t.invoke(w, conn, name, invoke)
	if err != nil {
		return // 错误响应已写
	}
	// UseProtoNames: JSON 键与 proto 字段名一致（snake_case），对 API 消费者可预测
	b, merr := protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}.Marshal(resp)
	if merr != nil {
		writeError(w, http.StatusInternalServerError, "response marshal: "+merr.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// invoke — R91: call 的前半段（RPC+错误写响应），返回响应供需要后处理的路由使用
// （项目创建：创建者成员登记需拿到 project_id 后再写响应）。
func (t *Transcoder) invoke(w http.ResponseWriter, conn *grpc.ClientConn, name string, invoke func(ctx context.Context) (proto.Message, error)) (proto.Message, error) {
	if conn == nil {
		writeError(w, http.StatusServiceUnavailable, "backend connection not configured")
		return nil, status.Error(codes.Internal, "backend connection not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.callTimeout)
	defer cancel()
	resp, err := invoke(ctx)
	if err != nil {
		log.Printf("[transcoder] %s: %v", name, err)
		writeError(w, grpcToHTTP(err), status.Code(err).String()+": "+status.Convert(err).Message())
		return nil, err
	}
	return resp, nil
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
	// R84: JSON 转码口请求体上限 1MiB——decodeBody 无界 ReadAll，认证用户一条大 body
	// 即可 OOM 网关（gRPC 4MB 上限在下游，拦不住网关自身）。上传口走独立 mux 条目
	// 自带 100MB MaxBytesReader（upload.go），不受影响。
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
		// V2.2 (ADR-255): GET /v1/auth/registration-policy — 策略只读公开面（Q10），
		// 仅 mode 枚举值；与 POST 族并列同段。
		if r.Method == http.MethodGet && len(rest) == 1 && rest[0] == "registration-policy" {
			t.registrationPolicy(w, r)
			return
		}
		if r.Method != http.MethodPost || len(rest) != 1 {
			writeError(w, http.StatusMethodNotAllowed, "auth endpoints are POST /v1/auth/{login|register|refresh|logout}; GET /v1/auth/registration-policy")
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
	case "agents":
		// TP20-T2 切换 Agent 收口：清单端点 GET /v1/agents——登录可读（/v1 保护链
		// 已含 JWT，非 admin 专属——创建页与切换 Modal 同源，计划 §0 权限裁决）
		t.agents(w, r, rest)
	case "orchestrations":
		t.orchestrations(w, r, rest)
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
	case "inference":
		// ADR-217: 推理 provider/路由管理面——凭据写入与路由切换，全路由 admin 门禁
		if !requireAdmin(w, r) {
			return
		}
		t.inference(w, r, rest)
	case "workspaces":
		// 切片3b: workspace 清单（team 上车；admin 面——与 inference 同门禁）
		if !requireAdmin(w, r) {
			return
		}
		t.workspaces(w, r, rest)
	case "invites":
		// V2.2 (ADR-255): 邀请码管理面——生成/吊销/删除/列表全 admin 门禁
		if !requireAdmin(w, r) {
			return
		}
		t.invites(w, r, rest)
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

// registrationPolicy — GET /v1/auth/registration-policy → UserService/GetRegistrationPolicy
// （V2.2 ADR-255，Q10）。/v1/auth/* 链天然免 JWT：注册页据此自适应（open 隐藏邀请码栏/
// disabled 入口置灰），根治前端静态文案与实况漂移。仅回 mode 枚举值，无码表无计数。
func (t *Transcoder) registrationPolicy(w http.ResponseWriter, r *http.Request) {
	client := pb.NewUserServiceClient(t.projectConn)
	t.call(w, t.projectConn, "UserService/GetRegistrationPolicy", func(ctx context.Context) (proto.Message, error) {
		return client.GetRegistrationPolicy(ctx, &pb.GetRegistrationPolicyRequest{})
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
	// R88: 网关本地即时撤销（服务端纪元 R87 另管 refresh 侧）——此前登出后 access
	// 在全部业务路由仍可用 30min。
	if tok := req.GetAccessToken(); tok != "" {
		middleware.RevokeAccess(tok)
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
	// ADR-257: 项目创建=操作面（DEVELOPER+；VIEWER 只读）。
		if !requireOperator(w, r, "projects (create)") {
			return
		}
		// R91: 创建者自动登记为项目成员（成员制授权的准入面；无身份则无从登记）。
		// 成员登记失败不回滚创建（幂等键已消耗），留日志 admin 可补。
		resp, cerr := t.invoke(w, t.projectConn, "ProjectService/CreateProject", func(ctx context.Context) (proto.Message, error) {
			return client.CreateProject(ctx, req)
		})
		if cerr != nil {
			return
		}
		if uid := callerID(r); uid != "" && resp != nil {
			mctx, mcancel := context.WithTimeout(context.Background(), t.callTimeout)
			defer mcancel()
			project := resp.(*pb.Project)
			if _, merr := client.AddProjectMember(mctx, &pb.AddProjectMemberRequest{
				Metadata: &pb.RequestMetadata{RequestId: newRequestID()},
				Member:   &pb.ProjectMember{ProjectId: project.GetProjectId(), UserId: uid, Role: "developer"},
			}); merr != nil {
				log.Printf("[authz] creator member registration failed project=%s user=%s: %v", project.GetProjectId(), uid, merr)
			}
		}
		b, merr := protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}.Marshal(resp)
		if merr != nil {
			writeError(w, http.StatusInternalServerError, "response marshal: "+merr.Error())
			return
		}
		writeJSON(w, http.StatusOK, b)
	case len(rest) == 0 && r.Method == http.MethodGet:
		// ADR-257: 裸列表=审计数据面，认证即读（R91 一期 admin 收紧自此反转）。
		req := &pb.ListProjectsRequest{}
		// ADR-256: sort 接线（filter 此前已在白名单但后端未实现，本批落地）
		if err := decodeQuery(r, req, "pagination", "filter", "sort"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.projectConn, "ProjectService/ListProjects", func(ctx context.Context) (proto.Message, error) {
			return client.ListProjects(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		// ADR-257: 项目读取=认证即读
		req := &pb.GetProjectRequest{ProjectId: rest[0]}
		t.call(w, t.projectConn, "ProjectService/GetProject", func(ctx context.Context) (proto.Message, error) {
			return client.GetProject(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodPut:
		// ADR-257: 项目更新=操作面 DEVELOPER+（R91 一期 admin-only 放宽；删除仍 admin）
		if !requireOperator(w, r, "project/"+rest[0]+" (update)") {
			return
		}
		req := &pb.UpdateProjectRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// R91: body 钉死路径身份（服务端按 body project_id 落改——URL 与 body 错位
		// 即绕过门禁，R73 同型教训）
		if req.GetProject() != nil {
			req.Project.ProjectId = rest[0]
		}
		t.call(w, t.projectConn, "ProjectService/UpdateProject", func(ctx context.Context) (proto.Message, error) {
			return client.UpdateProject(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodDelete:
		// ADR-257: 删除=管理面（admin only）
		if !requireAdmin(w, r) {
			return
		}
		req := &pb.DeleteProjectRequest{ProjectId: rest[0]}
		t.call(w, t.projectConn, "ProjectService/DeleteProject", func(ctx context.Context) (proto.Message, error) {
			return client.DeleteProject(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "config" && r.Method == http.MethodGet:
		// ADR-257: 项目配置读取=认证即读
		req := &pb.GetProjectConfigRequest{ProjectId: rest[0]}
		t.call(w, t.projectConn, "ProjectService/GetProjectConfig", func(ctx context.Context) (proto.Message, error) {
			return client.GetProjectConfig(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "config" && r.Method == http.MethodPut:
		// ADR-257: 配置更新=操作面 DEVELOPER+
		if !requireOperator(w, r, "project/"+rest[0]+" (config update)") {
			return
		}
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
// （如 pagination={"page_size":20}——示例值，非本服务常量；分页口径依据: 03 §5）。
// 非 JSON 标量直传是调用方高频误区（TP12-T3 回归）。
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
		// ADR-257: 任务读取=认证即读
		req := &pb.GetScanTaskRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetScanTask", func(ctx context.Context) (proto.Message, error) {
			return client.GetScanTask(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "progress":
		// ADR-257: 认证即读
		req := &pb.GetTaskProgressRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetTaskProgress", func(ctx context.Context) (proto.Message, error) {
			return client.GetTaskProgress(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "logs":
		// ADR-167: 执行日志（增量游标 after_log_id + limit 经 query 透传）；ADR-257 认证即读
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
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "usage":
		// ADR-247（切片3后半）：任务 usage 计量聚合（task_usage 表，baton
		// usage.recorded 终态归集投影）；totals+分模型明细；ADR-257 认证即读
		req := &pb.GetTaskUsageRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetTaskUsage", func(ctx context.Context) (proto.Message, error) {
			return client.GetTaskUsage(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "stats":
		// v0.1.2（2026-10-10 GUI 遍历判例）：任务详情概况实时统计。此前概况计数
		// 吃报告 JSON 快照（生成时点冻结），人工裁决回写后永不动——实时/快照两种
		// 口径各归其位：本路由=实时档（findings 表两轴桶），报告页维持快照口径。
		// GetTaskResultStats 在 result-service，不 经 task-service 中转。ADR-257 认证即读。
		req := &pb.GetTaskResultStatsRequest{TaskId: rest[0]}
		t.call(w, t.resultConn, "ResultService/GetTaskResultStats", func(ctx context.Context) (proto.Message, error) {
			return pb.NewResultServiceClient(t.resultConn).GetTaskResultStats(ctx, req)
		})
	case len(rest) == 5 && rest[1] == "graph" && rest[2] == "nodes" && r.Method == http.MethodPost && rest[4] == "message":
		// v0.5 人机共编辑（graph-engine-v0.5-blackboard-hoc.md §4）：向沙箱内
		// Agent 追加 user 消息（回合边界生效）；操作面 DEVELOPER+（ADR-257）。NodeId=rest[3]。
		if !requireOperator(w, r, "task/"+rest[0]+" (graph message)") {
			return
		}
		req := &pb.InjectGraphNodeMessageRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.TaskId, req.NodeId = rest[0], rest[3]                     // 路径权威（decodeBody 的 protojson 会重置预填字段——TP12-T3 同款）
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()} // R4：幂等键网关注入
		t.call(w, t.dshConn, "RuntimeService/InjectGraphNodeMessage", func(ctx context.Context) (proto.Message, error) {
			return pb.NewRuntimeServiceClient(t.dshConn).InjectGraphNodeMessage(ctx, req)
		})
	case len(rest) == 5 && rest[1] == "graph" && rest[2] == "nodes" && r.Method == http.MethodPost && rest[4] == "reopen":
		// v0.5 终态节点重开（§2.1）；操作面 DEVELOPER+（ADR-257）。NodeId=rest[3]。
		if !requireOperator(w, r, "task/"+rest[0]+" (graph reopen)") {
			return
		}
		req := &pb.ReopenGraphNodeRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.TaskId, req.NodeId = rest[0], rest[3]
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.dshConn, "RuntimeService/ReopenGraphNode", func(ctx context.Context) (proto.Message, error) {
			return pb.NewRuntimeServiceClient(t.dshConn).ReopenGraphNode(ctx, req)
		})
	case len(rest) == 5 && rest[1] == "graph" && rest[2] == "nodes" && r.Method == http.MethodPost && rest[4] == "claim":
		// v0.5 认领层（§2.1/§5.1 Claim 按钮）：human 占住节点；操作面 DEVELOPER+（ADR-257）。
		if !requireOperator(w, r, "task/"+rest[0]+" (graph claim)") {
			return
		}
		req := &pb.ClaimGraphNodeRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.TaskId, req.NodeId = rest[0], rest[3]
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.dshConn, "RuntimeService/ClaimGraphNode", func(ctx context.Context) (proto.Message, error) {
			return pb.NewRuntimeServiceClient(t.dshConn).ClaimGraphNode(ctx, req)
		})

	case len(rest) == 5 && rest[1] == "graph" && rest[2] == "nodes" && r.Method == http.MethodDelete && rest[4] == "claim":
		// v0.5 释放（human release→调度恢复）；操作面 DEVELOPER+（ADR-257）。
		if !requireOperator(w, r, "task/"+rest[0]+" (graph release)") {
			return
		}
		req := &pb.ReleaseGraphNodeRequest{TaskId: rest[0], NodeId: rest[3],
			Metadata: &pb.RequestMetadata{RequestId: newRequestID()}}
		t.call(w, t.dshConn, "RuntimeService/ReleaseGraphNode", func(ctx context.Context) (proto.Message, error) {
			return pb.NewRuntimeServiceClient(t.dshConn).ReleaseGraphNode(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "graph":
		// 切片5+6 暴露批 P1a（2026-09-20）：图执行视图投影（graph.log 回放；
		// 设计=伞仓 docs/designs/platform-graph-web-exposure.md §1）；非图任务=空态；ADR-257 认证即读
		req := &pb.GetTaskGraphRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetTaskGraph", func(ctx context.Context) (proto.Message, error) {
			return client.GetTaskGraph(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "snapshot":
		// ADR-170: 详情页聚合快照——task+progress+logs+ai-log 单口轮询（3s=20/min），
		// 替代此前 4 个独立轮询器 ~90/min，会吃满 07 §7 单用户限流预算（429 页面冻结）；ADR-257 认证即读
		t.taskSnapshot(w, r, rest[0])
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "ws":
		// ADR-172: WebSocket 秒级推送（人类指令 2026-09-01）——升级后服务端 1s 聚合推帧，
		// 帧结构与 snapshot 同构；前端断线自动回退 snapshot 轮询；ADR-257 认证即读
		t.taskWatch(w, r, rest[0])
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "ai-log":
		// ADR-168: AI 交互日志（沙箱 bridge SSE 原始帧；字节游标增量，任务终态后即最终日志）；ADR-257 认证即读
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
		t.call(w, t.dshConn, "RuntimeService/GetAIInteractionLog", func(ctx context.Context) (proto.Message, error) {
			client := pb.NewRuntimeServiceClient(t.dshConn)
			return client.GetAIInteractionLog(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "source-file":
		// ADR-195: 任务源码全文读取（发现详情代码上下文全文复核；gateway 本地源树）；ADR-257 认证即读
		t.sourceFile(w, r, rest[0])
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "context":
		// 14号 §7: GetTaskContext（融合/审核视图数据源）；ADR-257 认证即读
		req := &pb.GetTaskContextRequest{TaskId: rest[0]}
		t.call(w, t.taskConn, "TaskService/GetTaskContext", func(ctx context.Context) (proto.Message, error) {
			return client.GetTaskContext(ctx, req)
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "metrics":
		// 14号 §7: CalculateMetrics（对比视图；task 维度聚合，ADR-133 口径）；ADR-257 认证即读
		fclient := pb.NewSASTFusionServiceClient(t.adapterConn)
		t.call(w, t.adapterConn, "SASTFusionService/CalculateMetrics", func(ctx context.Context) (proto.Message, error) {
			return fclient.CalculateMetrics(ctx, &pb.CalculateMetricsRequest{TaskId: rest[0]})
		})
	case len(rest) == 2 && r.Method == http.MethodGet && rest[1] == "comparison-report":
		// 14号 §7: GenerateComparisonReport（模式C 对比报告）；ADR-257 认证即读
		fclient := pb.NewSASTFusionServiceClient(t.adapterConn)
		t.call(w, t.adapterConn, "SASTFusionService/GenerateComparisonReport", func(ctx context.Context) (proto.Message, error) {
			return fclient.GenerateComparisonReport(ctx, &pb.GenerateComparisonReportRequest{TaskId: rest[0]})
		})
	case len(rest) == 2 && r.Method == http.MethodPost && rest[1] == "report":
		// 14号 §7: GenerateReport（10 §3.2 口径）；幂等键网关生成；操作面 DEVELOPER+（ADR-257）
		if !requireOperator(w, r, "task/"+rest[0]+" (report generate)") {
			return
		}
		rclient := pb.NewReportServiceClient(t.resultConn)
		req := &pb.GenerateReportRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		// R89: body 钉死路径身份（R73 同型教训——防 body task_id 指向他人任务）
		req.TaskId = rest[0]
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
				// D2 S2 换手：?agent=<id>（可选）=pause 同时换笔（PAUSED 态限定语义在
				// engine 侧 transition 成立后执行）
				return client.PauseTask(ctx, &pb.PauseTaskRequest{
					TaskId: id, Agent: r.URL.Query().Get("agent"),
				})
			},
			"resume": func(ctx context.Context) (proto.Message, error) {
				return client.ResumeTask(ctx, &pb.ResumeTaskRequest{TaskId: id})
			},
			// TP20-T2 切换 Agent（PAUSED|QUEUED 态）：body {"agent": id}；归属门禁
			// 与 pause 同面 1:1（操作面门禁在下方统一执行，§0 权限裁决→ADR-257）。
			"agent": func(ctx context.Context) (proto.Message, error) {
				req := &pb.SetTaskAgentRequest{TaskId: id}
				if err := decodeBody(r, req); err != nil {
					return nil, status.Error(codes.InvalidArgument, err.Error())
				}
				req.TaskId = id // R89: 路径身份权威——body task_id 不采信
				return client.SetTaskAgent(ctx, req)
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
		// ADR-257: 生命周期动作=操作面 DEVELOPER+（路由合法性先行，授权在后——未知动作保持 404 语义）
		if !requireOperator(w, r, "task/"+rest[0]+" action "+rest[1]) {
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
	nodeCursors := parseNodeCursors(r.URL.Query()["node_cursor"]) // 批2：图任务逐节点游标
	wg.Add(1)
	go func() {
		defer wg.Done()
		if t.dshConn == nil {
			return
		}
		req := &pb.GetAIInteractionLogRequest{TaskId: taskID, Cursor: aiCursor, NodeCursors: nodeCursors}
		if resp, err := pb.NewRuntimeServiceClient(t.dshConn).GetAIInteractionLog(ctx, req); err == nil {
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
		// ADR-257: 发现列表=审计数据面，认证即读（R90 一期收紧自此反转）
		t.call(w, t.resultConn, "ResultService/ListFindings", func(ctx context.Context) (proto.Message, error) {
			return client.ListFindings(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		// ADR-257: 发现读取=认证即读
		req := &pb.GetFindingRequest{FindingId: rest[0]}
		t.call(w, t.resultConn, "ResultService/GetFinding", func(ctx context.Context) (proto.Message, error) {
			return client.GetFinding(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "verdict" && r.Method == http.MethodPut:
		// 14号 §7 / 10 §3.2: 人工 triage 回写（proto L1240 UpdateVerdict）
		// ADR-257: verdict 回写=写面，维持 owner/admin（R90 判定面不随读面反转）
		if !t.canAccessTaskViaFinding(w, r, rest[0]) {
			return
		}
		req := &pb.UpdateVerdictRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 第四轮审计 P1（越权）：decode 后无条件钉死路径身份（R89/R73 纪律，report
		// 生成/agent 动作等同型路由同款）——protojson.Unmarshal 会 Reset 预填值，
		// body 携带 finding_id 即可越权改写他人 finding（原"仅空时回填"是旁路）。
		req.FindingId = rest[0]
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
	// R90: 批量与单条 verdict 同口径——单条收口批量直通即门禁旁路
	if !t.canBatchVerdict(w, r, req.GetFindingIds()) {
		return
	}
	t.call(w, t.resultConn, "ResultService/BatchUpdateVerdict", func(ctx context.Context) (proto.Message, error) {
		return client.BatchUpdateVerdict(ctx, req)
	})
}

// isAdmin — JWT role claim 是否 ROLE_ADMIN（用户面门禁的共用判定核心）。
func isAdmin(r *http.Request) bool {
	role, _ := r.Context().Value(middleware.UserRoleKey).(string)
	return role == "ROLE_ADMIN"
}

// requireAdmin — 管理端路由门禁（V2.1 ADR-205）：JWT role claim 必须 ROLE_ADMIN。
// 旧令牌（A6 前 sign、无 role claim）一律 403，重新登录即获得新 claim；
// 服务端强制（gRPC metadata 贯通 caller 身份）为 V2.2 候选，当前网关是唯一管理入口。
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if isAdmin(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "admin role required")
	return false
}

// adminOrSelf — R73：用户资料读改门禁（admin 全权；非 admin 仅 self）。
// /v1/users/{id} 此前裸奔：PUT 显式传 role=ROLE_ADMIN 即可自封管理员（无服务端
// caller 身份，网关是唯一管理入口，V2.1 ADR-205），GET 可枚举任意用户资料。
func adminOrSelf(w http.ResponseWriter, r *http.Request, userID string) bool {
	if isAdmin(r) {
		return true
	}
	if uid, _ := r.Context().Value(middleware.UserIDKey).(string); uid != "" && uid == userID {
		return true
	}
	writeError(w, http.StatusForbidden, "admin role or self required")
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
		// R85（复审）: 权限矩阵是资料的超集敏感面（可枚举任意用户的角色能力），
		// 与资料读取同门禁——admin 或 self。
		if !adminOrSelf(w, r, rest[0]) {
			return
		}
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
		// R73: 资料读取 admin 或 self（此前任意认证用户可枚举任意用户）
		if !adminOrSelf(w, r, rest[0]) {
			return
		}
		req := &pb.GetUserRequest{UserId: rest[0]}
		t.call(w, t.projectConn, "UserService/GetUser", func(ctx context.Context) (proto.Message, error) {
			return client.GetUser(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodPut:
		// R73: 资料更新 admin 或 self；非 admin 的 self 强制剥离 role 变更
		// （服务端 UpdateUser 对 UNSPECIFIED 保全存量 role——剥离即阻断自封 admin）
		if !adminOrSelf(w, r, rest[0]) {
			return
		}
		req := &pb.UpdateUserRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// R85（复审 P1 旁路收口）: 服务端按 body 的 user_id 落改——门禁却只看 URL 段，
		// body 携带他人 user_id 即跨用户改写。非 admin 一律把 body 钉死到 URL 身份，
		// 且自助可变面收窄到 email（role/state/username 强制剥离：state 依赖服务端
		// UNSPECIFIED 保全，username 空=服务端保全，防被停用者自复活/改名撞占 admin）。
		if !isAdmin(r) && req.GetUser() != nil {
			req.User.UserId = rest[0]
			req.User.Role = pb.Role_ROLE_UNSPECIFIED
			req.User.Username = ""
			req.User.State = pb.User_USER_STATE_UNSPECIFIED
		} else if req.GetUser() != nil && req.GetUser().GetUserId() == "" {
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
		// task_id 为 proto L1264 顶层字段（任务↔报告双向导航）；ADR-256: filter/sort 接线
		if err := decodeQuery(r, req, "task_id", "pagination", "filter", "sort"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// ADR-257: 报告列表=审计数据面，认证即读（R90 一期收紧自此反转）
		t.call(w, t.resultConn, "ReportService/ListReports", func(ctx context.Context) (proto.Message, error) {
			return client.ListReports(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodGet:
		// ADR-257: 报告读取=认证即读
		req := &pb.GetReportRequest{ReportId: rest[0]}
		t.call(w, t.resultConn, "ReportService/GetReport", func(ctx context.Context) (proto.Message, error) {
			return client.GetReport(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "download" && r.Method == http.MethodGet:
		// ADR-257: 下载=读面，认证即读
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

// inference — /v1/inference/*（推理 provider/路由管理面，ADR-217）。全路由 admin
// 门禁（serveHTTP 分发处统一 requireAdmin）。透传 RuntimeService → manager →
// OpenShell 网关；workspace 请求级可选（ADR-247 切片3后半：per-team 命名空间
// 配置入口——GET/DELETE 经 ?workspace= query，PUT/POST 经 body 字段；缺省=
// runtime 全局配置注入）；credentials 只进不出（任何响应不回显凭据）。
// 写操作幂等键由网关生成注入（R4）。
func (t *Transcoder) inference(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewRuntimeServiceClient(t.dshConn)
	ws := r.URL.Query().Get("workspace") // ADR-247: 空=全局缺省（与 body 字段二选一，query 优先）
	switch {
	case len(rest) == 1 && rest[0] == "providers" && r.Method == http.MethodGet:
		req := &pb.ListInferenceProvidersRequest{Workspace: ws}
		t.call(w, t.dshConn, "RuntimeService/ListInferenceProviders", func(ctx context.Context) (proto.Message, error) {
			return client.ListInferenceProviders(ctx, req)
		})
	case len(rest) == 1 && rest[0] == "providers" && r.Method == http.MethodPost:
		req := &pb.UpsertInferenceProviderRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if ws != "" {
			req.Workspace = ws // query 覆盖 body（与路径名权威同型）
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.dshConn, "RuntimeService/UpsertInferenceProvider", func(ctx context.Context) (proto.Message, error) {
			return client.UpsertInferenceProvider(ctx, req)
		})
	case len(rest) == 2 && rest[0] == "providers" && r.Method == http.MethodGet:
		req := &pb.GetInferenceProviderRequest{Name: rest[1], Workspace: ws}
		t.call(w, t.dshConn, "RuntimeService/GetInferenceProvider", func(ctx context.Context) (proto.Message, error) {
			return client.GetInferenceProvider(ctx, req)
		})
	case len(rest) == 2 && rest[0] == "providers" && r.Method == http.MethodPut:
		req := &pb.UpsertInferenceProviderRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Name = rest[1] // 路径名权威：覆盖 body 同名字段
		if ws != "" {
			req.Workspace = ws // ADR-247
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.dshConn, "RuntimeService/UpsertInferenceProvider", func(ctx context.Context) (proto.Message, error) {
			return client.UpsertInferenceProvider(ctx, req)
		})
	case len(rest) == 2 && rest[0] == "providers" && r.Method == http.MethodDelete:
		req := &pb.DeleteInferenceProviderRequest{
			Name:      rest[1],
			Workspace: ws, // ADR-247
			Metadata:  &pb.RequestMetadata{RequestId: newRequestID()},
		}
		t.call(w, t.dshConn, "RuntimeService/DeleteInferenceProvider", func(ctx context.Context) (proto.Message, error) {
			return client.DeleteInferenceProvider(ctx, req)
		})
	case len(rest) == 1 && rest[0] == "route" && r.Method == http.MethodGet:
		req := &pb.GetInferenceRouteRequest{Workspace: ws}
		t.call(w, t.dshConn, "RuntimeService/GetInferenceRoute", func(ctx context.Context) (proto.Message, error) {
			return client.GetInferenceRoute(ctx, req)
		})
	case len(rest) == 1 && rest[0] == "route" && r.Method == http.MethodPut:
		req := &pb.SetInferenceRouteRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if ws != "" {
			req.Workspace = ws // query 覆盖 body（与路径名权威同型）
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.dshConn, "RuntimeService/SetInferenceRoute", func(ctx context.Context) (proto.Message, error) {
			return client.SetInferenceRoute(ctx, req)
		})
	default:
		writeError(w, http.StatusNotFound,
			"use GET|POST /v1/inference/providers, GET|PUT|DELETE /v1/inference/providers/{name}, GET|PUT /v1/inference/route")
	}
}

// agents — GET /v1/agents（TP20-T2 切换 Agent 收口）：runtime ListAgents 只读
// 转发（descriptor 注册表投影；读 RPC 无幂等键，03 §2）。清单=静态注册表，无
// 可用性探测（运行期装配校验兜底，Modal 文案如实——计划 §6 不做清单）。
func (t *Transcoder) agents(w http.ResponseWriter, r *http.Request, rest []string) {
	if r.Method == http.MethodGet && len(rest) == 0 {
		client := pb.NewRuntimeServiceClient(t.dshConn)
		t.call(w, t.dshConn, "RuntimeService/ListAgents", func(ctx context.Context) (proto.Message, error) {
			return client.ListAgents(ctx, &pb.ListAgentsRequest{})
		})
		return
	}
	writeError(w, http.StatusNotFound, "use GET /v1/agents")
}

// workspaces — /v1/workspaces（切片3b：team 上车；扩面含建/删区写通道）。
func (t *Transcoder) workspaces(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewRuntimeServiceClient(t.dshConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		req := &pb.CreateWorkspaceRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		t.call(w, t.dshConn, "RuntimeService/CreateWorkspace", func(ctx context.Context) (proto.Message, error) {
			return client.CreateWorkspace(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodDelete:
		req := &pb.DeleteWorkspaceRequest{
			Name:     rest[0], // 路径名权威
			Metadata: &pb.RequestMetadata{RequestId: newRequestID()},
		}
		t.call(w, t.dshConn, "RuntimeService/DeleteWorkspace", func(ctx context.Context) (proto.Message, error) {
			return client.DeleteWorkspace(ctx, req)
		})
	case len(rest) == 0 && r.Method == http.MethodGet:
		limit, offset := 0, 0
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "query 'limit' must be a non-negative integer")
				return
			}
			limit = n
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "query 'offset' must be a non-negative integer")
				return
			}
			offset = n
		}
		req := &pb.ListWorkspacesRequest{Limit: int32(limit), Offset: int32(offset)}
		t.call(w, t.dshConn, "RuntimeService/ListWorkspaces", func(ctx context.Context) (proto.Message, error) {
			return client.ListWorkspaces(ctx, req)
		})
	default:
		writeError(w, http.StatusNotFound, "use GET/POST /v1/workspaces, DELETE /v1/workspaces/{name}")
	}
}

// notifications — NotificationService（storage-service，14号 §7 通知中心）。
func (t *Transcoder) notifications(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewNotificationServiceClient(t.storageConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		// ADR-212: user_id 一律取 JWT 身份（07/14 号通知中心按人分域）——原实现
		// 取自 query 且后端仅校验非空，任何登录用户可读任意用户的通知（IDOR）
		userID, _ := r.Context().Value(middleware.UserIDKey).(string)
		req := &pb.ListNotificationsRequest{UserId: userID}
		if r.URL.Query().Get("unread_only") == "true" {
			req.UnreadOnly = true
		}
		nctx, ncancel := context.WithTimeout(r.Context(), t.callTimeout) // R63
		defer ncancel()
		t.call(w, t.storageConn, "NotificationService/ListNotifications", func(context.Context) (proto.Message, error) {
			return client.ListNotifications(nctx, req)
		})
	case len(rest) == 2 && rest[1] == "read" && r.Method == http.MethodPost:
		// ADR-212: 归属核验前置（零 proto 改动）——MarkNotificationRead 仅凭
		// notification_id 且后端无从获知调用者，任何登录用户此前可标记任意用户
		// 的通知已读（IDOR）。先 List 本人通知校验成员再标记。
		userID, _ := r.Context().Value(middleware.UserIDKey).(string)
		nctx, ncancel := context.WithTimeout(r.Context(), t.callTimeout) // R63: 组合操作超时（原裸 ctx 可悬挂）
		defer ncancel()
		owned, lerr := client.ListNotifications(nctx, &pb.ListNotificationsRequest{UserId: userID})
		if lerr != nil {
			writeError(w, http.StatusBadGateway, "ownership check failed: "+lerr.Error())
			return
		}
		ownedByID := false
		for _, n := range owned.GetNotifications() {
			if n.GetNotificationId() == rest[0] {
				ownedByID = true
				break
			}
		}
		if !ownedByID {
			writeError(w, http.StatusNotFound, "notification not found")
			return
		}
		req := &pb.MarkNotificationReadRequest{NotificationId: rest[0]}
		t.call(w, t.storageConn, "NotificationService/MarkNotificationRead", func(ctx context.Context) (proto.Message, error) {
			return client.MarkNotificationRead(ctx, req)
		})
	case len(rest) == 1 && rest[0] == "read-all" && r.Method == http.MethodPost:
		// ADR-222: 一键全部已读——组合式实现（零 proto 改动，ADR-212 同款就地组合）：
		// List 本人未读（user_id 强制 JWT，归属天然安全）→ 逐条 MarkNotificationRead。
		// 单条失败不整批失败：计数如实返回，残留未读由前端失效重查自然呈现。
		userID, _ := r.Context().Value(middleware.UserIDKey).(string)
		// 第四轮审计 P2：组合操作超时上界（单条 /read 的 ADR-212⑩/R63 同型）——
		// List+N 次串行 Mark 裸用请求 ctx，后端慢时长期悬挂；http.Server 未设
		// WriteTimeout，无任何兜底。
		ctx, cancel := context.WithTimeout(r.Context(), t.callTimeout)
		defer cancel()
		owned, lerr := client.ListNotifications(ctx, &pb.ListNotificationsRequest{UserId: userID, UnreadOnly: true})
		if lerr != nil {
			writeError(w, http.StatusBadGateway, "list unread failed: "+lerr.Error())
			return
		}
		marked := 0
		for _, n := range owned.GetNotifications() {
			if _, err := client.MarkNotificationRead(ctx, &pb.MarkNotificationReadRequest{NotificationId: n.GetNotificationId()}); err == nil {
				marked++
			}
		}
		b, _ := json.Marshal(map[string]int{"marked": marked})
		writeJSON(w, http.StatusOK, b)
	default:
		writeError(w, http.StatusNotFound,
			"use GET /v1/notifications, POST /v1/notifications/read-all, POST /v1/notifications/{id}/read")
	}
}

// ---- invites（V2.2 ADR-255；admin 门禁在域路由入口 requireAdmin） ----

// invites — /v1/invites 面四路由：
//
//	GET    /v1/invites?include_terminal=  → List（缺省仅有效码；config 注入码只读随行）
//	POST   /v1/invites                    → Create（幂等键网关生成）
//	POST   /v1/invites/{id}/revoke        → Revoke（幂等键网关生成）
//	DELETE /v1/invites/{id}               → Delete（终态护栏在 service 层；幂等键网关生成）
func (t *Transcoder) invites(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewUserServiceClient(t.projectConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		req := &pb.ListInviteCodesRequest{}
		if err := decodeQuery(r, req, "pagination", "include_terminal"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.projectConn, "UserService/ListInviteCodes", func(ctx context.Context) (proto.Message, error) {
			return client.ListInviteCodes(ctx, req)
		})
	case len(rest) == 0 && r.Method == http.MethodPost:
		req := &pb.CreateInviteCodeRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Metadata = &pb.RequestMetadata{RequestId: newRequestID()}
		req.CreatedBy = callerID(r) // JWT sub（审查批：多管理员归属，替代服务端恒 "admin"）
		t.call(w, t.projectConn, "UserService/CreateInviteCode", func(ctx context.Context) (proto.Message, error) {
			return client.CreateInviteCode(ctx, req)
		})
	case len(rest) == 2 && rest[1] == "revoke" && r.Method == http.MethodPost:
		req := &pb.RevokeInviteCodeRequest{Id: rest[0], Metadata: &pb.RequestMetadata{RequestId: newRequestID()}}
		t.call(w, t.projectConn, "UserService/RevokeInviteCode", func(ctx context.Context) (proto.Message, error) {
			return client.RevokeInviteCode(ctx, req)
		})
	case len(rest) == 1 && r.Method == http.MethodDelete:
		req := &pb.DeleteInviteCodeRequest{Id: rest[0], Metadata: &pb.RequestMetadata{RequestId: newRequestID()}}
		t.call(w, t.projectConn, "UserService/DeleteInviteCode", func(ctx context.Context) (proto.Message, error) {
			return client.DeleteInviteCode(ctx, req)
		})
	default:
		writeError(w, http.StatusNotFound, "invites: GET /v1/invites | POST /v1/invites | POST /v1/invites/{id}/revoke | DELETE /v1/invites/{id}")
	}
}

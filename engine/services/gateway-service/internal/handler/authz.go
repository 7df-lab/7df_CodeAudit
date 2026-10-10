package handler

// R89-R91: 资源归属授权（水平越权收口，设计提案获批 2026-09-13）。
// ADR-257（2026-10-10 RBAC 层级制决议）部分反转：审计数据读面（projects/reports/
// findings/tasks 的列表与详情）放开为认证即读；项目 create/update 放宽为操作面
// DEVELOPER+。写面判定（verdict 回写=owner 或 admin；项目删除/管理面=admin）不随
// 读面反转，R89-R91 的归属判定就此收敛为写面专用。
// 纪律（吸收 R73 教训）：归属判定只用路径参数，禁信 body 字段；拒绝统一 403
// writeError + 审计日志（侦测面：谁在何时探谁的资源）；查询失败保守拒绝。

import (
	"context"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/gateway-service/internal/middleware"
)

// authzLookupTimeout — 归属查证的单次 RPC 上限（resource read 亚毫秒级内存查询；
// 定值无 07 基线，取舍见 ADR-230/231）。
const authzLookupTimeout = 5 * time.Second

// callerID — JWT sub（UserIDKey 由 JWTMiddleware 注入；缺失=旧令牌/内部直调）。
func callerID(r *http.Request) string {
	uid, _ := r.Context().Value(middleware.UserIDKey).(string)
	return uid
}

// denyAuthz — 统一拒绝：403 + 审计日志（探测行为留痕）。
func denyAuthz(w http.ResponseWriter, r *http.Request, resource string) {
	log.Printf("[authz] deny user=%q resource=%s path=%s", callerID(r), resource, r.URL.Path)
	writeError(w, http.StatusForbidden, "resource owner or admin required")
}

// isOperator — ADR-257 操作面角色：DEVELOPER 或 ADMIN（VIEWER 只读审计数据面）。
func isOperator(r *http.Request) bool {
	role, _ := r.Context().Value(middleware.UserRoleKey).(string)
	return role == "ROLE_DEVELOPER" || role == "ROLE_ADMIN"
}

// requireOperator — 操作面门禁（项目创建/更新、任务生命周期、图编排互动、报告
// 生成）：DEVELOPER+；VIEWER 拒。403 + 审计日志（与 denyAuthz 同型）。
func requireOperator(w http.ResponseWriter, r *http.Request, resource string) bool {
	if isOperator(r) {
		return true
	}
	log.Printf("[authz] deny operator-only user=%q role-missing resource=%s path=%s", callerID(r), resource, r.URL.Path)
	writeError(w, http.StatusForbidden, "operator role (developer or admin) required")
	return false
}

// canAccessTask — R89→ADR-257: 任务域写面归属门禁（verdict 回写专用路径；owner
// 或 admin）。读面已放开为认证即读。NotFound 如实 404；查证失败保守拒绝（403/502）。
func (t *Transcoder) canAccessTask(w http.ResponseWriter, r *http.Request, taskID string) bool {
	if isAdmin(r) {
		return true
	}
	uid := callerID(r)
	if uid == "" {
		denyAuthz(w, r, "task/"+taskID)
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), authzLookupTimeout)
	defer cancel()
	resp, err := pb.NewTaskServiceClient(t.taskConn).GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: taskID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			writeError(w, http.StatusNotFound, "task not found: "+taskID)
			return false
		}
		log.Printf("[authz] task lookup failed task=%s: %v", taskID, err)
		denyAuthz(w, r, "task/"+taskID)
		return false
	}
	if resp.GetCreatedBy() != uid {
		denyAuthz(w, r, "task/"+taskID)
		return false
	}
	return true
}

// canAccessTaskViaFinding — R90→ADR-257: finding 域写面（verdict）按 ID 访问 →
// 解析 task_id → 任务归属。读面已放开。
func (t *Transcoder) canAccessTaskViaFinding(w http.ResponseWriter, r *http.Request, findingID string) bool {
	if isAdmin(r) {
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), authzLookupTimeout)
	defer cancel()
	resp, err := pb.NewResultServiceClient(t.resultConn).GetFinding(ctx, &pb.GetFindingRequest{FindingId: findingID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			writeError(w, http.StatusNotFound, "finding not found: "+findingID)
			return false
		}
		log.Printf("[authz] finding lookup failed finding=%s: %v", findingID, err)
		denyAuthz(w, r, "finding/"+findingID)
		return false
	}
	return t.canAccessTask(w, r, resp.GetFinding().GetTaskId())
}

// canAccessTaskViaReport — R90→ADR-257: 报告域写面按 ID 访问 → 解析 task_id →
// 任务归属。读面已放开。
func (t *Transcoder) canAccessTaskViaReport(w http.ResponseWriter, r *http.Request, reportID string) bool {
	if isAdmin(r) {
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), authzLookupTimeout)
	defer cancel()
	resp, err := pb.NewReportServiceClient(t.resultConn).GetReport(ctx, &pb.GetReportRequest{ReportId: reportID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			writeError(w, http.StatusNotFound, "report not found: "+reportID)
			return false
		}
		log.Printf("[authz] report lookup failed report=%s: %v", reportID, err)
		denyAuthz(w, r, "report/"+reportID)
		return false
	}
	return t.canAccessTask(w, r, resp.GetTaskId())
}

// canBatchVerdict — R90: 批量 triage 与单条 verdict 同口径（逐 finding → task 归属，
// 去重后逐个查证，首个拒绝即短路）——单条收口而批量直通即整条门禁的旁路。
// 空批次不在此拦（交服务端语义）。
func (t *Transcoder) canBatchVerdict(w http.ResponseWriter, r *http.Request, findingIDs []string) bool {
	if isAdmin(r) {
		return true
	}
	seen := make(map[string]struct{}, len(findingIDs))
	for _, id := range findingIDs {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if !t.canAccessTaskViaFinding(w, r, id) {
			return false
		}
	}
	return true
}

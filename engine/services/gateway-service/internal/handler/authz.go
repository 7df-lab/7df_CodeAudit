package handler

// R89-R91: 资源归属授权（水平越权收口，设计提案获批 2026-09-13）。
// 口径：admin 全权；其余按资源 owner（任务 created_by）或项目成员判定。
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

// canAccessTask — R89: 任务域归属门禁（owner 或 admin）。NotFound 如实 404；
// 查证失败保守拒绝（403/502），不放行。
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

// canAccessProject — R91: 项目域门禁。读=成员或 admin；写（更新/删除/配置变更）=
// admin（一期口径：项目写面=管理面，成员写力如需下放另立裁决）。NotFound 404；
// 查证失败保守拒绝。
func (t *Transcoder) canAccessProject(w http.ResponseWriter, r *http.Request, projectID string, write bool) bool {
	if isAdmin(r) {
		return true
	}
	uid := callerID(r)
	if uid == "" {
		denyAuthz(w, r, "project/"+projectID)
		return false
	}
	if write {
		// 一期写面收紧为管理面（此前任意认证用户可改/删任意项目）
		log.Printf("[authz] deny project write user=%q project=%s path=%s", uid, projectID, r.URL.Path)
		writeError(w, http.StatusForbidden, "project write requires admin")
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), authzLookupTimeout)
	defer cancel()
	resp, err := pb.NewProjectServiceClient(t.projectConn).ListProjectMembers(ctx, &pb.ListProjectMembersRequest{ProjectId: projectID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			writeError(w, http.StatusNotFound, "project not found: "+projectID)
			return false
		}
		log.Printf("[authz] project members lookup failed project=%s: %v", projectID, err)
		denyAuthz(w, r, "project/"+projectID)
		return false
	}
	for _, m := range resp.GetMembers() {
		if m.GetUserId() == uid {
			return true
		}
	}
	denyAuthz(w, r, "project/"+projectID)
	return false
}

// canAccessTaskViaFinding — R90: finding 域按 ID 访问 → 解析 task_id → 任务归属。
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

// canAccessTaskViaReport — R90: 报告域按 ID 访问 → 解析 task_id → 任务归属。
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

// gateTaskScopedList — R89/R90: 任务维度列表接口（findings/reports list 带 task_id）
// 的归属门禁；不带 task_id 的裸列表=全量枚举面，一期收紧为 admin（owner 经 ID/自己
// 的 task_id 访问；服务端 owner 过滤能力立项后可放宽——设计提案 §一.二期）。
func (t *Transcoder) gateTaskScopedList(w http.ResponseWriter, r *http.Request, taskID string) bool {
	if taskID != "" {
		return t.canAccessTask(w, r, taskID)
	}
	if isAdmin(r) {
		return true
	}
	denyAuthz(w, r, r.URL.Path+" (unscoped list)")
	return false
}

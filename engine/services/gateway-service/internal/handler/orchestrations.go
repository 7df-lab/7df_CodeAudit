package handler

// orchestrations.go — 编排（负载 pack）生命周期 REST 面（编排 CRUD 批 2026-09-21；
// 人类裁决立项）。读=登录即可（编排目录为主导航页数据源）；写（建/更/停）=admin
// （负载定义属产品基线管理面，与推理 Provider 管理同档）。
// 后端=TaskService 编排生命周期 RPC（append-only 修订语义，见 proto 注）。

import (
	"context"
	"net/http"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/gateway-service/internal/middleware"
)

// callerCtx — 身份贯通：JWT sub（jwt middleware 注入 r.Context）→ gRPC outgoing
// metadata x-user-id（engine createdByFromCtx 消费=编排修订 created_by）。ADR-199
// 任务链用 req.CreatedBy 字段注入同源；编排链选 metadata 免 proto 契约面扩张。
// 无身份（未过鉴权面的内部调用）=不注入，engine 侧诚实降级为空串。
func callerCtx(r *http.Request, ctx context.Context) context.Context {
	if uid, ok := r.Context().Value(middleware.UserIDKey).(string); ok && uid != "" {
		return metadata.AppendToOutgoingContext(ctx, "x-user-id", uid)
	}
	return ctx
}

func (t *Transcoder) orchestrations(w http.ResponseWriter, r *http.Request, rest []string) {
	client := pb.NewTaskServiceClient(t.taskConn)
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		// 目录（登录即可——主导航页数据源）；默认过滤停用
		req := &pb.ListOrchestrationsRequest{
			IncludeDeprecated: r.URL.Query().Get("include_deprecated") == "true",
			IncludeHistory:    r.URL.Query().Get("include_history") == "true",
		}
		t.call(w, t.taskConn, "TaskService/ListOrchestrations", func(ctx context.Context) (proto.Message, error) {
			return client.ListOrchestrations(ctx, req)
		})

	case len(rest) == 0 && r.Method == http.MethodPost:
		// 新建编排（admin）
		if !isAdmin(r) {
			denyAuthz(w, r, "orchestrations (create)")
			return
		}
		req := &pb.CreateOrchestrationRequest{}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.call(w, t.taskConn, "TaskService/CreateOrchestration", func(ctx context.Context) (proto.Message, error) {
			return client.CreateOrchestration(callerCtx(r, ctx), req)
		})

	case len(rest) == 1 && r.Method == http.MethodPut:
		// 更新编排 = append 新修订（admin）
		if !isAdmin(r) {
			denyAuthz(w, r, "orchestrations (update)")
			return
		}
		req := &pb.UpdateOrchestrationRequest{Name: rest[0]}
		if err := decodeBody(r, req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Name = rest[0] // 路径段权威：body 携带 name 不得覆盖（protojson Unmarshal 会写字段）
		t.call(w, t.taskConn, "TaskService/UpdateOrchestration", func(ctx context.Context) (proto.Message, error) {
			return client.UpdateOrchestration(callerCtx(r, ctx), req)
		})

	case len(rest) == 1 && r.Method == http.MethodDelete:
		// 停用/恢复编排（admin）——?unde_precate=true 恢复
		if !isAdmin(r) {
			denyAuthz(w, r, "orchestrations (deprecate)")
			return
		}
		req := &pb.DeprecateOrchestrationRequest{
			Name:        rest[0],
			UndePrecate: r.URL.Query().Get("unde_precate") == "true",
		}
		t.call(w, t.taskConn, "TaskService/DeprecateOrchestration", func(ctx context.Context) (proto.Message, error) {
			return client.DeprecateOrchestration(ctx, req)
		})

	default:
		writeError(w, http.StatusNotFound, "unknown orchestrations endpoint (GET/POST /v1/orchestrations, PUT/DELETE /v1/orchestrations/{name})")
	}
}

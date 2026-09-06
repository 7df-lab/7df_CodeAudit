package handler

// UserService gRPC 适配层：V2.1 (ADR-205) 起 13 RPC——原 8 个 + 用户生命周期五接口。
// 写 RPC 幂等模式与 ProjectHandler.CreateProject 一致（idm 缓存 + bodyHash）。

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/idempotency"
	"github.com/codeaudit/services/project-service/internal/service"
)

// UserHandler implements v1.UserServiceServer.
type UserHandler struct {
	v1.UnimplementedUserServiceServer
	svc *service.UserService
	idm *idempotency.Store // V2.1 (ADR-205)：写 RPC 幂等
}

// NewUserHandler creates a UserHandler.
func NewUserHandler(svc *service.UserService, idm *idempotency.Store) *UserHandler {
	return &UserHandler{svc: svc, idm: idm}
}

// ---- UserService RPCs (13, V2.1) ----

// Login — returns JWT tokens (HS256, secret from CODEAUDIT_JWT_SECRET, 03 §4).
func (h *UserHandler) Login(ctx context.Context, req *v1.LoginRequest) (*v1.LoginResponse, error) {
	resp, err := h.svc.Login(req.GetUsername(), req.GetPassword())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "login failed: %v", err)
	}
	return resp, nil
}

// Logout — invalidates the access token.
func (h *UserHandler) Logout(ctx context.Context, req *v1.LogoutRequest) (*emptypb.Empty, error) {
	if req.GetAccessToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token is required")
	}
	h.svc.Logout(req.GetAccessToken())
	return &emptypb.Empty{}, nil
}

// RefreshToken — validates a refresh token and issues new tokens.
func (h *UserHandler) RefreshToken(ctx context.Context, req *v1.RefreshTokenRequest) (*v1.RefreshTokenResponse, error) {
	if req.GetRefreshToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "refresh_token is required")
	}
	resp, err := h.svc.RefreshToken(req.GetRefreshToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "refresh failed: %v", err)
	}
	return resp, nil
}

// GetCurrentUser — extracts user info from access token.
func (h *UserHandler) GetCurrentUser(ctx context.Context, req *v1.GetCurrentUserRequest) (*v1.User, error) {
	if req.GetAccessToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token is required")
	}
	user, err := h.svc.GetCurrentUser(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
	}
	return user, nil
}

// GetUser — returns a user by ID.
func (h *UserHandler) GetUser(ctx context.Context, req *v1.GetUserRequest) (*v1.User, error) {
	user, ok := h.svc.GetUser(req.GetUserId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "user %s not found", req.GetUserId())
	}
	return user, nil
}

// UpdateUser — updates user fields.
func (h *UserHandler) UpdateUser(ctx context.Context, req *v1.UpdateUserRequest) (*v1.User, error) {
	user, ok := h.svc.UpdateUser(req.GetUser())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "user %s not found", req.GetUser().GetUserId())
	}
	return user, nil
}

// ValidatePermission — checks if a user has the specified permission.
func (h *UserHandler) ValidatePermission(ctx context.Context, req *v1.ValidatePermissionRequest) (*v1.ValidatePermissionResponse, error) {
	allowed, reason := h.svc.ValidatePermission(
		req.GetUserId(),
		req.GetResourceType(),
		req.GetResourceId(),
		req.GetAction(),
	)
	return &v1.ValidatePermissionResponse{
		Allowed: allowed,
		Reason:  reason,
	}, nil
}

// GetUserPermissions — returns the permission list for a user.
func (h *UserHandler) GetUserPermissions(ctx context.Context, req *v1.GetUserPermissionsRequest) (*v1.UserPermissions, error) {
	perms := h.svc.GetUserPermissions(req.GetUserId())
	return &v1.UserPermissions{
		UserId:      req.GetUserId(),
		Permissions: perms,
	}, nil
}

// ---- V2.1 用户生命周期 (ADR-205)。写 RPC 幂等模式与 ProjectHandler.CreateProject 一致 ----

// withIdempotency — 幂等泛型封装：metadata 必带（03 §2）；同 request_id+body 命中缓存
// 直接回放首次响应（RegisterUser/ResetPassword 的令牌与临时密码重放不变，R4/R6）。
func withIdempotency[T any](idm *idempotency.Store, req *v1.RequestMetadata, body interface{}, exec func() (*T, error)) (*T, error) {
	if req == nil || req.GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.request_id is required (03 §2)")
	}
	bodyBytes, _ := json.Marshal(body)
	bodyHash := idempotency.BodyHash(bodyBytes)
	cached, err := idm.Check(req.GetRequestId(), bodyHash)
	if err != nil {
		return nil, status.Errorf(codes.AlreadyExists, "idempotency conflict: %v", err)
	}
	if cached != nil {
		var out T
		if jerr := json.Unmarshal(cached.ResponseBytes, &out); jerr == nil {
			return &out, nil
		}
		// 缓存反序列化失败回落重新执行（诚实降级，不伪装成功）
	}
	resp, err := exec()
	if err != nil {
		return nil, err
	}
	resultBytes, _ := json.Marshal(resp)
	idm.Save(req.GetRequestId(), bodyHash, &idempotency.Result{ResponseBytes: resultBytes})
	return resp, nil
}

// RegisterUser — 自助注册（幂等）；成功签发令牌对（注册即登录）。
func (h *UserHandler) RegisterUser(ctx context.Context, req *v1.RegisterUserRequest) (*v1.LoginResponse, error) {
	return withIdempotency(h.idm, req.GetMetadata(), map[string]string{
		"username": req.GetUsername(), "email": req.GetEmail(), "password": req.GetPassword(), "invite_code": req.GetInviteCode(),
	}, func() (*v1.LoginResponse, error) {
		user, err := h.svc.RegisterUser(req.GetUsername(), req.GetEmail(), req.GetPassword(), req.GetInviteCode())
		if err != nil {
			return nil, err
		}
		resp, lerr := h.svc.Login(user.GetUsername(), req.GetPassword())
		if lerr != nil {
			return nil, status.Errorf(codes.Internal, "post-register login: %v", lerr)
		}
		return resp, nil
	})
}

// ListUsers — 管理端列表（网关 admin 门禁；分页同 ListProjects offset 游标口径）。
func (h *UserHandler) ListUsers(ctx context.Context, req *v1.ListUsersRequest) (*v1.ListUsersResponse, error) {
	recs := h.svc.ListUsers(req.GetState(), req.GetUsernameContains())

	offset := 0
	if req.GetPagination() != nil && req.GetPagination().GetCursor() != "" {
		if _, err := fmt.Sscanf(req.GetPagination().GetCursor(), "%d", &offset); err != nil || offset < 0 {
			return nil, status.Error(codes.InvalidArgument, "invalid cursor (03 §5)")
		}
	}
	pageSize := 20
	if req.GetPagination() != nil && req.GetPagination().GetPageSize() > 0 {
		pageSize = int(req.GetPagination().GetPageSize())
		if pageSize > 100 {
			pageSize = 100
		}
	}

	end := offset + pageSize
	if end > len(recs) {
		end = len(recs)
	}
	users := make([]*v1.User, 0, end-offset)
	for _, rec := range recs[offset:end] {
		users = append(users, rec.User)
	}

	resp := &v1.ListUsersResponse{Users: users}
	if end < len(recs) {
		resp.Pagination = &v1.PaginationResponse{
			NextCursor: fmt.Sprintf("%d", end),
			HasNext:    true,
			Total:      int32(len(recs)),
		}
	} else {
		resp.Pagination = &v1.PaginationResponse{Total: int32(len(recs))}
	}
	return resp, nil
}

// CreateUser — 管理员直建（幂等），must_change_password 置真。
func (h *UserHandler) CreateUser(ctx context.Context, req *v1.CreateUserRequest) (*v1.User, error) {
	return withIdempotency(h.idm, req.GetMetadata(), map[string]string{
		"username": req.GetUsername(), "email": req.GetEmail(), "password": req.GetPassword(), "role": req.GetRole().String(),
	}, func() (*v1.User, error) {
		return h.svc.CreateUser(req.GetUsername(), req.GetEmail(), req.GetPassword(), req.GetRole())
	})
}

// ChangePassword — 自助改密（幂等；同 request_id 重放不再二次校验旧密码）。
func (h *UserHandler) ChangePassword(ctx context.Context, req *v1.ChangePasswordRequest) (*emptypb.Empty, error) {
	return withIdempotency(h.idm, req.GetMetadata(), map[string]string{
		"user_id": req.GetUserId(), "old_password": req.GetOldPassword(), "new_password": req.GetNewPassword(),
	}, func() (*emptypb.Empty, error) {
		if err := h.svc.ChangePassword(req.GetUserId(), req.GetOldPassword(), req.GetNewPassword()); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	})
}

// ResetPassword — 管理员重置（幂等；临时密码同 request_id 重放返回同一值）。
func (h *UserHandler) ResetPassword(ctx context.Context, req *v1.ResetPasswordRequest) (*v1.ResetPasswordResponse, error) {
	return withIdempotency(h.idm, req.GetMetadata(), map[string]string{
		"user_id": req.GetUserId(),
	}, func() (*v1.ResetPasswordResponse, error) {
		temp, err := h.svc.ResetPassword(req.GetUserId())
		if err != nil {
			return nil, err
		}
		return &v1.ResetPasswordResponse{TemporaryPassword: temp, MustChangePassword: true}, nil
	})
}

// Package handler implements gRPC handler adapters for ProjectService (11 RPCs)
// and UserService (8 RPCs). Handlers translate between gRPC request/response
// types and the business logic layer in service/.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/idempotency"
	"github.com/codeaudit/services/project-service/internal/service"
)

// ProjectHandler implements v1.ProjectServiceServer.
type ProjectHandler struct {
	v1.UnimplementedProjectServiceServer
	svc *service.ProjectService
	idm *idempotency.Store
}

// NewProjectHandler creates a ProjectHandler.
func NewProjectHandler(svc *service.ProjectService, idm *idempotency.Store) *ProjectHandler {
	return &ProjectHandler{svc: svc, idm: idm}
}

// ---- ProjectService RPCs (11) ----

// CreateProject — write RPC, has RequestMetadata.metadata, must implement idempotency (03 §2).
func (h *ProjectHandler) CreateProject(ctx context.Context, req *v1.CreateProjectRequest) (*v1.Project, error) {
	// R4: read RequestMetadata for idempotency
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.request_id is required (03 §2)")
	}

	bodyBytes, _ := json.Marshal(req.GetProject())
	bodyHash := idempotency.BodyHash(bodyBytes)

	cached, err := h.idm.Check(req.GetMetadata().GetRequestId(), bodyHash)
	if err != nil {
		return nil, status.Errorf(codes.AlreadyExists, "idempotency conflict: %v", err)
	}
	if cached != nil {
		// Replay cached response
		var proj v1.Project
		if err := json.Unmarshal(cached.ResponseBytes, &proj); err == nil {
			return &proj, nil
		}
		// fallback: re-execute
	}

	project := h.svc.Create(req.GetProject())

	// Cache the result
	resultBytes, _ := json.Marshal(project)
	h.idm.Save(req.GetMetadata().GetRequestId(), bodyHash, &idempotency.Result{
		ResponseBytes: resultBytes,
	})

	return project, nil
}

// GetProject — read RPC.
func (h *ProjectHandler) GetProject(ctx context.Context, req *v1.GetProjectRequest) (*v1.Project, error) {
	proj, ok := h.svc.Get(req.GetProjectId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "project %s not found", req.GetProjectId())
	}
	return proj, nil
}

// UpdateProject — write RPC (no RequestMetadata in proto).
func (h *ProjectHandler) UpdateProject(ctx context.Context, req *v1.UpdateProjectRequest) (*v1.Project, error) {
	proj, ok := h.svc.Update(req.GetProject())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "project %s not found", req.GetProject().GetProjectId())
	}
	return proj, nil
}

// DeleteProject — write RPC (no RequestMetadata in proto).
func (h *ProjectHandler) DeleteProject(ctx context.Context, req *v1.DeleteProjectRequest) (*emptypb.Empty, error) {
	if !h.svc.Delete(req.GetProjectId()) {
		return nil, status.Errorf(codes.NotFound, "project %s not found", req.GetProjectId())
	}
	return &emptypb.Empty{}, nil
}

// ListProjects — read RPC.
func (h *ProjectHandler) ListProjects(ctx context.Context, req *v1.ListProjectsRequest) (*v1.ListProjectsResponse, error) {
	all := h.svc.List()

	// ADR-161: 与任务/报告列表同一"最新活动优先"口径（ADR-149）——此前按存储插入序
	// （最老在前），项目数超过前端分页大小时新建项目落在末页，用户"创建成功却看不见"。
	// created_at 降序 + project_id 决胜保证稳定序（03 §5）。
	sort.Slice(all, func(i, j int) bool {
		ci, cj := all[i].GetCreatedAt().AsTime(), all[j].GetCreatedAt().AsTime()
		if !ci.Equal(cj) {
			return ci.After(cj)
		}
		return all[i].GetProjectId() > all[j].GetProjectId()
	})

	// Simple offset-based pagination using cursor as index
	pageSize := int32(20)
	if req.GetPagination() != nil && req.GetPagination().GetPageSize() > 0 {
		pageSize = req.GetPagination().GetPageSize()
		if pageSize > 100 {
			pageSize = 100 // 07 §10 max page size
		}
	}

	offset := 0
	if req.GetPagination() != nil && req.GetPagination().GetCursor() != "" {
		if cur := req.GetPagination().GetCursor(); cur != "" {
			n, err := strconv.Atoi(cur)
			if err != nil || n < 0 {
				return nil, status.Error(codes.InvalidArgument, "invalid cursor (03 §5)")
			}
			offset = n
		}
	}

	total := int32(len(all))
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + int(pageSize)
	if end > len(all) {
		end = len(all)
	}
	page := all[offset:end]

	nextCursor := ""
	hasNext := end < len(all)
	if hasNext {
		nextCursor = fmt.Sprintf("%d", end)
	}

	return &v1.ListProjectsResponse{
		Projects: page,
		Pagination: &v1.PaginationResponse{
			NextCursor: nextCursor,
			HasNext:    hasNext,
			Total:      total,
		},
	}, nil
}

// GetProjectConfig — read RPC.
func (h *ProjectHandler) GetProjectConfig(ctx context.Context, req *v1.GetProjectConfigRequest) (*v1.ProjectConfig, error) {
	cfg, ok := h.svc.GetConfig(req.GetProjectId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "config for project %s not found", req.GetProjectId())
	}
	return cfg, nil
}

// UpdateProjectConfig — write RPC (no RequestMetadata).
// ADR-135: 此前可为不存在的项目写 config；先校验项目存在。

func (h *ProjectHandler) UpdateProjectConfig(ctx context.Context, req *v1.UpdateProjectConfigRequest) (*v1.ProjectConfig, error) {
	// ADR-135: 校验项目存在（此前可为不存在的项目写 config）
	if _, ok := h.svc.Get(req.GetConfig().GetProjectId()); !ok {
		return nil, status.Errorf(codes.NotFound, "project %s not found", req.GetConfig().GetProjectId())
	}
	return h.svc.UpdateConfig(req.GetConfig()), nil
}

// GetProjectStats — read RPC.
func (h *ProjectHandler) GetProjectStats(ctx context.Context, req *v1.GetProjectStatsRequest) (*v1.ProjectStats, error) {
	stats, ok := h.svc.GetStats(req.GetProjectId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "stats for project %s not found", req.GetProjectId())
	}
	return stats, nil
}

// AddProjectMember — write RPC, has RequestMetadata.metadata, must implement idempotency.
func (h *ProjectHandler) AddProjectMember(ctx context.Context, req *v1.AddProjectMemberRequest) (*v1.ProjectMember, error) {
	// R4: read RequestMetadata for idempotency
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.request_id is required (03 §2)")
	}

	bodyBytes, _ := json.Marshal(req.GetMember())
	bodyHash := idempotency.BodyHash(bodyBytes)

	cached, err := h.idm.Check(req.GetMetadata().GetRequestId(), bodyHash)
	if err != nil {
		return nil, status.Errorf(codes.AlreadyExists, "idempotency conflict: %v", err)
	}
	if cached != nil {
		var member v1.ProjectMember
		if err := json.Unmarshal(cached.ResponseBytes, &member); err == nil {
			return &member, nil
		}
	}

	member := h.svc.AddMember(req.GetMember())

	resultBytes, _ := json.Marshal(member)
	h.idm.Save(req.GetMetadata().GetRequestId(), bodyHash, &idempotency.Result{
		ResponseBytes: resultBytes,
	})

	return member, nil
}

// RemoveProjectMember — write RPC (no RequestMetadata).
func (h *ProjectHandler) RemoveProjectMember(ctx context.Context, req *v1.RemoveProjectMemberRequest) (*emptypb.Empty, error) {
	h.svc.RemoveMember(req.GetProjectId(), req.GetUserId())
	return &emptypb.Empty{}, nil
}

// ListProjectMembers — read RPC.
func (h *ProjectHandler) ListProjectMembers(ctx context.Context, req *v1.ListProjectMembersRequest) (*v1.ListProjectMembersResponse, error) {
	members := h.svc.ListMembers(req.GetProjectId())

	pageSize := int32(20)
	if req.GetPagination() != nil && req.GetPagination().GetPageSize() > 0 {
		pageSize = req.GetPagination().GetPageSize()
		if pageSize > 100 {
			pageSize = 100
		}
	}

	offset := 0
	if req.GetPagination() != nil && req.GetPagination().GetCursor() != "" {
		if cur := req.GetPagination().GetCursor(); cur != "" {
			n, err := strconv.Atoi(cur)
			if err != nil || n < 0 {
				return nil, status.Error(codes.InvalidArgument, "invalid cursor (03 §5)")
			}
			offset = n
		}
	}

	total := int32(len(members))
	if offset > len(members) {
		offset = len(members)
	}
	end := offset + int(pageSize)
	if end > len(members) {
		end = len(members)
	}
	page := members[offset:end]

	nextCursor := ""
	hasNext := end < len(members)
	if hasNext {
		nextCursor = fmt.Sprintf("%d", end)
	}

	return &v1.ListProjectMembersResponse{
		Members: page,
		Pagination: &v1.PaginationResponse{
			NextCursor: nextCursor,
			HasNext:    hasNext,
			Total:      total,
		},
	}, nil
}

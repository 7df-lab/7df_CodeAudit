// V2.2 (ADR-255): 邀请码管理业务逻辑——Create/List/Revoke/Delete + 注册消费合并校验
// + 注册策略只读。语义裁决源：grill 会 Q1-Q14（伞仓 .agent/plans/invite-mgmt-plan-2026-10.md）。
// 数值以 07 §7.1（ADR-255 提案行）为唯一事实源。
package service

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
)

// 邀请码策略常量（07 §7.1，ADR-255 提案行；改动须先改 07 再同步此处引用）。
const (
	inviteDefaultTTLDays = 30 // 未设 expires_at 且未显式永不过期时的缺省有效期
	inviteCodeChars      = 10 // 码体字符数（展示为 CODEAUDIT-XXXXX-XXXXX）
	inviteRetryLimit     = 5  // 撞键重试上限（07 §7.1；code 表 UNIQUE 兜底）
)

// inviteCodeAlphabet — Crockford Base32 去 I/L/O/U（防人工转写歧义）。
const inviteCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// randomFromAlphabet — crypto/rand 均匀取样（generateTempPassword 同型，参数化字母表）。
func randomFromAlphabet(n int, alphabet string) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("rand int: %w", err)
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}

// generateInviteCode — CODEAUDIT-XXXXX-XXXXX（系统生成；管理员不可自定义，Q3）。
func generateInviteCode() (string, error) {
	raw, err := randomFromAlphabet(inviteCodeChars, inviteCodeAlphabet)
	if err != nil {
		return "", err
	}
	return "CODEAUDIT-" + raw[:5] + "-" + raw[5:], nil
}

// CreateInviteCode — admin 生成。三值语义（proto CreateInviteCodeRequest）：
// max_uses 未设=缺省 1（一次性）/0=不限；expires_at 未设且非永久=now+30 天（07 §7.1）/
// never_expires 显式永久；二者同设=INVALID_ARGUMENT。role（ADR-257）：仅 DEVELOPER|
// VIEWER；UNSPECIFIED=按 DEVELOPER；ADMIN 禁发（admin 经控制台直建，防自助注册自封）。
// 写穿镜像 best-effort（ADR-250 同口径），跨副本撞码（PG UNIQUE）重试。
func (s *UserService) CreateInviteCode(maxUses int32, hasMaxUses bool, expiresAt *timestamppb.Timestamp, neverExpires bool, note, createdBy string, role v1.Role) (*v1.InviteCode, error) {
	if maxUses < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_uses must be >= 0 (0 = unlimited)")
	}
	switch role {
	case v1.Role_ROLE_UNSPECIFIED, v1.Role_ROLE_DEVELOPER:
		role = v1.Role_ROLE_DEVELOPER // 缺省 DEVELOPER（ADR-257 Q6）
	case v1.Role_ROLE_VIEWER:
	default:
		return nil, status.Error(codes.InvalidArgument, "invite role must be DEVELOPER or VIEWER (admin is created via admin console)")
	}
	if !hasMaxUses {
		maxUses = 1 // 缺省一次性（07 §7.1）
	}
	now := time.Now()
	var expiry *timestamppb.Timestamp
	switch {
	case neverExpires && expiresAt != nil:
		return nil, status.Error(codes.InvalidArgument, "never_expires and expires_at are mutually exclusive")
	case neverExpires:
		// 显式永久（Q2）
	case expiresAt != nil:
		if !expiresAt.AsTime().After(now) {
			return nil, status.Error(codes.InvalidArgument, "expires_at must be in the future")
		}
		expiry = expiresAt
	default:
		expiry = timestamppb.New(now.AddDate(0, 0, inviteDefaultTTLDays)) // 07 §7.1
	}
	for attempt := 0; attempt < inviteRetryLimit; attempt++ {
		code, err := generateInviteCode()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "generate invite code: %v", err)
		}
		rec := &v1.InviteCode{
			Id:        "invite-" + generateID(),
			Code:      code,
			MaxUses:   maxUses,
			ExpiresAt: expiry,
			State:     "active",
			Note:      note,
			CreatedBy: createdBy,
			CreatedAt: timestamppb.New(now),
			Role:      role,
		}
		if !s.store.CreateInviteCode(rec) {
			continue // 本副本撞码
		}
		if s.inviteMirror != nil {
			if err := s.inviteMirror.UpsertInviteCode(rec); err != nil {
				if pgUniqueViolation(err) {
					s.store.DeleteInviteCode(rec.GetId()) // 回滚本地，重试新码
					continue
				}
				mirrorErr("invite-create", rec.GetId(), err)
			}
		}
		return rec, nil
	}
	return nil, status.Error(codes.Internal, "invite code collision retry exhausted")
}

// ListInviteCodes — 管理端列表（分页在 handler 层）；includeTerminal=false 时仅有效码
// （Q11 默认筛）。静态配置注入码随行只读返回（Q4/Q13 bootstrap 层，不入库不管理）。
func (s *UserService) ListInviteCodes(includeTerminal bool) ([]*v1.InviteCode, []string) {
	now := time.Now()
	var out []*v1.InviteCode
	for _, rec := range s.store.ListInviteCodes() {
		if !includeTerminal && repo.InviteTerminal(rec, now) {
			continue
		}
		out = append(out, rec)
	}
	var configCodes []string
	for _, c := range s.auth.InviteCodes {
		if c != "" {
			configCodes = append(configCodes, c)
		}
	}
	return out, configCodes
}

// inviteCodeByID — id 寻址（内存 miss 回落镜像；跨副本建的码可被吊销/删除，ADR-250 读回落同型）。
func (s *UserService) inviteCodeByID(id string) (*v1.InviteCode, bool) {
	if rec, ok := s.store.GetInviteCodeByID(id); ok {
		return rec, true
	}
	if s.inviteMirror != nil {
		if all, err := s.inviteMirror.ListInviteCodes(); err == nil {
			for _, r := range all {
				if r.GetId() == id {
					s.store.PutInviteCode(r)
					return r, true
				}
			}
		} else {
			mirrorErr("invite-lookup", id, err)
		}
	}
	return nil, false
}

// RevokeInviteCode — 状态翻转非删除（活跃码唯一下线通道，Q11）；重复吊销幂等返回原码。
func (s *UserService) RevokeInviteCode(id string) (*v1.InviteCode, error) {
	rec, ok := s.inviteCodeByID(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "invite code %s not found", id)
	}
	if rec.GetState() == "revoked" {
		return rec, nil
	}
	rec.State = "revoked"
	rec.RevokedAt = timestamppb.New(time.Now())
	s.store.UpdateInviteCode(rec)
	if s.inviteMirror != nil {
		if err := s.inviteMirror.UpsertInviteCode(rec); err != nil {
			mirrorErr("invite-revoke", id, err)
		}
	}
	return rec, nil
}

// DeleteInviteCode — 硬删（无审计残迹，Q11 追加）。护栏 Q14：仅终态码（已吊销/已用尽/
// 已过期）可删——活跃码两步走（先吊销后删）；谓词同源 repo.InviteTerminal。
func (s *UserService) DeleteInviteCode(id string) error {
	rec, ok := s.inviteCodeByID(id)
	if !ok {
		return status.Errorf(codes.NotFound, "invite code %s not found", id)
	}
	if !repo.InviteTerminal(rec, time.Now()) {
		return status.Error(codes.FailedPrecondition, "active invite code must be revoked first (two-step guard)")
	}
	s.store.DeleteInviteCode(id)
	if s.inviteMirror != nil {
		if err := s.inviteMirror.DeleteInviteCode(id); err != nil {
			mirrorErr("invite-delete", id, err)
		}
	}
	return nil
}

// GetRegistrationPolicy — 注册策略只读（仅 mode 枚举值；网关 /v1/auth 公开面，Q10）。
func (s *UserService) GetRegistrationPolicy() string { return s.registrationMode() }

// inviteCodeLooksValid — 注册前廉价预检（不消费）：DB 有效码或静态层命中。真正的
// 原子消费在 CreateUser 成功之后（consumeInviteCode），预检只为把明显废码挡在建号前。
func (s *UserService) inviteCodeLooksValid(code string) bool {
	if code == "" {
		return false
	}
	now := time.Now()
	if rec, ok := s.store.GetInviteCode(code); ok && repo.InviteUsable(rec, now) {
		return true
	}
	if s.inviteMirror != nil {
		if rec, err := s.inviteMirror.GetInviteCode(code); err == nil && rec != nil && repo.InviteUsable(rec, now) {
			return true
		}
	}
	return s.inviteCodeValid(code)
}

// consumeInviteCode — 注册消费（原子）。PG 档：CAS 条件更新为跨副本权威，成功后回灌
// 内存副本；PG 故障=降级内存档（ADR-250 中间件故障域不反噬注册可用性；降级窗内单副本
// 超卖风险 ADR-255 注记）。DB 码未命中/谓词拒 → 静态配置层兜底（V1 语义不变：配置内
// 无限有效，bootstrap 层）。
func (s *UserService) consumeInviteCode(code, userID string) bool {
	if code == "" {
		return false
	}
	now := time.Now()
	if s.inviteMirror != nil {
		ok, err := s.inviteMirror.ConsumeInviteCode(code, userID, now)
		if err != nil {
			mirrorErr("invite-consume", code, err)
			return s.store.ConsumeInviteCode(code, userID, now) || s.inviteCodeValid(code)
		}
		if ok {
			if rec, gerr := s.inviteMirror.GetInviteCode(code); gerr == nil && rec != nil {
				s.store.PutInviteCode(rec)
			}
			return true
		}
		// PG miss/谓词拒：内存档兜底（镜像写穿 best-effort 失败的码预检可见，此处须同源可消费），再落静态层
		return s.store.ConsumeInviteCode(code, userID, now) || s.inviteCodeValid(code)
	}
	return s.store.ConsumeInviteCode(code, userID, now) || s.inviteCodeValid(code)
}

// inviteCodeRole — 解析邀请码的注册落地角色（ADR-257）。PG 镜像优先（跨副本权威），
// 回落内存档；仅 VIEWER 特判，其余（UNSPECIFIED/DEVELOPER/查无此码/空码）一律
// DEVELOPER——静态配置码与历史码无 role 字段，语义=缺省 DEVELOPER。
func (s *UserService) inviteCodeRole(code string) v1.Role {
	if code == "" {
		return v1.Role_ROLE_DEVELOPER
	}
	var rec *v1.InviteCode
	if s.inviteMirror != nil {
		if r, err := s.inviteMirror.GetInviteCode(code); err == nil && r != nil {
			rec = r
		}
	}
	if rec == nil {
		if r, ok := s.store.GetInviteCode(code); ok {
			rec = r
		}
	}
	if rec != nil && rec.GetRole() == v1.Role_ROLE_VIEWER {
		return v1.Role_ROLE_VIEWER
	}
	return v1.Role_ROLE_DEVELOPER
}

// V2.1 (ADR-205): 用户生命周期五接口的业务逻辑——
// RegisterUser / ListUsers / CreateUser / ChangePassword / ResetPassword。
// 密码一律 bcrypt；策略数值以 07 §账号安全（ADR-205 提案行）为唯一事实源。
package service

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
)

// 账号安全策略常量（07 §账号安全，ADR-205 提案行；改动须先改 07 再同步此处引用）。
const (
	minPasswordLen  = 8
	maxPasswordLen  = 64
	tempPasswordLen = 12
)

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// AuthConfig — 注册策略（configs codeaudit.yaml auth.*，main.go 经 SetAuthConfig 装配）。
// InviteCodes 为配置静态分发的有效邀请码（V1；表+管理 RPC 为 V2.2 候选，ADR-205）。
type AuthConfig struct {
	RegistrationMode string   // invitation | open | disabled（空=invitation）
	InviteCodes      []string // 邀请码制下的有效码
}

// SetAuthConfig injects the registration policy (cmd/main.go, ADR-137 全局配置).
func (s *UserService) SetAuthConfig(cfg AuthConfig) { s.auth = cfg }

func (s *UserService) registrationMode() string {
	if s.auth.RegistrationMode == "" {
		return "invitation"
	}
	return s.auth.RegistrationMode
}

func (s *UserService) inviteCodeValid(code string) bool {
	for _, c := range s.auth.InviteCodes {
		if c != "" && c == code {
			return true
		}
	}
	return false
}

// validateNewUser — 注册/建号共用字段校验（03 §3 INVALID_ARGUMENT 语义）。
func validateNewUser(username, email, password string) error {
	if !usernameRe.MatchString(username) {
		return status.Errorf(codes.InvalidArgument, "username must match %s", usernameRe.String())
	}
	if !emailRe.MatchString(email) {
		return status.Error(codes.InvalidArgument, "email format invalid")
	}
	if err := validatePasswordStrength(password); err != nil {
		return err
	}
	return nil
}

// validatePasswordStrength — 长度下限 + 必含字母与数字（07 §账号安全）。
func validatePasswordStrength(password string) error {
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return status.Errorf(codes.InvalidArgument, "password length must be %d-%d (07 §账号安全)", minPasswordLen, maxPasswordLen)
	}
	var hasLetter, hasDigit bool
	for _, r := range password {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return status.Error(codes.InvalidArgument, "password must contain both letters and digits (07 §账号安全)")
	}
	return nil
}

// hashPassword — bcrypt 统一入口（登录比对与种子账号同口径）。
func hashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", status.Errorf(codes.Internal, "hash password: %v", err)
	}
	return string(h), nil
}

// newUserID — 同 service/project.go generateID 惯例的 user 前缀变体。
func newUserID() string { return "user-" + generateID() }

// RegisterUser — 自助注册（幂等缓存由 handler 层承载）；成功返回建号用户，
// 令牌对由 handler 复用 LoginResponse 语义签发（注册即登录，ADR-205）。
// 角色固定 ROLE_DEVELOPER（自注册不可自封 admin）。
func (s *UserService) RegisterUser(username, email, password, inviteCode string) (*v1.User, error) {
	switch s.registrationMode() {
	case "disabled":
		return nil, status.Error(codes.FailedPrecondition, "registration is disabled")
	case "invitation":
		if !s.inviteCodeValid(inviteCode) {
			return nil, status.Error(codes.FailedPrecondition, "invalid or missing invite code")
		}
	}
	if err := validateNewUser(username, email, password); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	rec := &repo.UserRecord{
		User: &v1.User{
			UserId:   newUserID(),
			Username: username,
			Email:    email,
			State:    v1.User_USER_STATE_ACTIVE,
			Role:     v1.Role_ROLE_DEVELOPER,
		},
		Password: hash,
	}
	if !s.store.CreateUser(rec) {
		return nil, status.Error(codes.AlreadyExists, "username or email already exists")
	}
	return rec.User, nil
}

// ListUsers — 管理端用户列表（过滤+排序在 store 层；分页在 handler 层，同 ListProjects 口径）。
func (s *UserService) ListUsers(state v1.User_UserState, usernameContains string) []*repo.UserRecord {
	return s.store.ListUsers(state, usernameContains)
}

// CreateUser — 管理员直建：must_change_password 置真（临时密码语义），角色由请求指定
//（UNSPECIFIED 缺省 DEVELOPER）。自注册不可指定角色，此为管理通道专属。
func (s *UserService) CreateUser(username, email, password string, role v1.Role) (*v1.User, error) {
	if err := validateNewUser(username, email, password); err != nil {
		return nil, err
	}
	if role == v1.Role_ROLE_UNSPECIFIED {
		role = v1.Role_ROLE_DEVELOPER
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	rec := &repo.UserRecord{
		User: &v1.User{
			UserId:             newUserID(),
			Username:           username,
			Email:              email,
			State:              v1.User_USER_STATE_ACTIVE,
			Role:               role,
			MustChangePassword: true,
		},
		Password: hash,
	}
	if !s.store.CreateUser(rec) {
		return nil, status.Error(codes.AlreadyExists, "username or email already exists")
	}
	return rec.User, nil
}

// ChangePassword — 自助改密：校验旧密码，成功即清除 must_change_password。
func (s *UserService) ChangePassword(userID, oldPassword, newPassword string) error {
	rec, ok := s.store.GetUser(userID)
	if !ok {
		return status.Errorf(codes.NotFound, "user %s not found", userID)
	}
	if bcrypt.CompareHashAndPassword([]byte(rec.Password), []byte(oldPassword)) != nil {
		return status.Error(codes.Unauthenticated, "old password incorrect")
	}
	if err := validatePasswordStrength(newPassword); err != nil {
		return err
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	rec.Password = hash
	rec.User.MustChangePassword = false
	s.store.UpdateUser(rec)
	return nil
}

// ResetPassword — 管理员重置：服务端生成一次性临时密码（不落日志，仅响应返回一次），
// must_change_password 置真强制首登改密。
func (s *UserService) ResetPassword(userID string) (string, error) {
	rec, ok := s.store.GetUser(userID)
	if !ok {
		return "", status.Errorf(codes.NotFound, "user %s not found", userID)
	}
	temp, err := generateTempPassword(tempPasswordLen)
	if err != nil {
		return "", status.Errorf(codes.Internal, "generate temp password: %v", err)
	}
	hash, err := hashPassword(temp)
	if err != nil {
		return "", err
	}
	rec.Password = hash
	rec.User.MustChangePassword = true
	s.store.UpdateUser(rec)
	return temp, nil
}

// generateTempPassword — crypto/rand 生成的字母数字临时密码（无歧义字符集）。
func generateTempPassword(n int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
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

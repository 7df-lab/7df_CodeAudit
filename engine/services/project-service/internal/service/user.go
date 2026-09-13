// Package service implements business logic for the User domain, including
// JWT-based authentication (HS256, 03 §4).
package service

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
)

const (
	// Token expiry: access=30min, refresh=7d（D2 裁决 2026-09-11：实现 1h/24h 与
	// 契约三方冲突——03 §4、configs yaml access_ttl_min=30·refresh_ttl_day=7、gateway
	// taskwatch wsMaxLifetime=6h 按 30min 推导——改代码对齐；现网影响仅 token 提前续期）
	accessTokenExpiry  = 30 * time.Minute
	refreshTokenExpiry = 7 * 24 * time.Hour
)

// UserService holds business methods for user operations and JWT auth.
type UserService struct {
	store *repo.MemoryStore

	// auth — 注册策略（V2.1 ADR-205，configs codeaudit.yaml auth.*，main.go 装配）。
	auth AuthConfig

	// revokedTokens holds blacklisted access tokens (logout)。
	// R69（2026-09-12 待办收尾）：值改撤销时刻——惰性清扫超 24h 条目（access TTL
	// 30min，超窗撤销记录永不再命中；原 map[string]struct{} 无界增长）。
	mu            sync.RWMutex
	revokedTokens map[string]time.Time

	// sessionsInvalidBefore — R87: 用户会话纪元（ userID → 时刻）。iat 早于纪元的
	// refresh token 一律拒绝。触发点=Logout/改密成功/管理员重置/停用（停用即踢）。
	// 惰性清扫阈值 8d 须 ≥refresh TTL 7d（R69 的 24h 是按 access 30min 定的，直接
	// 沿用会让纪元比 refresh 先蒸发）；数值源 ADR-230。
	sessionsInvalidBefore map[string]time.Time
}

// NewUserService creates a UserService backed by the given store.
func NewUserService(store *repo.MemoryStore) *UserService {
	return &UserService{
		store:                 store,
		revokedTokens:         make(map[string]time.Time),
		sessionsInvalidBefore: make(map[string]time.Time),
	}
}

// jwtSecret reads the secret from CODEAUDIT_JWT_SECRET env var (03 §4).
// R65（2026-09-12 待办收尾）：未配置即 panic（fail-fast，对齐 gateway 同键位口径）——
// 原 fail-open 落开发缺省密钥：签发/校验两侧密钥不一致时表现为全量登录失败难归因，
// 且缺省密钥已知=伪造令牌面。compose/env 模板均已要求该键。
func jwtSecret() []byte {
	secret := os.Getenv("CODEAUDIT_JWT_SECRET")
	if secret == "" {
		panic("CODEAUDIT_JWT_SECRET unset — refusing to sign with a known default (R65; gateway fails fast on the same key)")
	}
	return []byte(secret)
}

// Login authenticates a user and returns JWT tokens (HS256, 03 §4).
// V2.1 (ADR-205): 密码比对改 bcrypt（偿还 POC 明文债）；token 携带 role claim
//（网关管理路由门禁依赖；旧令牌无 role，重新登录即获得）。
func (s *UserService) Login(username, password string) (*v1.LoginResponse, error) {
	rec, ok := s.store.GetUserByUsername(username)
	if !ok {
		// R65：失败文案合一（防用户名枚举——网关 401 原文透传）
		return nil, fmt.Errorf("invalid username or password")
	}
	if bcrypt.CompareHashAndPassword([]byte(rec.Password), []byte(password)) != nil {
		return nil, fmt.Errorf("invalid username or password")
	}

	now := time.Now()

	// Access token (HS256, 03 §4)
	accessClaims := jwt.MapClaims{
		"sub":      rec.User.GetUserId(),
		"username": rec.User.GetUsername(),
		"role":     rec.User.GetRole().String(),
		"exp":      now.Add(accessTokenExpiry).Unix(),
		"iat":      now.Unix(),
		"type":     "access",
	}
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessSigned, err := accessToken.SignedString(jwtSecret())
	if err != nil {
		return nil, fmt.Errorf("failed to sign access token: %w", err)
	}

	// Refresh token (HS256, 03 §4)
	refreshClaims := jwt.MapClaims{
		"sub":  rec.User.GetUserId(),
		"exp":  now.Add(refreshTokenExpiry).Unix(),
		"iat":  now.Unix(),
		"type": "refresh",
	}
	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshSigned, err := refreshToken.SignedString(jwtSecret())
	if err != nil {
		return nil, fmt.Errorf("failed to sign refresh token: %w", err)
	}

	return &v1.LoginResponse{
		AccessToken:  accessSigned,
		RefreshToken: refreshSigned,
		ExpiresInS:   int64(accessTokenExpiry.Seconds()),
	}, nil
}

// Logout blacklists the given access token.
// R87: 同时推进该用户会话纪元——此前登出只影响 GetCurrentUser，被盗 refresh（7d）
// 仍可持续换新 access 对；纪元使 iat 早于登出时刻的全部 refresh 即刻失效。
func (s *UserService) Logout(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.revokedTokens[accessToken] = now
	for tok, at := range s.revokedTokens { // R69: 惰性清扫（access TTL 30min，24h 足裕）
		if now.Sub(at) > 24*time.Hour {
			delete(s.revokedTokens, tok)
		}
	}
	if uid, err := s.extractUserID(accessToken); err == nil { // 解析失败=无效令牌，纪元无从谈起
		s.sessionsInvalidBefore[uid] = now
		for u, at := range s.sessionsInvalidBefore { // R87: 惰性清扫（≥refresh TTL 7d，8d 足裕）
			if now.Sub(at) > 8*24*time.Hour {
				delete(s.sessionsInvalidBefore, u)
			}
		}
	}
}

// invalidateSessions — R87: 推进用户会话纪元（改密/重置/停用通道；登出走 Logout 内联）。
func (s *UserService) invalidateSessions(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionsInvalidBefore[userID] = time.Now()
}

// sessionInvalidated — R87: 该用户纪元是否已推进到 iat 之后（refresh 准入判定）。
func (s *UserService) sessionInvalidated(userID string, iat time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	epoch, ok := s.sessionsInvalidBefore[userID]
	return ok && iat.Before(epoch)
}

// IsRevoked checks if a token was revoked via Logout.
func (s *UserService) IsRevoked(token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.revokedTokens[token]
	return ok
}

// RefreshToken validates a refresh token and issues new token pair.
func (s *UserService) RefreshToken(refreshTokenStr string) (*v1.RefreshTokenResponse, error) {
	token, err := jwt.Parse(refreshTokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return jwtSecret(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid refresh token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid refresh token claims")
	}

	tokenType, _ := claims["type"].(string)
	if tokenType != "refresh" {
		return nil, fmt.Errorf("token is not a refresh token")
	}

	sub, _ := claims["sub"].(string)
	rec, exists := s.store.GetUser(sub)
	if !exists {
		return nil, fmt.Errorf("user not found: %s", sub)
	}

	// R87: 会话纪元守卫——iat 早于纪元（登出/改密/重置/停用之后签发）的 refresh
	// 一律拒绝（03 §4 会话语义收紧，见 ADR-230）。
	if iatF, ok := claims["iat"].(float64); ok {
		if s.sessionInvalidated(sub, time.Unix(int64(iatF), 0)) {
			return nil, fmt.Errorf("session has been invalidated, please login again")
		}
	}

	// Issue new tokens
	return s.issueTokenPair(rec.User.GetUserId(), rec.User.GetUsername(), rec.User.GetRole().String())
}

// GetCurrentUser extracts user info from an access token.
func (s *UserService) GetCurrentUser(accessToken string) (*v1.User, error) {
	if s.IsRevoked(accessToken) {
		return nil, fmt.Errorf("token has been revoked")
	}
	userID, err := s.extractUserID(accessToken)
	if err != nil {
		return nil, err
	}
	rec, ok := s.store.GetUser(userID)
	if !ok {
		return nil, fmt.Errorf("user not found: %s", userID)
	}
	return rec.User, nil
}

// GetUser returns a user by ID.
func (s *UserService) GetUser(userID string) (*v1.User, bool) {
	rec, ok := s.store.GetUser(userID)
	if !ok {
		return nil, false
	}
	return rec.User, true
}

// UpdateUser updates user fields.
func (s *UserService) UpdateUser(u *v1.User) (*v1.User, bool) {
	rec, ok := s.store.GetUser(u.GetUserId())
	if !ok {
		return nil, false
	}
	// Preserve fields that shouldn't be overwritten
	if u.GetUsername() == "" {
		u.Username = rec.User.GetUsername()
	}
	if u.GetEmail() == "" {
		u.Email = rec.User.GetEmail()
	}
	if u.GetCreatedAt() == nil {
		u.CreatedAt = rec.User.GetCreatedAt()
	}
	// V2.1 (ADR-205): role 缺省（UNSPECIFIED）不清空；must_change_password 为 proto3
	// bool 无 presence，UpdateUser 通道一律保全存量值（强改密标记只经 ChangePassword 清除）
	if u.GetRole() == v1.Role_ROLE_UNSPECIFIED {
		u.Role = rec.User.GetRole()
	}
	// R85（复审修正）: state 缺省（UNSPECIFIED）同样保全存量——此前缺省直通会把
	// 未带 state 字段的更新写成 0 值；网关侧非 admin self 更新剥离 state 后亦依赖
	// 此保全（防被停用用户自助改回 ACTIVE）。
	if u.GetState() == v1.User_USER_STATE_UNSPECIFIED {
		u.State = rec.User.GetState()
	}
	u.MustChangePassword = rec.User.GetMustChangePassword()
	rec.User = u
	s.store.UpdateUser(rec)
	// R87: 停用即踢——有效 state 离开 ACTIVE（管理员停用/锁定）推进会话纪元，已发
	// refresh 即刻失效（state 不变更不触发；非 ACTIVE→ACTIVE 的恢复不踢旧会话）。
	// 此处 u.State 已经过上文 UNSPECIFIED 保全，即为生效 state。
	if u.GetState() != v1.User_USER_STATE_ACTIVE {
		s.invalidateSessions(u.GetUserId())
	}
	return u, true
}

// ValidatePermission checks if a user has the specified action on a resource.
func (s *UserService) ValidatePermission(userID, resourceType, resourceID, action string) (bool, string) {
	perms := s.store.GetUserPermissionList(userID)
	needed := resourceType + ":" + action
	for _, p := range perms {
		if p == needed {
			return true, ""
		}
	}
	return false, fmt.Sprintf("user %s lacks permission %s on %s/%s", userID, action, resourceType, resourceID)
}

// GetUserPermissions returns the permission list for a user.
func (s *UserService) GetUserPermissions(userID string) []string {
	return s.store.GetUserPermissionList(userID)
}

// extractUserID parses the access token and returns the "sub" claim.
func (s *UserService) extractUserID(accessToken string) (string, error) {
	token, err := jwt.Parse(accessToken, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return jwtSecret(), nil
	})
	if err != nil {
		return "", fmt.Errorf("invalid access token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid access token claims")
	}

	tokenType, _ := claims["type"].(string)
	if tokenType != "access" {
		return "", fmt.Errorf("token is not an access token")
	}

	sub, _ := claims["sub"].(string)
	return sub, nil
}

// issueTokenPair creates a new access + refresh token pair (helper for RefreshToken).
// role 进入 access claims（V2.1 ADR-205：网关管理路由门禁的授权依据）。
func (s *UserService) issueTokenPair(userID, username, role string) (*v1.RefreshTokenResponse, error) {
	now := time.Now()

	accessClaims := jwt.MapClaims{
		"sub":      userID,
		"username": username,
		"role":     role,
		"exp":      now.Add(accessTokenExpiry).Unix(),
		"iat":      now.Unix(),
		"type":     "access",
	}
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessSigned, err := accessToken.SignedString(jwtSecret())
	if err != nil {
		return nil, fmt.Errorf("failed to sign access token: %w", err)
	}

	refreshClaims := jwt.MapClaims{
		"sub":  userID,
		"exp":  now.Add(refreshTokenExpiry).Unix(),
		"iat":  now.Unix(),
		"type": "refresh",
	}
	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshSigned, err := refreshToken.SignedString(jwtSecret())
	if err != nil {
		return nil, fmt.Errorf("failed to sign refresh token: %w", err)
	}

	return &v1.RefreshTokenResponse{
		AccessToken:  accessSigned,
		RefreshToken: refreshSigned,
		ExpiresInS:   int64(accessTokenExpiry.Seconds()),
	}, nil
}

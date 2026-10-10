package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Context key type to avoid collisions
type contextKey string

const (
	UserIDKey   contextKey = "user_id"
	UserRoleKey contextKey = "user_role"
)

// JWTClaims represents the JWT claims - 依据: 03 §4 (JWT access 30min / refresh 7d)
type JWTClaims struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	// Type 令牌类型（access|refresh，签发面 user.go 一律携带）——依据: 03 §4。
	// 第四轮审计 P1：refresh(7d) 与 access 同为 HS256、仅 type 可辨；不校验则
	// refresh 可直接作 Bearer 打全部业务路由，且绕过 R87 纪元/R88 撤销集
	// （两者只掐 access 的刷新链路）——被盗 refresh 至多可用 7 天。
	Type string `json:"type,omitempty"`
	jwt.RegisteredClaims
}

// JWTMiddleware validates JWT tokens and extracts user information
// 依据: 03 §1.1 (gateway-service 认证能力)
func JWTMiddleware(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract token from Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			// ADR-172: 浏览器 WebSocket API 无法自定义 Authorization 头——
			// 仅对 WS 升级请求接受 token=<JWT> 查询参数（URL 中 token 不落业务日志）
			if tk := r.URL.Query().Get("token"); tk != "" {
				authHeader = "Bearer " + tk
			}
		}
		if authHeader == "" {
			http.Error(w, `{"error": "missing authorization header"}`, http.StatusUnauthorized)
			return
		}

		// Check Bearer prefix
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			http.Error(w, `{"error": "invalid authorization format, expected: Bearer <token>"}`, http.StatusUnauthorized)
			return
		}

		tokenString := parts[1]

		// R88: 网关本地撤销集（登出即时性）——撤销记录由 /v1/auth/logout 通道写入，
		// 服务端 revokedTokens 只有 GetCurrentUser 消费、管不到业务路由。
		if AccessRevoked(tokenString) {
			http.Error(w, `{"error": "token has been revoked"}`, http.StatusUnauthorized)
			return
		}

		// Parse and validate token - 依据: 03 §4 (JWT HS256 auth)
		token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
			// Verify signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(secret), nil
		})

		if err != nil {
			http.Error(w, `{"error": "invalid or expired token"}`, http.StatusUnauthorized)
			return
		}

		// Extract claims
		claims, ok := token.Claims.(*JWTClaims)
		if !ok || !token.Valid {
			http.Error(w, `{"error": "invalid token claims"}`, http.StatusUnauthorized)
			return
		}

		// 第四轮审计 P1：只接受 access 型（签发面两类令牌恒带 type；缺省即异常来源，
		// fail-closed 拒绝）——refresh 仅许走 /v1/auth/refresh（skipAuth 链）。
		if claims.Type != "access" {
			http.Error(w, `{"error": "invalid token type"}`, http.StatusUnauthorized)
			return
		}

		// Check token expiration - 依据: 03 §4 (access token 30min validity)
		if claims.ExpiresAt != nil && claims.ExpiresAt.Time.Before(time.Now()) {
			http.Error(w, `{"error": "token expired"}`, http.StatusUnauthorized)
			return
		}

		// Set user information in context for downstream handlers
		// ADR-199 修复: 签发口径是标准 sub（project-service user.go L65 "sub": user_id），
		// 此处却只解析自定义 user_id——x-user-id 此前恒为空串。双口径兼容：
		// 自定义 user_id 缺省时回退 RegisteredClaims.Subject（jwt 库已代解析 sub）。
		userID := claims.UserID
		if userID == "" {
			userID = claims.Subject
		}
		ctx := context.WithValue(r.Context(), UserIDKey, userID)
		ctx = context.WithValue(ctx, UserRoleKey, claims.Role)

		// Forward headers to backend services
		r.Header.Set("x-user-id", userID)
		r.Header.Set("x-user-role", claims.Role)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

package middleware

// R88: 网关本地 access token 撤销集——登出即时性补齐。服务端 revokedTokens 仅
// GetCurrentUser 消费，网关业务路由此前不回查：登出后 access（30min）在全部业务
// 路由照常可用。refresh 侧由服务端会话纪元（R87）负责；本集只管 access 的即时性。
// TTL=access TTL 30min（07 §8 口径），超时条目随查询惰性清扫（rateLimiter
// cleanup 同款形态）。

import (
	"sync"
	"time"
)

var (
	accessRevokedMu sync.RWMutex
	accessRevoked   = make(map[string]time.Time)
	// accessRevocationTTL — 撤销记录保留时长=access TTL（07 §8 口径 30min）；
	// 超过即自然过期无需查询。var 仅为测试可注入（TTL 行为面）。
	accessRevocationTTL = 30 * time.Minute
)

// RevokeAccess — 登出通道调用：撤销该 access token 的网关侧通行（即时生效）。
func RevokeAccess(token string) {
	accessRevokedMu.Lock()
	defer accessRevokedMu.Unlock()
	now := time.Now()
	accessRevoked[token] = now
	for tok, at := range accessRevoked { // 惰性清扫
		if now.Sub(at) > accessRevocationTTL {
			delete(accessRevoked, tok)
		}
	}
}

// AccessRevoked — JWT 中间件每请求查询（O(1)，含惰性清扫）。
func AccessRevoked(token string) bool {
	accessRevokedMu.RLock()
	at, ok := accessRevoked[token]
	accessRevokedMu.RUnlock()
	if !ok {
		return false
	}
	if time.Since(at) > accessRevocationTTL {
		accessRevokedMu.Lock()
		delete(accessRevoked, token)
		accessRevokedMu.Unlock()
		return false
	}
	return true
}

package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 07 §7 限流50req/min
const RateLimitPerMinute = 50

// tokenBucket implements a token bucket rate limiter
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

// newTokenBucket creates a new token bucket with specified capacity and refill rate
func newTokenBucket(maxTokens float64, refillRate float64) *tokenBucket {
	return &tokenBucket{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// allow checks if a request is allowed and consumes a token if so
func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastRefill = now

	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

// rateLimiter manages per-IP rate limiting using token buckets
type rateLimiter struct {
	mu              sync.RWMutex
	buckets         map[string]*tokenBucket
	maxRate         float64 // requests per minute
	cleanupInterval time.Duration
}

// newRateLimiter creates a new rate limiter
func newRateLimiter(requestsPerMinute int) *rateLimiter {
	rl := &rateLimiter{
		buckets:         make(map[string]*tokenBucket),
		maxRate:         float64(requestsPerMinute),
		cleanupInterval: 5 * time.Minute,
	}

	// Start cleanup goroutine to remove stale entries
	go rl.cleanup()

	return rl
}

// getBucket returns the token bucket for a given IP, creating one if needed
func (rl *rateLimiter) getBucket(ip string) *tokenBucket {
	rl.mu.RLock()
	bucket, exists := rl.buckets[ip]
	rl.mu.RUnlock()

	if exists {
		return bucket
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Double-check after acquiring write lock
	bucket, exists = rl.buckets[ip]
	if !exists {
		// Create new bucket with capacity = maxRate, refill rate = maxRate/60 per second
		bucket = newTokenBucket(rl.maxRate, rl.maxRate/60)
		rl.buckets[ip] = bucket
	}

	return bucket
}

// cleanup removes stale entries periodically
func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		// Remove buckets that haven't been used in the last 10 minutes
		for ip, bucket := range rl.buckets {
			bucket.mu.Lock()
			if time.Since(bucket.lastRefill) > 10*time.Minute {
				delete(rl.buckets, ip)
			}
			bucket.mu.Unlock()
		}
		rl.mu.Unlock()
	}
}

// getClientIP extracts the client IP from the request.
// 依据: 07 §7 限流按请求方计费。XFF 可被客户端伪造——仅在网关部署于可信反向代理
// 之后（CODEAUDIT_TRUST_PROXY=true）才信任 XFF；默认取 RemoteAddr（ADR-132）。
func getClientIP(r *http.Request, trustXFF bool) string {
	if trustXFF {
		// X-Forwarded-For can contain multiple IPs, use the first one
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if len(parts) > 0 {
				ip := strings.TrimSpace(parts[0])
				if ip != "" {
					return ip
				}
			}
		}
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// RateLimitMiddleware creates a rate limiting middleware using token bucket algorithm
// 依据: 07 §7 限流值（数值来自全局配置 gateway.rate_limit_per_min，ADR-137）；
// trustXFF 仅在可信代理部署时为 true（ADR-132）
func RateLimitMiddleware(trustXFF bool, perMinute int, next http.Handler) http.Handler {
	limiter := newRateLimiter(perMinute)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 限流键=单用户（07 §7 口径）：优先按 Bearer 令牌（每登录会话一桶）。
		// 全部浏览器流量经反向代理后 RemoteAddr 同为代理地址，按 IP 计数会让
		// 所有用户/标签页共抢一个桶（ADR-170 实测：详情页轮询被无关标签页打死）。
		key := "user:" + r.Header.Get("Authorization")
		if key == "user:" {
			key = getClientIP(r, trustXFF)
		}
		bucket := limiter.getBucket(key)

		if !bucket.allow() {
			// Rate limit exceeded - return 429 with Retry-After header
			// 依据: HTTP RFC 6585 (429 Too Many Requests)
			w.Header().Set("Retry-After", "60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "rate limit exceeded", "retry_after": 60}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

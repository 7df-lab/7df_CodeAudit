package middleware

import (
	"bufio"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// redactToken — 日志脱敏（ADR-212）：ADR-172 专为浏览器 WS 接受 ?token=<JWT>
// 查询参数，但本中间件此前打 r.RequestURI 全文，把有效访问令牌整条写进网关
// 日志（与 jwt.go"URL 中 token 不落业务日志"的声明相悖）。凡带 token 查询
// 参数的 URI，值一律替换为 REDACTED；其余查询参数原样保留。
func redactToken(requestURI string) string {
	u, err := url.ParseRequestURI(requestURI)
	if err != nil {
		// 解析失败退回朴素裁剪：token= 后至下一个 &/结尾
		if i := strings.Index(requestURI, "token="); i >= 0 {
			end := strings.IndexAny(requestURI[i:], "&")
			if end < 0 {
				return requestURI[:i] + "token=REDACTED"
			}
			return requestURI[:i] + "token=REDACTED" + requestURI[i+end:]
		}
		return requestURI
	}
	q := u.Query()
	if q.Get("token") != "" {
		q.Set("token", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.RequestURI()
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code before writing it
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack 透传（ADR-172）：WebSocket 升级要求 http.Hijacker，包装层不得丢失该能力
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("middleware/responseWriter: underlying writer is not http.Hijacker")
	}
	rw.statusCode = http.StatusSwitchingProtocols
	return h.Hijack()
}

// LoggingMiddleware logs HTTP requests with timing and status
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &responseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		// Process request
		next.ServeHTTP(wrapped, r)

		// Log request details
		duration := time.Since(start)
		log.Printf("[%s] %s %s %d %v",
			r.Method,
			redactToken(r.RequestURI),
			r.RemoteAddr,
			wrapped.statusCode,
			duration,
		)
	})
}

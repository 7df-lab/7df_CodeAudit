package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRateLimiterUnderLimit(t *testing.T) {
	limiter := newRateLimiter(50) // 50 requests per minute

	// Test that requests under the limit pass through
	for i := 0; i < 50; i++ {
		bucket := limiter.getBucket("192.168.1.1")
		if !bucket.allow() {
			t.Errorf("Request %d should be allowed but was rejected", i+1)
		}
	}
}

func TestRateLimiterOverLimit(t *testing.T) {
	limiter := newRateLimiter(50) // 50 requests per minute

	// Consume all tokens
	for i := 0; i < 50; i++ {
		bucket := limiter.getBucket("192.168.1.1")
		if !bucket.allow() {
			t.Fatalf("Request %d should be allowed", i+1)
		}
	}

	// Next request should be rejected
	bucket := limiter.getBucket("192.168.1.1")
	if bucket.allow() {
		t.Error("Request should be rejected after limit exceeded")
	}
}

func TestRateLimiterDifferentIPs(t *testing.T) {
	limiter := newRateLimiter(50) // 50 requests per minute per IP

	// Test that different IPs have separate limits
	ip1 := "192.168.1.1"
	ip2 := "192.168.1.2"

	// Consume all tokens for IP1
	for i := 0; i < 50; i++ {
		bucket := limiter.getBucket(ip1)
		if !bucket.allow() {
			t.Fatalf("IP1 request %d should be allowed", i+1)
		}
	}

	// IP1 should be rate limited
	bucket1 := limiter.getBucket(ip1)
	if bucket1.allow() {
		t.Error("IP1 should be rate limited")
	}

	// IP2 should still have tokens
	bucket2 := limiter.getBucket(ip2)
	if !bucket2.allow() {
		t.Error("IP2 should still be allowed")
	}
}

func TestRateLimiterResetAfterWindow(t *testing.T) {
	// Create a rate limiter with very short refill for testing
	limiter := newRateLimiter(10) // 10 requests per minute

	// Consume all tokens
	for i := 0; i < 10; i++ {
		bucket := limiter.getBucket("10.0.0.1")
		if !bucket.allow() {
			t.Fatalf("Request %d should be allowed", i+1)
		}
	}

	// Verify rate limited
	bucket := limiter.getBucket("10.0.0.1")
	if bucket.allow() {
		t.Error("Should be rate limited initially")
	}

	// Wait for tokens to refill (simulate time passing by manipulating lastRefill)
	bucket.mu.Lock()
	bucket.lastRefill = time.Now().Add(-61 * time.Second) // Simulate 61 seconds ago
	bucket.mu.Unlock()

	// Should be allowed now after refill
	if !bucket.allow() {
		t.Error("Should be allowed after refill period")
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	// Create a test handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Wrap with rate limiter
	limitedHandler := RateLimitMiddleware(false, 50, handler)

	// Test with a specific IP
	t.Run("UnderLimit", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "192.168.1.1:12345"
			rr := httptest.NewRecorder()
			limitedHandler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("Request %d: expected status 200, got %d", i+1, rr.Code)
			}
		}
	})

	t.Run("OverLimit", func(t *testing.T) {
		// Already made 50 requests above, next should be 429
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		rr := httptest.NewRecorder()
		limitedHandler.ServeHTTP(rr, req)

		if rr.Code != http.StatusTooManyRequests {
			t.Errorf("Expected status 429, got %d", rr.Code)
		}

		// Check Retry-After header
		retryAfter := rr.Header().Get("Retry-After")
		if retryAfter != "60" {
			t.Errorf("Expected Retry-After: 60, got %s", retryAfter)
		}
	})
}

func TestRateLimitMiddlewareConcurrent(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	limitedHandler := RateLimitMiddleware(false, 50, handler)

	var wg sync.WaitGroup
	allowed := make(chan bool, 100)

	// Simulate concurrent requests from same IP
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "10.0.0.1:8080"
			rr := httptest.NewRecorder()
			limitedHandler.ServeHTTP(rr, req)
			allowed <- rr.Code == http.StatusOK
		}()
	}

	wg.Wait()
	close(allowed)

	// Count allowed requests
	allowedCount := 0
	for a := range allowed {
		if a {
			allowedCount++
		}
	}

	// Should be exactly 50 allowed (the rate limit)
	if allowedCount != 50 {
		t.Errorf("Expected exactly 50 allowed requests, got %d", allowedCount)
	}
}

func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xForwarded string
		trustXFF   bool
		expected   string
	}{
		{
			name:       "RemoteAddr only",
			remoteAddr: "192.168.1.1:12345",
			expected:   "192.168.1.1",
		},
		{
			name:       "X-Forwarded-For single (trusted proxy)",
			remoteAddr: "10.0.0.1:8080",
			xForwarded: "203.0.113.1",
			trustXFF:   true,
			expected:   "203.0.113.1",
		},
		{
			name:       "X-Forwarded-For multiple (trusted proxy)",
			remoteAddr: "10.0.0.1:8080",
			xForwarded: "203.0.113.1, 70.41.3.18, 150.172.238.178",
			trustXFF:   true,
			expected:   "203.0.113.1",
		},
		{
			name:       "X-Forwarded-For with spaces (trusted proxy)",
			remoteAddr: "10.0.0.1:8080",
			xForwarded: "  203.0.113.1  ",
			trustXFF:   true,
			expected:   "203.0.113.1",
		},
		{
			name:       "XFF ignored when not trusted (client cannot spoof, ADR-132)",
			remoteAddr: "10.0.0.1:8080",
			xForwarded: "203.0.113.1",
			trustXFF:   false,
			expected:   "10.0.0.1",
		},
		{
			name:       "XFF honored when trusted proxy",
			remoteAddr: "10.0.0.1:8080",
			xForwarded: "203.0.113.1",
			trustXFF:   true,
			expected:   "203.0.113.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xForwarded != "" {
				req.Header.Set("X-Forwarded-For", tt.xForwarded)
			}

			ip := getClientIP(req, tt.trustXFF)
			if ip != tt.expected {
				t.Errorf("getClientIP() = %v, want %v", ip, tt.expected)
			}
		})
	}
}

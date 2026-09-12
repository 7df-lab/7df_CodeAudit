// Package idempotency implements an in-memory idempotency key store.
//
// Design basis: 03 §2 — Write RPCs with RequestMetadata must enforce idempotency:
//   - Same key + same body → return cached first response
//   - Same key + different body → return ALREADY_EXISTS(9)
//   - Missing metadata → return INVALID_ARGUMENT(3)
//
// ADR-228（2026-09-12 幂等三套统一）：去重窗口 24h（03 §2 契约值——窗口外同键视同
// 新请求）+ FIFO 上限 10k（内存防护，与 storage-service 内存档同口径）。project-
// service 为全内存存储（repo/memory.go），本表生命周期=服务生命周期——重启即丢是
// 服务存储模型边界（与 task-service 实体 PG 兜底 / storage-service Redis TTL 的
// 分层差见 ADR-228），非本包缺陷。
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// dedupWindow — 03 §2 去重窗口 24h；maxEntries — FIFO 上限（ADR-228 内存防护）。
const (
	dedupWindow = 24 * time.Hour
	maxEntries  = 10_000
)

// Result holds a previously computed response so the handler can replay it.
type Result struct {
	ResponseBytes []byte
}

// Store is a bounded, windowed, concurrency-safe map keyed by request_id.
type Store struct {
	mu      sync.RWMutex
	entries map[string]entry
	order   []string // FIFO 驱逐序（首次插入序）
	now     func() time.Time
}

type entry struct {
	bodyHash string
	result   *Result
	savedAt  time.Time
}

// New creates a new in-memory idempotency store.
func New() *Store {
	return &Store{
		entries: make(map[string]entry),
		now:     time.Now,
	}
}

// BodyHash computes a SHA-256 hex digest of the serialised request body.
func BodyHash(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// Check performs the idempotency check described in 03 §2.
//
// Returns:
//   - (cachedResult, nil)     if same key + same body (within window) → replay
//   - (nil, ErrAlreadyExists) if same key + different body (within window) → conflict
//   - (nil, nil)              if key is new or past the 24h window → caller should proceed
func (s *Store) Check(requestID, bodyHash string) (*Result, error) {
	s.mu.RLock()
	existing, ok := s.entries[requestID]
	s.mu.RUnlock()

	if !ok {
		return nil, nil // new key, proceed
	}
	if s.now().Sub(existing.savedAt) > dedupWindow {
		// 窗口外（03 §2）：同键视同新请求；顺带清除过期项
		s.mu.Lock()
		if cur, still := s.entries[requestID]; still && cur == existing {
			s.deleteLocked(requestID)
		}
		s.mu.Unlock()
		return nil, nil
	}
	if existing.bodyHash == bodyHash {
		return existing.result, nil // same key + same body → replay
	}
	return nil, &ErrAlreadyExists{RequestID: requestID} // same key + different body → conflict
}

// Save persists the result for a given request_id so future calls can replay.
// Beyond maxEntries the oldest inserted key is dropped (FIFO, ADR-228).
func (s *Store) Save(requestID, bodyHash string, result *Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[requestID]; !exists {
		s.order = append(s.order, requestID)
	}
	s.entries[requestID] = entry{
		bodyHash: bodyHash,
		result:   result,
		savedAt:  s.now(),
	}
	for len(s.order) > maxEntries {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// deleteLocked — 移除键并同步 FIFO 序（调用方持写锁）。
func (s *Store) deleteLocked(requestID string) {
	delete(s.entries, requestID)
	for i, id := range s.order {
		if id == requestID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

// ErrAlreadyExists is returned when the same request_id is reused with a
// different request body (03 §2 → ALREADY_EXISTS gRPC code 9).
type ErrAlreadyExists struct {
	RequestID string
}

func (e *ErrAlreadyExists) Error() string {
	return fmt.Sprintf("idempotency key %s already used with a different body", e.RequestID)
}

// ErrAlreadyExistsSentinel is a pre-allocated sentinel for simple checks.
var ErrAlreadyExistsSentinel = &ErrAlreadyExists{}

// Package idempotency implements an in-memory idempotency key store.
//
// Design basis: 03 §2 — Write RPCs with RequestMetadata must enforce idempotency:
//   - Same key + same body → return cached first response
//   - Same key + different body → return ALREADY_EXISTS(9)
//   - Missing metadata → return INVALID_ARGUMENT(3)
//
// This is a Redis DB=5 placeholder interface; in production, swap the backend
// to Redis with identical API surface. The in-memory map is sufficient for
// single-instance POC.
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// Result holds a previously computed response so the handler can replay it.
type Result struct {
	ResponseBytes []byte
}

// Store is a concurrency-safe map keyed by request_id.
// TODO(production): Replace with Redis DB=5 backend; keep this interface.
type Store struct {
	mu      sync.RWMutex
	entries map[string]entry
}

type entry struct {
	bodyHash string
	result   *Result
}

// New creates a new in-memory idempotency store.
func New() *Store {
	return &Store{
		entries: make(map[string]entry),
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
//   - (cachedResult, nil)     if same key + same body → replay
//   - (nil, ErrAlreadyExists) if same key + different body → conflict
//   - (nil, nil)              if key is new → caller should proceed
func (s *Store) Check(requestID, bodyHash string) (*Result, error) {
	s.mu.RLock()
	existing, ok := s.entries[requestID]
	s.mu.RUnlock()

	if !ok {
		return nil, nil // new key, proceed
	}

	if existing.bodyHash == bodyHash {
		return existing.result, nil // same key + same body → replay
	}

	return nil, &ErrAlreadyExists{RequestID: requestID} // same key + different body → conflict
}

// Save persists the result for a given request_id so future calls can replay.
func (s *Store) Save(requestID, bodyHash string, result *Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[requestID] = entry{
		bodyHash: bodyHash,
		result:   result,
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

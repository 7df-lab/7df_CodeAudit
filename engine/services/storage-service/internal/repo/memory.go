// Package repo provides in-memory storage for files and notifications.
// This is a placeholder for production storage backed by MinIO (09 §1).
package repo

import (
	"sync"
	"time"

	v1 "github.com/codeaudit/proto-gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MemoryStore holds in-memory data for the storage-service.
// MinIO buckets referenced: reports, cpg, sast-raw (09 §1).
type MemoryStore struct {
	mu            sync.RWMutex
	files         map[string]*v1.StoredFile   // file_id → StoredFile
	fileData      map[string][]byte           // file_id → assembled chunk data
	notifications map[string]*v1.Notification // notification_id → Notification

	// Idempotency cache for write RPCs (03 §2, R4).
	// key = RequestMetadata.request_id
	idempotencyMu   sync.RWMutex
	idempotencyKeys map[string]*IdempotencyEntry
}

// IdempotencyEntry tracks an idempotent request.
type IdempotencyEntry struct {
	BodyHash string      // summary of the request body for duplicate detection
	Response interface{} // cached response
}

// NewMemoryStore creates a new empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		files:           make(map[string]*v1.StoredFile),
		fileData:        make(map[string][]byte),
		notifications:   make(map[string]*v1.Notification),
		idempotencyKeys: make(map[string]*IdempotencyEntry),
	}
}

// ---- File operations ----

// SaveFile stores a file and its raw data.
func (m *MemoryStore) SaveFile(file *v1.StoredFile, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	file.CreatedAt = timestamppb.Now()
	m.files[file.FileId] = file
	m.fileData[file.FileId] = data
}

// GetFile returns a StoredFile by id.
func (m *MemoryStore) GetFile(fileID string) (*v1.StoredFile, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.files[fileID]
	return f, ok
}

// GetFileData returns the raw bytes for a file.
func (m *MemoryStore) GetFileData(fileID string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.fileData[fileID]
	return d, ok
}

// DeleteFile removes a file by id.
func (m *MemoryStore) DeleteFile(fileID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, fileID)
	delete(m.fileData, fileID)
}

// ListFiles returns files whose FilePath starts with the given prefix.
func (m *MemoryStore) ListFiles(prefix string) []*v1.StoredFile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*v1.StoredFile
	for _, f := range m.files {
		if prefix == "" || startsWith(f.FilePath, prefix) {
			result = append(result, f)
		}
	}
	return result
}

// ---- Notification operations ----

// SaveNotification stores a notification.
func (m *MemoryStore) SaveNotification(n *v1.Notification) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n.CreatedAt == nil {
		n.CreatedAt = timestamppb.Now()
	}
	m.notifications[n.NotificationId] = n
}

// GetNotification returns a notification by id.
func (m *MemoryStore) GetNotification(id string) (*v1.Notification, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.notifications[id]
	return n, ok
}

// MarkRead marks a notification as read.
func (m *MemoryStore) MarkRead(id string) (*v1.Notification, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.notifications[id]
	if ok {
		n.Read = true
	}
	return n, ok
}

// ListNotifications returns notifications for a user, optionally filtered to unread only.
func (m *MemoryStore) ListNotifications(userID string, unreadOnly bool) []*v1.Notification {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*v1.Notification
	for _, n := range m.notifications {
		if n.UserId != userID {
			continue
		}
		if unreadOnly && n.Read {
			continue
		}
		result = append(result, n)
	}
	return result
}

// ---- Idempotency (03 §2, R4) ----

// CheckIdempotency checks whether the given request_id has been seen before.
// Returns:
//   - (nil, false, nil) if key not seen – caller should proceed.
//   - (entry, true, nil) if key seen with same bodyHash – return cached.
//   - (nil, false, error) if key seen with different bodyHash – ALREADY_EXISTS.
func (m *MemoryStore) CheckIdempotency(requestID, bodyHash string) (*IdempotencyEntry, bool, error) {
	m.idempotencyMu.RLock()
	defer m.idempotencyMu.RUnlock()
	entry, ok := m.idempotencyKeys[requestID]
	if !ok {
		return nil, false, nil
	}
	if entry.BodyHash == bodyHash {
		return entry, true, nil
	}
	// Same key + different body → ALREADY_EXISTS(9)
	return nil, false, ErrAlreadyExists
}

// SetIdempotency stores an idempotency entry for the given request_id.
func (m *MemoryStore) SetIdempotency(requestID, bodyHash string, response interface{}) {
	m.idempotencyMu.Lock()
	defer m.idempotencyMu.Unlock()
	m.idempotencyKeys[requestID] = &IdempotencyEntry{
		BodyHash: bodyHash,
		Response: response,
	}
}

// ---- helpers ----

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// GenerateID is a simple unique-ID generator using nanosecond timestamp.
// In production, use UUID.
func GenerateID(prefix string) string {
	return prefix + "-" + time.Now().Format("20060102150405.000000000")
}

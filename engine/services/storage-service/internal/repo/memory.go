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
	// ADR-228（2026-09-12 幂等三套统一）：与 Redis 档同口径——24h 去重窗口
	//（redis.go idemTTL）+ FIFO 上限 10k（内存防护）。窗口外同键视同新请求。
	idempotencyMu   sync.RWMutex
	idempotencyKeys map[string]*IdempotencyEntry
	idempotencyOrd  []string // FIFO 驱逐序（首次插入序）
	now             func() time.Time
}

// 内存档幂等窗口/上界（ADR-228；窗口与 redis.go idemTTL 同值，两侧不可漂移）。
const (
	idemWindow  = 24 * time.Hour
	idemMaxKeys = 10_000
)

// IdempotencyEntry tracks an idempotent request.
type IdempotencyEntry struct {
	BodyHash  string      // summary of the request body for duplicate detection
	Response  interface{} // cached response
	CreatedAt time.Time   // 窗口判定锚（ADR-228；Redis 档由服务端 TTL 承担）
}

// NewMemoryStore creates a new empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		files:           make(map[string]*v1.StoredFile),
		fileData:        make(map[string][]byte),
		notifications:   make(map[string]*v1.Notification),
		idempotencyKeys: make(map[string]*IdempotencyEntry),
		now:             time.Now,
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
//   - (nil, false, nil) if key not seen (or past the 24h window, ADR-228) – proceed.
//   - (entry, true, nil) if key seen with same bodyHash – return cached.
//   - (nil, false, error) if key seen with different bodyHash – ALREADY_EXISTS.
func (m *MemoryStore) CheckIdempotency(requestID, bodyHash string) (*IdempotencyEntry, bool, error) {
	m.idempotencyMu.RLock()
	entry, ok := m.idempotencyKeys[requestID]
	m.idempotencyMu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if m.now().Sub(entry.CreatedAt) > idemWindow {
		// 窗口外（03 §2/ADR-228）：同键视同新请求；顺带清除过期项
		m.idempotencyMu.Lock()
		if cur, still := m.idempotencyKeys[requestID]; still && cur == entry {
			delete(m.idempotencyKeys, requestID)
			for i, id := range m.idempotencyOrd {
				if id == requestID {
					m.idempotencyOrd = append(m.idempotencyOrd[:i], m.idempotencyOrd[i+1:]...)
					break
				}
			}
		}
		m.idempotencyMu.Unlock()
		return nil, false, nil
	}
	if entry.BodyHash == bodyHash {
		return entry, true, nil
	}
	// Same key + different body → ALREADY_EXISTS(9)
	return nil, false, ErrAlreadyExists
}

// SetIdempotency stores an idempotency entry for the given request_id.
// Beyond idemMaxKeys the oldest inserted key is dropped (FIFO, ADR-228).
func (m *MemoryStore) SetIdempotency(requestID, bodyHash string, response interface{}) {
	m.idempotencyMu.Lock()
	defer m.idempotencyMu.Unlock()
	if _, exists := m.idempotencyKeys[requestID]; !exists {
		m.idempotencyOrd = append(m.idempotencyOrd, requestID)
	}
	m.idempotencyKeys[requestID] = &IdempotencyEntry{
		BodyHash:  bodyHash,
		Response:  response,
		CreatedAt: m.now(),
	}
	for len(m.idempotencyOrd) > idemMaxKeys {
		oldest := m.idempotencyOrd[0]
		m.idempotencyOrd = m.idempotencyOrd[1:]
		delete(m.idempotencyKeys, oldest)
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

// PG 幂等实现（HA P1-a，ADR-250）：project 库 idempotency 表——多副本键可见面。
// 语义与 MemoryStore 等价（24h 窗/三态/回放完整响应）；PG 不可用错误如实上抛
// （handler 映射 Unavailable——诚实降级不伪装放行）。
package idempotency

import (
	"database/sql"
	"log"
	"time"
)

// PGStore — project 库幂等表实现。
type PGStore struct{ db *sql.DB }

// NewPGStore — 建表（ADR-111 幂等自建纪律）。
func NewPGStore(db *sql.DB) (*PGStore, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS idempotency(
  request_id TEXT PRIMARY KEY, body_hash TEXT NOT NULL,
  response JSONB NOT NULL, saved_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	return &PGStore{db: db}, nil
}

// Check — 窗口外行惰性删除（视同新请求）。
func (s *PGStore) Check(requestID, bodyHash string) (*Result, error) {
	var hash, response string
	var savedAt time.Time
	err := s.db.QueryRow(`SELECT body_hash, response, saved_at FROM idempotency WHERE request_id=$1`,
		requestID).Scan(&hash, &response, &savedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if time.Since(savedAt) > dedupWindow {
		_, _ = s.db.Exec(`DELETE FROM idempotency WHERE request_id=$1`, requestID)
		return nil, nil
	}
	if hash != bodyHash {
		return nil, &ErrAlreadyExists{RequestID: requestID}
	}
	return &Result{ResponseBytes: []byte(response)}, nil
}

// Save — best-effort 写穿（接口无 error 同内存签名；失败 log——与 service 层写穿
// 语义一致，PG 故障=中间件故障域 Q4）。
func (s *PGStore) Save(requestID, bodyHash string, result *Result) {
	_, err := s.db.Exec(`INSERT INTO idempotency(request_id, body_hash, response)
VALUES($1,$2,$3) ON CONFLICT(request_id) DO UPDATE SET body_hash=EXCLUDED.body_hash, response=EXCLUDED.response, saved_at=now()`,
		requestID, bodyHash, string(result.ResponseBytes))
	if err != nil {
		log.Printf("[project-idem-pg] save %s 失败（本进程幂等仍有效、跨副本暂不可见）: %v", requestID, err)
	}
}

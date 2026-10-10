// RPC 幂等面外置（HA P1-b，ADR-250；主仓库 docs/designs/platform-ha-2026-09-19.md
// Q3/Q9——执行器保持无库身份，幂等走 Redis：键形对齐 storage-service 先例
// `ca:idem:` + TTL 24h）。语义=现役内存版等价迁移（同键同体回放原响应/异体
// AlreadyExists/新键放行）；Redis 不可用=诚实降级进程内存档（跨副本幂等暂失效，
// 单进程三态仍成立——PG/Redis 故障=中间件故障域，Q4 边界外）。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// idemState — 三态判定结果。
type idemState int

const (
	idemNew      idemState = iota // 新键：放行执行
	idemReplay                    // 同键同体：回放缓存响应
	idemConflict                  // 同键异体：ALREADY_EXISTS
)

// idemStore — 幂等键面抽象（内存/Redis 双实现）。
type idemStore interface {
	// check — 按 request_id 查指纹与缓存响应（resp 仅 idemReplay 时非 nil）。
	check(ctx context.Context, requestID, fingerprint string) (idemState, []byte, error)
	// save — 首次成功后缓存（fingerprint+序列化响应）。
	save(ctx context.Context, requestID, fingerprint string, resp []byte) error
}

// ---- 内存实现（缺省档；即现役 map 语义的接口化迁移） ----

type memIdemStore struct {
	mu    sync.Mutex
	m     map[string]memEntry
	order []string // FIFO 防无界（顺带修现役无淘汰泄漏面，ADR-250）
}
type memEntry struct {
	fp      string
	resp    []byte
	savedAt time.Time
}

const (
	idemWindow     = 24 * time.Hour
	idemMaxEntries = 10_000
)

func newMemIdemStore() *memIdemStore { return &memIdemStore{m: map[string]memEntry{}} }

func (s *memIdemStore) check(_ context.Context, requestID, fingerprint string) (idemState, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[requestID]
	if !ok || time.Since(e.savedAt) > idemWindow {
		return idemNew, nil, nil
	}
	if e.fp != fingerprint {
		return idemConflict, nil, nil
	}
	return idemReplay, e.resp, nil
}

func (s *memIdemStore) save(_ context.Context, requestID, fingerprint string, resp []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[requestID]; !ok {
		s.order = append(s.order, requestID)
		if len(s.order) > idemMaxEntries {
			delete(s.m, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.m[requestID] = memEntry{fp: fingerprint, resp: resp, savedAt: time.Now()}
	return nil
}

// ---- Redis 实现（跨副本可见面） ----

type redisIdemStore struct{ rdb *redis.Client }

func newRedisIdemStore(addr string) (*redisIdemStore, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return &redisIdemStore{rdb: rdb}, nil
}

func (s *redisIdemStore) key(requestID string) string {
	return "ca:idem:dsh:" + requestID // storage 先例 ca:idem: 前缀族
}

func (s *redisIdemStore) check(ctx context.Context, requestID, fingerprint string) (idemState, []byte, error) {
	raw, err := s.rdb.Get(ctx, s.key(requestID)).Bytes()
	if err == redis.Nil {
		return idemNew, nil, nil
	}
	if err != nil {
		return idemNew, nil, err // Redis 故障：如实上抛（调用方降级新键执行+日志）
	}
	var e idemPayload
	if err := json.Unmarshal(raw, &e); err != nil {
		return idemNew, nil, fmt.Errorf("idem payload 损坏: %w", err)
	}
	if e.FP != fingerprint {
		return idemConflict, nil, nil
	}
	return idemReplay, e.Resp, nil
}

// idemPayload — Redis 序列化形态（字段必须导出——memEntry 未导出字段 Marshal
// 恒 {}，DOOR4 实证存档全丢即此因）。
type idemPayload struct {
	FP      string    `json:"fp"`
	Resp    []byte    `json:"resp"`
	SavedAt time.Time `json:"saved_at"`
}

func (s *redisIdemStore) save(ctx context.Context, requestID, fingerprint string, resp []byte) error {
	raw, err := json.Marshal(idemPayload{FP: fingerprint, Resp: resp, SavedAt: time.Now()})
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, s.key(requestID), raw, idemWindow).Err()
}

// newIdemStore — 装配（best-effort Redis 档）：addr 空=内存；拨号失败=降级内存+日志。
func newIdemStore(addr string) idemStore {
	if addr == "" {
		return newMemIdemStore()
	}
	if s, err := newRedisIdemStore(addr); err == nil {
		log.Printf("[runtime] 幂等 Redis 档启用（%s；跨副本三态可见）", addr)
		return s
	} else {
		log.Printf("[runtime] 幂等 Redis 不可用（%v）——降级进程内存档（跨副本幂等暂失效，单进程三态仍成立；ADR-250）", err)
		return newMemIdemStore()
	}
}

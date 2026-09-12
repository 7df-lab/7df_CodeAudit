// 内存档幂等契约测试（03 §2 三态 + ADR-228 窗口/上限；与 Redis 档 idemTTL 同口径、
// 与 project-service idempotency.Store / task-service 实体兜底层同表互锁——改口径
// 必须三侧齐红，ADR-225 #5 双份镜像互锁同款纪律）。窗口用注入时钟锁定（不睡眠）。
package repo

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func newMemStore() (*MemoryStore, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	m := NewMemoryStore()
	m.now = clock.Now
	return m, clock
}

func TestMemoryIdem_ThreeStates(t *testing.T) {
	m, _ := newMemStore()
	// 新键 → proceed
	if e, hit, err := m.CheckIdempotency("r1", "h1"); e != nil || hit || err != nil {
		t.Fatalf("new key: e=%v hit=%v err=%v", e, hit, err)
	}
	m.SetIdempotency("r1", "h1", "resp")
	// 同键同体 → 命中缓存
	if e, hit, err := m.CheckIdempotency("r1", "h1"); !hit || err != nil || e == nil || e.Response != "resp" {
		t.Fatalf("same key+body: e=%v hit=%v err=%v", e, hit, err)
	}
	// 同键异体 → ALREADY_EXISTS 语义
	_, _, err := m.CheckIdempotency("r1", "h2")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("same key diff body: err=%v", err)
	}
}

func TestMemoryIdem_Window24h(t *testing.T) {
	m, clock := newMemStore()
	m.SetIdempotency("r1", "h1", "a")
	clock.t = clock.t.Add(idemWindow - time.Minute)
	if _, hit, err := m.CheckIdempotency("r1", "h1"); !hit || err != nil {
		t.Fatalf("窗口内应命中: hit=%v err=%v", hit, err)
	}
	clock.t = clock.t.Add(2 * time.Minute) // 越过 24h
	if e, hit, err := m.CheckIdempotency("r1", "h1"); e != nil || hit || err != nil {
		t.Fatalf("窗口外同键视同新请求: e=%v hit=%v err=%v", e, hit, err)
	}
	// 窗口外同键异体也不再报冲突（新请求语义）
	if _, _, err := m.CheckIdempotency("r1", "h9"); err != nil {
		t.Fatalf("窗口外同键异体不应冲突: err=%v", err)
	}
}

func TestMemoryIdem_FIFOCap(t *testing.T) {
	m, _ := newMemStore()
	m.SetIdempotency("first", "h", "1")
	for i := 0; i < idemMaxKeys; i++ {
		m.SetIdempotency(fmt.Sprintf("r-%06d", i), "h", "x")
	}
	if _, hit, err := m.CheckIdempotency("first", "h"); hit || err != nil {
		t.Fatalf("FIFO 驱逐未生效: hit=%v err=%v", hit, err)
	}
	if _, hit, err := m.CheckIdempotency(fmt.Sprintf("r-%06d", idemMaxKeys-1), "h"); !hit || err != nil {
		t.Fatalf("最新键应保留: hit=%v err=%v", hit, err)
	}
	if len(m.idempotencyKeys) > idemMaxKeys || len(m.idempotencyOrd) > idemMaxKeys {
		t.Fatalf("上界失守: keys=%d ord=%d", len(m.idempotencyKeys), len(m.idempotencyOrd))
	}
}

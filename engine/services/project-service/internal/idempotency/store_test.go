// store 契约测试（03 §2 三态 + ADR-228 窗口/上限）。窗口用注入时钟锁定（不睡眠）；
// 三态/窗口/驱逐与 storage-service 内存档、task-service 实体兜底层同表互锁——
// 改口径必须三侧齐红（ADR-225 #5 双份镜像互锁同款纪律）。
package idempotency

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore() (*Store, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	s := New()
	s.now = clock.Now
	return s, clock
}

func TestStore_ThreeStates(t *testing.T) {
	s, _ := newTestStore()
	// 新键 → proceed
	if r, err := s.Check("k1", "h1"); r != nil || err != nil {
		t.Fatalf("new key: r=%v err=%v", r, err)
	}
	s.Save("k1", "h1", &Result{ResponseBytes: []byte("resp")})
	// 同键同体 → 重放首次应答
	r, err := s.Check("k1", "h1")
	if err != nil || r == nil || string(r.ResponseBytes) != "resp" {
		t.Fatalf("same key+body: r=%v err=%v", r, err)
	}
	// 同键异体 → ALREADY_EXISTS 语义错误
	_, err = s.Check("k1", "h2")
	var ae *ErrAlreadyExists
	if !errors.As(err, &ae) || ae.RequestID != "k1" {
		t.Fatalf("same key diff body: err=%v", err)
	}
}

func TestStore_Window24h(t *testing.T) {
	s, clock := newTestStore()
	s.Save("k1", "h1", &Result{ResponseBytes: []byte("a")})
	clock.Advance(24*time.Hour - time.Minute)
	if r, err := s.Check("k1", "h1"); err != nil || r == nil {
		t.Fatalf("window 内应重放: r=%v err=%v", r, err)
	}
	clock.Advance(2 * time.Minute) // 越过 24h（03 §2 窗口）
	if r, err := s.Check("k1", "h1"); r != nil || err != nil {
		t.Fatalf("窗口外同键视同新请求: r=%v err=%v", r, err)
	}
	// 窗口外可同体重放新值（过期项已被清除）
	s.Save("k1", "h1", &Result{ResponseBytes: []byte("b")})
	if r, _ := s.Check("k1", "h1"); r == nil || string(r.ResponseBytes) != "b" {
		t.Fatalf("窗口外重写未生效: %v", r)
	}
}

func TestStore_FIFOCap(t *testing.T) {
	s, _ := newTestStore()
	s.Save("first", "h", &Result{ResponseBytes: []byte("1")})
	for i := 0; i < maxEntries; i++ { // 填满到上限（first 在队首）
		s.Save(mkKey(i), "h", &Result{ResponseBytes: []byte("x")})
	}
	// first 已被 FIFO 驱逐 → 视同新键；最后写入的键仍在
	if r, err := s.Check("first", "h"); r != nil || err != nil {
		t.Fatalf("FIFO 驱逐未生效: r=%v err=%v", r, err)
	}
	if r, err := s.Check(mkKey(maxEntries-1), "h"); err != nil || r == nil {
		t.Fatalf("最新键应保留: r=%v err=%v", r, err)
	}
	if len(s.entries) > maxEntries || len(s.order) > maxEntries {
		t.Fatalf("上界失守: entries=%d order=%d", len(s.entries), len(s.order))
	}
}

func mkKey(i int) string { return fmt.Sprintf("k-%06d", i) }

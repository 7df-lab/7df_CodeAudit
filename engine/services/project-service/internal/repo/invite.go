// Package repo — 邀请码存储（V2.2 ADR-255）。
// 记录=v1.InviteCode 直存（码本体在 proto 内，无 proto 外秘密字段，免 Record 包装）。
// 内存档：map 以 code 为键（消费主键）；id 操作线性扫（码为管理员手建，量级小）。
// PG 档（mirror_pg.go 同文件族）：code 主键+谓词列(state/max_uses/used_count/expires)+
// payload JSONB（仓惯例）——消费原子性=SQL 条件更新（多副本权威），内存档=互斥锁内
// 同谓词（单副本语义）。谓词双档同源=本文件 InviteUsable/InviteTerminal（防两处漂移）。
package repo

import (
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/codeaudit/proto-gen"
)

// InviteMirror — 邀请码镜像窄接口（ADR-254 同型：具体类型无 fake 不可单测）。
// nil=内存档。ConsumeInviteCode 为 PG 权威原子消费（CAS 条件更新）。
type InviteMirror interface {
	UpsertInviteCode(rec *v1.InviteCode) error
	ListInviteCodes() ([]*v1.InviteCode, error)
	GetInviteCode(code string) (*v1.InviteCode, error) // nil, nil = 未命中
	ConsumeInviteCode(code, userID string, now time.Time) (bool, error)
	DeleteInviteCode(id string) error
}

// InviteUsable — 消费谓词（内存档锁内与 PG 档 WHERE 同语义；改动须双档同步）：
// active 且未用尽且未过期。
func InviteUsable(rec *v1.InviteCode, now time.Time) bool {
	if rec.GetState() != "active" {
		return false
	}
	if rec.GetMaxUses() > 0 && rec.GetUsedCount() >= rec.GetMaxUses() {
		return false
	}
	if rec.GetExpiresAt() != nil && !rec.GetExpiresAt().AsTime().After(now) {
		return false
	}
	return true
}

// InviteTerminal — 终态判定（DeleteInviteCode 护栏 Q14 与 web 状态派生共用口径）：
// 已吊销 / 已用尽 / 已过期。state 域仅 {active,revoked}，故恰为 InviteUsable 的补集——
// 由此派生而非重写三条件（审查批收敛：消同义双写漂移面）。
func InviteTerminal(rec *v1.InviteCode, now time.Time) bool { return !InviteUsable(rec, now) }

// ---- MemoryStore 方法 ----

// CreateInviteCode — 建码（code 撞键=false，调用方重试新码）。
func (m *MemoryStore) CreateInviteCode(rec *v1.InviteCode) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.invites[rec.GetCode()]; exists {
		return false
	}
	m.invites[rec.GetCode()] = rec
	return true
}

// PutInviteCode — 回放/灌回用 upsert（镜像 hydrate 与 PG 消费后同步内存副本）。
func (m *MemoryStore) PutInviteCode(rec *v1.InviteCode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[rec.GetCode()] = rec
}

// GetInviteCode — 按 code 读（消费回落与注册校验共用）。
func (m *MemoryStore) GetInviteCode(code string) (*v1.InviteCode, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.invites[code]
	return rec, ok
}

// GetInviteCodeByID — 按 id 读（管理操作寻址）。
func (m *MemoryStore) GetInviteCodeByID(id string) (*v1.InviteCode, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.invites {
		if rec.GetId() == id {
			return rec, true
		}
	}
	return nil, false
}

// ListInviteCodes — 全量（created_at 倒序稳定序，分页在 handler 层）。
func (m *MemoryStore) ListInviteCodes() []*v1.InviteCode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*v1.InviteCode, 0, len(m.invites))
	for _, rec := range m.invites {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].GetCreatedAt().AsTime(), out[j].GetCreatedAt().AsTime()
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].GetId() > out[j].GetId() // 同刻并列以 id 破平（稳定序）
	})
	return out
}

// UpdateInviteCode — 状态翻转/计数写回（存在才写）。
func (m *MemoryStore) UpdateInviteCode(rec *v1.InviteCode) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.invites[rec.GetCode()]; !exists {
		return false
	}
	m.invites[rec.GetCode()] = rec
	return true
}

// ConsumeInviteCode — 内存档原子消费（互斥锁内谓词判定+计数+归属三步一体；
// 单副本语义，PG 档的跨副本权威见 mirror ConsumeInviteCode）。
func (m *MemoryStore) ConsumeInviteCode(code, userID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.invites[code]
	if !ok || !InviteUsable(rec, now) {
		return false
	}
	rec.UsedCount++
	rec.LastUsedBy = userID
	rec.LastUsedAt = timestamppb.New(now)
	return true
}

// DeleteInviteCode — 按 id 硬删（护栏在 service 层，存储不重复裁决）。
func (m *MemoryStore) DeleteInviteCode(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for code, rec := range m.invites {
		if rec.GetId() == id {
			delete(m.invites, code)
			return true
		}
	}
	return false
}

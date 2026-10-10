// revision 注册表（切片 2 §3 草案）：append-only 修订史 + 哈希命中复用 + 任务钉修订版。
// 依据: 主仓库 docs/designs/platform-slice2-pack-schema.md §3（pack_revision 表语义草案：
// 同 spec 重复提交不产生新 revision；回退到历史 spec 产生新 revision；任务运行期 pack
// 更新不影响已创建任务）；公理 A2（append-only）/C3；ADR-245。
//
// 持久化：PG 写穿镜像（与任务实体 R-31 同模式——内存是运行期权威，PG 是持久面；
// DSN 空=内存档诚实降级）。
package pack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Revision — 一条 pack 修订（immutable；设计 §3 表草案的内存投影）。
type Revision struct {
	Name        string    `json:"name"`
	Revision    int       `json:"revision"`
	SpecHash    string    `json:"spec_hash"`
	SpecJSON    string    `json:"spec_json"`
	Description string    `json:"description"` // pack 顶层描述（编排 CRUD 批 2026-09-21 起携带）
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// ID — 修订引用形态（task.config 约定键 pack_revision_id 的值）。
func (r *Revision) ID() string { return fmt.Sprintf("%s@%d", r.Name, r.Revision) }

// Registry — pack 修订注册表（内存权威；PG 写穿）。
type Registry struct {
	mu     sync.Mutex
	byName map[string][]*Revision // name → revisions（revision 升序 append-only）
	byHash map[string]*Revision   // name+"\x00"+hash → rev（哈希命中复用）
	mirror Mirror                 // 可空：PG 写穿镜像
	now    func() time.Time       // 测试注入点

	deprecated     map[string]bool                          // 停用标记（name 级；编排 CRUD 批 2026-09-21）
	deprecatedHook func(name string, deprecated bool) error // PG 持久钩子（可空=内存档）
}

// Mirror — 持久镜像面（pgPackStore 实现；nil=内存档）。Deprecated/SetDeprecated
// 为编排 CRUD 批扩展（停用标记持久；实现方可空操作降级=重启丢停用态）。
type Mirror interface {
	Upsert(rev *Revision) error
	ListAll() ([]*Revision, error)
	SetDeprecated(name string, deprecated bool) error
	ListDeprecated() (map[string]bool, error)
}

// NewRegistry — 空注册表。
func NewRegistry() *Registry {
	return &Registry{
		byName:     map[string][]*Revision{},
		byHash:     map[string]*Revision{},
		now:        time.Now,
		deprecated: map[string]bool{},
	}
}

// SetDeprecatedHook — 停用标记持久钩子（编排 CRUD 批；nil=内存档）。
func (r *Registry) SetDeprecatedHook(h func(name string, deprecated bool) error) {
	r.deprecatedHook = h
}

// SetDeprecated — name 级停用/恢复（停用的编排不进建任务可选与默认列表；历史
// 任务钉住的修订引用不受影响——append-only 语义不变）。
func (r *Registry) SetDeprecated(name string, deprecated bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if deprecated {
		r.deprecated[name] = true
	} else {
		delete(r.deprecated, name)
	}
	if r.deprecatedHook != nil {
		_ = r.deprecatedHook(name, deprecated) // 持久失败只影响重启后的停用态（可重标）
	}
}

// IsDeprecated — 停用态查询。
func (r *Registry) IsDeprecated(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deprecated[name]
}

// LatestAll — 每 name 取最新修订（编排列表投影；无停用过滤——过滤归调用方）。
func (r *Registry) LatestAll() map[string]*Revision {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]*Revision{}
	for name, revs := range r.byName {
		if len(revs) > 0 {
			out[name] = revs[len(revs)-1]
		}
	}
	return out
}

// History — 单 name 全量修订（升序副本；原 Revisions 同义重复已并此，ADR-219 死代码清理同批）。
func (r *Registry) History(name string) []*Revision {
	r.mu.Lock()
	defer r.mu.Unlock()
	revs := r.byName[name]
	out := make([]*Revision, len(revs))
	copy(out, revs)
	return out
}

// SetMirror — 挂持久镜像并回放历史（须在任何 Register 之前调用；镜像失败 fail-loud）。
func (r *Registry) SetMirror(m Mirror) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := m.ListAll()
	if err != nil {
		return err
	}
	for _, rev := range rows {
		r.byName[rev.Name] = append(r.byName[rev.Name], rev)
		r.byHash[rev.Name+"\x00"+rev.SpecHash] = rev
	}
	if dep, err := m.ListDeprecated(); err != nil {
		log.Printf("[pack-registry] 停用标记回放失败（降级=重启后停用态丢失，可重标）: %v", err)
	} else {
		for name := range dep {
			r.deprecated[name] = true
		}
	}
	r.mirror = m
	return nil
}

// ErrPackExists — RegisterIfAbsent 的已存在哨兵（create-only 语义）。
var ErrPackExists = errors.New("pack 已存在")

// Register — 注册一个已校验 pack。复用判定取最新修订哈希（回退=新修订，append-only）。
// 返回生效修订与 reused 标记。
func (r *Registry) Register(p *Pack, createdBy string) (rev *Revision, reused bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerLocked(p, createdBy, false)
}

// RegisterIfAbsent — create-only 注册（编排 Create 面）：锁内探测+追加原子化，
// 并发同名 Create 恰一成功（另一个收 ErrPackExists），不产生双修订。
func (r *Registry) RegisterIfAbsent(p *Pack, createdBy string) (rev *Revision, reused bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerLocked(p, createdBy, true)
}

func (r *Registry) registerLocked(p *Pack, createdBy string, ifAbsent bool) (rev *Revision, reused bool, err error) {
	hash, err := p.SpecHash()
	if err != nil {
		return nil, false, err
	}
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return nil, false, err
	}
	if ifAbsent {
		if latest := r.byName[p.Name]; len(latest) > 0 {
			return nil, false, ErrPackExists
		}
	}
	// 复用判定（设计 §3 语义精确形）：哈希命中**最新修订**=复用不追加；与历史某修订
	// 相同但非最新（回退）=追加新修订（append-only，不改历史）。
	if latest := r.byName[p.Name]; len(latest) > 0 && latest[len(latest)-1].SpecHash == hash {
		head := latest[len(latest)-1]
		if head.Description != p.Description { // 元数据刷新不产生新修订（revision 不变）
			// 副本替换而非原地写：Revision 契约=immutable，LatestAll/History 返回
			// 共享指针，原地 mutate 即读侧数据竞态。byName 头与 byHash 同步换新副本。
			fresh := *head
			fresh.Description = p.Description
			latest[len(latest)-1] = &fresh
			r.byHash[p.Name+"\x00"+hash] = &fresh
			if r.mirror != nil {
				_ = r.mirror.Upsert(&fresh) // 持久失败降级（同 Register 错误面纪律）
			}
			return &fresh, true, nil
		}
		return head, true, nil // 同 spec 重复提交不产生新 revision（设计 §3）
	}
	rev = &Revision{
		Name: p.Name, Revision: len(r.byName[p.Name]) + 1, SpecHash: hash,
		SpecJSON: string(specJSON), Description: p.Description, CreatedBy: createdBy, CreatedAt: r.now(),
	}
	if r.mirror != nil {
		if err := r.mirror.Upsert(rev); err != nil {
			return nil, false, fmt.Errorf("pack revision persist: %w", err)
		}
	}
	r.byName[rev.Name] = append(r.byName[rev.Name], rev)
	r.byHash[rev.Name+"\x00"+hash] = rev
	delete(r.deprecated, rev.Name) // 新修订=复活（停用态随最新修订消失）
	// 复活持久化：不落 hook 则 PG 回放把停用态复活
	// 回来——同进程内存与持久面矛盾，重启后静默翻转。持久失败降级（同 SetDeprecated
	// 错误面纪律：重启后停用态多活一轮，可重标）。
	if r.deprecatedHook != nil {
		_ = r.deprecatedHook(rev.Name, false)
	}
	return rev, false, nil
}

// PinLatest — 任务创建时钉死修订版（审计性：定义冻结在创建时刻；设计 §3 task 表增列语义）。
// 未知 pack = 错误（fail-fast，不静默回落缺省负载）。
func (r *Registry) PinLatest(name string) (*Revision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	revs := r.byName[name]
	if len(revs) == 0 {
		return nil, fmt.Errorf("未知 pack %q（已注册: %s）", name, r.names())
	}
	return revs[len(revs)-1], nil
}

// Lookup — 按 ID（name@revision，task.config 约定键形态）取修订（切片 5+6 ADR-249：
// 任务启动时快照重读——Revision immutable，与创建时钉扎等价）。未知 ID=错误。
func (r *Registry) Lookup(id string) (*Revision, error) {
	at := strings.LastIndexByte(id, '@')
	if at <= 0 || at == len(id)-1 {
		return nil, fmt.Errorf("修订引用 %q 非 name@revision 形态", id)
	}
	name, revNum := id[:at], 0
	for _, c := range id[at+1:] {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("修订引用 %q 版本段非数字", id)
		}
		revNum = revNum*10 + int(c-'0')
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	revs := r.byName[name]
	if revNum < 1 || revNum > len(revs) {
		return nil, fmt.Errorf("修订 %q 不在注册表（pack %q 共 %d 修订）", id, name, len(revs))
	}
	return revs[revNum-1], nil
}

// Names — 已注册 pack 名（升序；诊断/错误信息用）。
func (r *Registry) names() string {
	var out []string
	for k := range r.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "（无）"
	}
	return strings.Join(out, ", ")
}

// ---- PG 写穿镜像（R-31 同模式；ADR-245）----

// PGMirror — pack_revisions 表写穿（UNIQUE(name, revision) 即设计 §3 唯一约束）。
type PGMirror struct{ db *sql.DB }

// NewPGMirror — 复用任务库连接，启动自建表（ADR-111 同纪律）。
func NewPGMirror(db *sql.DB) (*PGMirror, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pack_revisions (
		name        TEXT NOT NULL,
		revision    INTEGER NOT NULL,
		spec_hash   TEXT NOT NULL,
		spec_json   JSONB NOT NULL,
		created_by  TEXT NOT NULL DEFAULT '',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		description TEXT NOT NULL DEFAULT '',
		UNIQUE(name, revision)
	)`); err != nil {
		return nil, fmt.Errorf("migrate pack_revisions: %w", err)
	}
	// 存量库补列（幂等迁移，ADR-245 同纪律）
	if _, err := db.Exec(`ALTER TABLE pack_revisions ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT ''`); err != nil {
		return nil, fmt.Errorf("migrate pack_revisions.description: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pack_deprecated (
		name      TEXT PRIMARY KEY,
		deployed_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("migrate pack_deprecated: %w", err)
	}
	return &PGMirror{db: db}, nil
}

// Upsert — 写穿单条修订。
func (m *PGMirror) Upsert(rev *Revision) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // R77 同型：锁内写穿封顶 2s
	defer cancel()
	_, err := m.db.ExecContext(ctx, `INSERT INTO pack_revisions
		(name, revision, spec_hash, spec_json, created_by, created_at, description)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name, revision) DO UPDATE SET spec_hash = EXCLUDED.spec_hash, spec_json = EXCLUDED.spec_json, description = EXCLUDED.description`,
		rev.Name, rev.Revision, rev.SpecHash, rev.SpecJSON, rev.CreatedBy, rev.CreatedAt, rev.Description)
	return err
}

// SetDeprecated / ListDeprecated — 停用标记持久（编排 CRUD 批）。
func (m *PGMirror) SetDeprecated(name string, deprecated bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if deprecated {
		_, err := m.db.ExecContext(ctx, `INSERT INTO pack_deprecated(name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, name)
		return err
	}
	_, err := m.db.ExecContext(ctx, `DELETE FROM pack_deprecated WHERE name = $1`, name)
	return err
}

func (m *PGMirror) ListDeprecated() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `SELECT name FROM pack_deprecated`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ListAll — 全量回放（升序）。
func (m *PGMirror) ListAll() ([]*Revision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `SELECT name, revision, spec_hash, spec_json, created_by, created_at, description
		FROM pack_revisions ORDER BY name ASC, revision ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Revision
	for rows.Next() {
		var rev Revision
		if err := rows.Scan(&rev.Name, &rev.Revision, &rev.SpecHash, &rev.SpecJSON, &rev.CreatedBy, &rev.CreatedAt, &rev.Description); err != nil {
			return nil, err
		}
		out = append(out, &rev)
	}
	return out, rows.Err()
}

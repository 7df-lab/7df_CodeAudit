// Package repo — PGMirror：project 库写穿镜像+读回落源（HA P1-a，ADR-250；
// 主仓库 docs/designs/platform-ha-2026-09-19.md Q3/Q6）。
//
// 架构=task-service R-31 同型（内存权威+PG 写穿+读回落）：MemoryStore 保持内存
// 语义不动；本镜像承担①写点同步写穿（失败上抛→RPC Unavailable——双副本一致性
// 依赖 PG 为跨副本可见面）②读点 miss 回落（单读灌回内存）③启动全量 hydrate
// （黑名单/纪元/List 权威面）。DSN 空=内存档（现状零变化）；非空但 PG 不可用
// =启动 fail-loud（诚实，不静默内存分叉）。
package repo

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/lib/pq"
)

// PGMirror — project 库镜像（五实体表+黑名单/纪元两小表）。
type PGMirror struct{ db *sql.DB }

// NewPGMirror — 打开+Ping+幂等建表（ADR-111 同纪律：启动自建，无独立 migration）。
func NewPGMirror(dsn string) (*PGMirror, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("project pg open: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("project pg ping: %w", err)
	}
	m := &PGMirror{db: db}
	if err := m.createTables(); err != nil {
		db.Close()
		return nil, err
	}
	return m, nil
}

func (m *PGMirror) DB() *sql.DB     { return m.idemDB() }
func (m *PGMirror) idemDB() *sql.DB { return m.db }

func (m *PGMirror) createTables() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS projects(
  project_id TEXT PRIMARY KEY, payload JSONB NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS project_configs(project_id TEXT PRIMARY KEY, payload JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS project_stats(project_id TEXT PRIMARY KEY, payload JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS project_members(
  project_id TEXT NOT NULL, user_id TEXT NOT NULL, payload JSONB NOT NULL,
  PRIMARY KEY(project_id, user_id));
CREATE TABLE IF NOT EXISTS users(
  user_id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, email TEXT NOT NULL UNIQUE,
  password TEXT NOT NULL, payload JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS revoked_tokens(token TEXT PRIMARY KEY, revoked_at TIMESTAMPTZ NOT NULL);
CREATE TABLE IF NOT EXISTS session_epochs(user_id TEXT PRIMARY KEY, invalid_before TIMESTAMPTZ NOT NULL);
CREATE INDEX IF NOT EXISTS idx_projects_updated ON projects(updated_at);`
	if _, err := m.db.Exec(ddl); err != nil {
		return fmt.Errorf("project pg migrate: %w", err)
	}
	if _, err := m.db.Exec(inviteDDL); err != nil { // V2.2 ADR-255
		return fmt.Errorf("project pg invite migrate: %w", err)
	}
	return nil
}

// ---- 通用序列化（protojson；lib/pq 驱动经空白导入注册）----

var _ = pq.Driver{}

// ---- projects ----

// UpsertProject — 写穿（同步，失败上抛）。
func (m *PGMirror) UpsertProject(rec *ProjectRecord) error {
	b, err := protojson.Marshal(rec.Project)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`INSERT INTO projects(project_id, payload) VALUES($1,$2)
ON CONFLICT(project_id) DO UPDATE SET payload=EXCLUDED.payload, updated_at=now()`,
		rec.Project.GetProjectId(), string(b))
	return err
}

// GetProject — 读回落（ok=false=确实不存在）。
func (m *PGMirror) GetProject(id string) (*ProjectRecord, bool, error) {
	var payload string
	if err := m.db.QueryRow(`SELECT payload FROM projects WHERE project_id=$1`, id).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	var p v1.Project
	if err := protojson.Unmarshal([]byte(payload), &p); err != nil {
		return nil, false, err
	}
	return &ProjectRecord{Project: &p}, true, nil
}

// DeleteProject — 级联四表（config/stats/members 同事务）。
func (m *PGMirror) DeleteProject(id string) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM projects WHERE project_id=$1`,
		`DELETE FROM project_configs WHERE project_id=$1`,
		`DELETE FROM project_stats WHERE project_id=$1`,
		`DELETE FROM project_members WHERE project_id=$1`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListProjects — List 权威面（全量拉，供增量同步）。
func (m *PGMirror) ListProjects() ([]*ProjectRecord, error) {
	rows, err := m.db.Query(`SELECT payload FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectRecord
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var p v1.Project
		if err := protojson.Unmarshal([]byte(payload), &p); err != nil {
			return nil, err
		}
		out = append(out, &ProjectRecord{Project: &p})
	}
	return out, rows.Err()
}

// ---- configs / stats（1:1 共享主键） ----

func (m *PGMirror) UpsertProjectConfig(rec *ProjectConfigRecord) error {
	b, err := protojson.Marshal(rec.Config)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`INSERT INTO project_configs(project_id, payload) VALUES($1,$2)
ON CONFLICT(project_id) DO UPDATE SET payload=EXCLUDED.payload`, rec.Config.GetProjectId(), string(b))
	return err
}

func (m *PGMirror) GetProjectConfig(id string) (*ProjectConfigRecord, bool, error) {
	var payload string
	if err := m.db.QueryRow(`SELECT payload FROM project_configs WHERE project_id=$1`, id).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	var c v1.ProjectConfig
	if err := protojson.Unmarshal([]byte(payload), &c); err != nil {
		return nil, false, err
	}
	return &ProjectConfigRecord{Config: &c}, true, nil
}

func (m *PGMirror) UpsertProjectStats(rec *ProjectStatsRecord) error {
	b, err := protojson.Marshal(rec.Stats)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`INSERT INTO project_stats(project_id, payload) VALUES($1,$2)
ON CONFLICT(project_id) DO UPDATE SET payload=EXCLUDED.payload`, rec.Stats.GetProjectId(), string(b))
	return err
}

func (m *PGMirror) GetProjectStats(id string) (*ProjectStatsRecord, bool, error) {
	var payload string
	if err := m.db.QueryRow(`SELECT payload FROM project_stats WHERE project_id=$1`, id).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	var st v1.ProjectStats
	if err := protojson.Unmarshal([]byte(payload), &st); err != nil {
		return nil, false, err
	}
	return &ProjectStatsRecord{Stats: &st}, true, nil
}

// ---- members ----

func (m *PGMirror) UpsertMember(rec *ProjectMemberRecord) error {
	b, err := protojson.Marshal(rec.Member)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`INSERT INTO project_members(project_id, user_id, payload) VALUES($1,$2,$3)
ON CONFLICT(project_id, user_id) DO UPDATE SET payload=EXCLUDED.payload`,
		rec.Member.GetProjectId(), rec.Member.GetUserId(), string(b))
	return err
}

func (m *PGMirror) DeleteMember(projectID, userID string) error {
	_, err := m.db.Exec(`DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, userID)
	return err
}

func (m *PGMirror) ListMembers(projectID string) ([]*ProjectMemberRecord, error) {
	rows, err := m.db.Query(`SELECT payload FROM project_members WHERE project_id=$1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectMemberRecord
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var mb v1.ProjectMember
		if err := protojson.Unmarshal([]byte(payload), &mb); err != nil {
			return nil, err
		}
		out = append(out, &ProjectMemberRecord{Member: &mb})
	}
	return out, rows.Err()
}

// ListMembersForUser — 权限读回落：该用户跨全部项目的成员关系（全表扫 user 侧半键）。
func (m *PGMirror) ListMembersForUser(userID string) ([]*ProjectMemberRecord, error) {
	rows, err := m.db.Query(`SELECT payload FROM project_members WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectMemberRecord
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var mb v1.ProjectMember
		if err := protojson.Unmarshal([]byte(payload), &mb); err != nil {
			return nil, err
		}
		out = append(out, &ProjectMemberRecord{Member: &mb})
	}
	return out, rows.Err()
}

// ---- users（password 列分离+payload=proto） ----

func (m *PGMirror) UpsertUser(rec *UserRecord) error {
	b, err := protojson.Marshal(rec.User)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`INSERT INTO users(user_id, username, email, password, payload)
VALUES($1,$2,$3,$4,$5)
ON CONFLICT(user_id) DO UPDATE SET username=EXCLUDED.username, email=EXCLUDED.email,
  password=EXCLUDED.password, payload=EXCLUDED.payload`,
		rec.User.GetUserId(), rec.User.GetUsername(), rec.User.GetEmail(), rec.Password, string(b))
	return err
}

func scanUser(row interface{ Scan(...interface{}) error }) (*UserRecord, bool, error) {
	var payload, password string
	if err := row.Scan(&payload, &password); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	var u v1.User
	if err := protojson.Unmarshal([]byte(payload), &u); err != nil {
		return nil, false, err
	}
	return &UserRecord{User: &u, Password: password}, true, nil
}

func (m *PGMirror) GetUser(id string) (*UserRecord, bool, error) {
	return scanUser(m.db.QueryRow(`SELECT payload, password FROM users WHERE user_id=$1`, id))
}

func (m *PGMirror) GetUserByUsername(username string) (*UserRecord, bool, error) {
	return scanUser(m.db.QueryRow(`SELECT payload, password FROM users WHERE username=$1`, username))
}

func (m *PGMirror) ListUsers() ([]*UserRecord, error) {
	rows, err := m.db.Query(`SELECT payload, password FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserRecord
	for rows.Next() {
		rec, ok, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	return out, rows.Err()
}

// ---- 黑名单/会话纪元（写双面+启动 hydrate 全量） ----

func (m *PGMirror) SaveRevokedToken(token string, at time.Time) error {
	_, err := m.db.Exec(`INSERT INTO revoked_tokens(token, revoked_at) VALUES($1,$2)
ON CONFLICT(token) DO NOTHING`, token, at)
	return err
}

// HydrateRevoked — 启动/增量全量回放（过期行过滤，残留无害）。
func (m *PGMirror) HydrateRevoked(window time.Duration) (map[string]time.Time, error) {
	rows, err := m.db.Query(`SELECT token, revoked_at FROM revoked_tokens WHERE revoked_at > now() - $1::interval`,
		window.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var tok string
		var at time.Time
		if err := rows.Scan(&tok, &at); err != nil {
			return nil, err
		}
		out[tok] = at
	}
	return out, rows.Err()
}

func (m *PGMirror) SaveSessionEpoch(userID string, at time.Time) error {
	_, err := m.db.Exec(`INSERT INTO session_epochs(user_id, invalid_before) VALUES($1,$2)
ON CONFLICT(user_id) DO UPDATE SET invalid_before=EXCLUDED.invalid_before`, userID, at)
	return err
}

func (m *PGMirror) HydrateEpochs() (map[string]time.Time, error) {
	rows, err := m.db.Query(`SELECT user_id, invalid_before FROM session_epochs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var uid string
		var at time.Time
		if err := rows.Scan(&uid, &at); err != nil {
			return nil, err
		}
		out[uid] = at
	}
	return out, rows.Err()
}

// ---- invite_codes（V2.2 ADR-255；InviteMirror 窄接口实现） ----
// 表形沿仓惯例：code 主键+消费谓词列（state/max_uses/used_count/expires_at，
// ConsumeInviteCode 的 SQL WHERE 面）+payload JSONB（全量 proto）。
// 消费原子性=CAS 条件更新（used_count 乐观锁），多副本下 PG 为唯一权威。

const inviteDDL = `
CREATE TABLE IF NOT EXISTS invite_codes(
  code TEXT PRIMARY KEY, id TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'active',
  max_uses INTEGER NOT NULL DEFAULT 1, used_count INTEGER NOT NULL DEFAULT 0,
  expires_at TIMESTAMPTZ NULL, payload JSONB NOT NULL);
CREATE INDEX IF NOT EXISTS idx_invite_codes_id ON invite_codes(id);`

func (m *PGMirror) inviteCols(rec *v1.InviteCode) (state string, maxUses, usedCount int, expires interface{}, payload string, err error) {
	b, merr := protojson.Marshal(rec)
	if merr != nil {
		return "", 0, 0, nil, "", merr
	}
	if rec.GetExpiresAt() != nil {
		expiry := rec.GetExpiresAt().AsTime()
		expires = expiry
	}
	return rec.GetState(), int(rec.GetMaxUses()), int(rec.GetUsedCount()), expires, string(b), nil
}

// UpsertInviteCode — 建码/状态翻转写穿（谓词列与 payload 同步重写，防双源漂移）。
func (m *PGMirror) UpsertInviteCode(rec *v1.InviteCode) error {
	state, maxUses, usedCount, expires, payload, err := m.inviteCols(rec)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`INSERT INTO invite_codes(code, id, state, max_uses, used_count, expires_at, payload)
VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT(code) DO UPDATE SET id=EXCLUDED.id, state=EXCLUDED.state, max_uses=EXCLUDED.max_uses,
  used_count=EXCLUDED.used_count, expires_at=EXCLUDED.expires_at, payload=EXCLUDED.payload`,
		rec.GetCode(), rec.GetId(), state, maxUses, usedCount, expires, payload)
	return err
}

func scanInvite(row interface{ Scan(...interface{}) error }) (*v1.InviteCode, bool, error) {
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	rec := &v1.InviteCode{}
	if err := protojson.Unmarshal([]byte(payload), rec); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// ListInviteCodes — 全量回放/管理列表权威读（created_at 排序在内存档做）。
func (m *PGMirror) ListInviteCodes() ([]*v1.InviteCode, error) {
	rows, err := m.db.Query(`SELECT payload FROM invite_codes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*v1.InviteCode
	for rows.Next() {
		rec, ok, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	return out, rows.Err()
}

// GetInviteCode — 按 code 读（消费回落；nil,nil=未命中）。
func (m *PGMirror) GetInviteCode(code string) (*v1.InviteCode, error) {
	rec, ok, err := scanInvite(m.db.QueryRow(`SELECT payload FROM invite_codes WHERE code=$1`, code))
	if err != nil || !ok {
		return nil, err
	}
	return rec, nil
}

// ConsumeInviteCode — PG 权威原子消费：谓词（active/未用尽/未过期）+CAS（used_count
// 乐观锁防读改写竞态）一体；0 行=码不存在或谓词拒（统一 false，不泄露区分）。
// payload 重写携带 last_used 归属（Q9）。与 repo.InviteUsable 同语义（改动双档同步）。
func (m *PGMirror) ConsumeInviteCode(code, userID string, now time.Time) (bool, error) {
	var current *v1.InviteCode
	var ok bool
	var err error
	if current, ok, err = scanInvite(m.db.QueryRow(
		`SELECT payload FROM invite_codes WHERE code=$1`, code)); err != nil || !ok {
		return false, err
	}
	if !InviteUsable(current, now) {
		return false, nil
	}
	next := proto.Clone(current).(*v1.InviteCode)
	next.UsedCount++
	next.LastUsedBy = userID
	next.LastUsedAt = timestamppb.New(now)
	payload, merr := protojson.Marshal(next)
	if merr != nil {
		return false, merr
	}
	res, uerr := m.db.Exec(`UPDATE invite_codes SET used_count=$3, payload=$4
WHERE code=$1 AND used_count=$2 AND state='active'
  AND (max_uses=0 OR used_count<max_uses) AND (expires_at IS NULL OR expires_at>$5)`,
		code, current.GetUsedCount(), next.GetUsedCount(), string(payload), now)
	if uerr != nil {
		return false, uerr
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeleteInviteCode — 按 id 硬删（终态护栏在 service 层，存储不重复裁决）。
func (m *PGMirror) DeleteInviteCode(id string) error {
	_, err := m.db.Exec(`DELETE FROM invite_codes WHERE id=$1`, id)
	return err
}

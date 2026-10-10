// Package service — 任务实体 PG 持久化镜像（R-31）。
//
// 依据: R-31 dind 全新环境实测——task-service 任务实体此前为纯内存态，容器重建
// 即全量蒸发（findings 在 PG、产物在 MinIO 均幸存，产生悬挂引用）。
// 形态: 写穿镜像——内存 map 仍是运行期权威（既有并发模型不变），每次任务实体
// 变更同步 upsert 一份 protojson 全量 payload；启动时 hydrate 回放到内存 map。
// 中途丢失的运行期细节（stage 进度/contexts/执行日志环形缓存）不在此档：
// 这些属 ADR-167/编排执行期工件，任务级终态与创建史才是用户可见的持久面。
package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/codeaudit/proto-gen"
	_ "github.com/lib/pq"
)

// pgTaskStore — tasks 表写穿镜像（lib/pq，与 result-service 同驱动）。
type pgTaskStore struct {
	db *sql.DB
}

// newPGTaskStore — 打开连接并自建表（ADR-111: 启动时 CREATE TABLE IF NOT EXISTS）。
func newPGTaskStore(dsn string) (*pgTaskStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		task_id    TEXT PRIMARY KEY,
		project_id TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		payload    JSONB NOT NULL
	)`); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// 切片 2（ADR-245）：pack 修订钉扎列（约定键 task.Config["pack_revision_id"] 的
	// 可查询投影；存量库 ADD COLUMN IF NOT EXISTS 幂等）。
	if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pack_revision_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return nil, fmt.Errorf("migrate pack_revision_id: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id)`); err != nil {
		return nil, fmt.Errorf("migrate index: %w", err)
	}
	// HA P1-c（ADR-250）：日志尾窗表——append 双写+内存 miss 尾查回落；request_id
	// 唯一列承接 logIdem 幂等（冲突=查回原行回放）。
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_logs (
		task_id    TEXT NOT NULL,
		log_id     BIGINT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		ts_ms      BIGINT NOT NULL DEFAULT 0,
		level      TEXT NOT NULL DEFAULT '',
		source     TEXT NOT NULL DEFAULT '',
		message    TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(task_id, log_id)
	)`); err != nil {
		return nil, fmt.Errorf("migrate task_logs: %w", err)
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_task_logs_request ON task_logs(request_id) WHERE request_id <> ''`); err != nil {
		return nil, fmt.Errorf("migrate task_logs idx: %w", err)
	}
	// 切片 3 后半（ADR-247）：usage 计量归集表——逐事件行存不预聚合（观测忠实性，
	// 公理 D2）；(task_id, seq) 主键锚 baton 事件序；归集=事务全量重算（DELETE+INSERT，
	// 与 baton project 全量重写 events.jsonl 幂等同构）。tenant/user/session 为
	// envelope 打标（TaskOrigin 贯通成果，缺失=空串不虚构）。
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_usage (
		task_id            TEXT NOT NULL,
		seq                BIGINT NOT NULL,
		tenant             TEXT NOT NULL DEFAULT '',
		"user"             TEXT NOT NULL DEFAULT '',
		session            TEXT NOT NULL DEFAULT '',
		model              TEXT NOT NULL DEFAULT '',
		input_tokens       BIGINT NOT NULL DEFAULT 0,
		output_tokens      BIGINT NOT NULL DEFAULT 0,
		cache_read_tokens  BIGINT NOT NULL DEFAULT 0,
		cache_write_tokens BIGINT NOT NULL DEFAULT 0,
		reasoning_tokens   BIGINT NOT NULL DEFAULT 0,
		total_tokens       BIGINT NOT NULL DEFAULT 0,
		recorded_at        TIMESTAMPTZ,
		collected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (task_id, seq)
	)`); err != nil {
		return nil, fmt.Errorf("migrate task_usage: %w", err)
	}
	return &pgTaskStore{db: db}, nil
}

// dbh — 底层连接池（pack 修订镜像复用同一 PG；ADR-245 不开第二连接池）。
func (st *pgTaskStore) dbh() *sql.DB { return st.db }

// usageRow — task_usage 单事件行（ADR-247）。
type usageRow struct {
	Seq                   int64
	Tenant, User, Session string
	Model                 string
	In, Out               int64
	CacheR, CacheW        int64
	Reason, Total         int64
	RecordedAt            time.Time
}

// replaceUsage — 任务 usage 全量重算归集（ADR-247）：事务内 DELETE+批量 INSERT。
// 空行集=该任务无 usage 事件，仍执行 DELETE（清历史残留，文件是真相）。
func (st *pgTaskStore) replaceUsage(taskID string, rows []usageRow) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_usage WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	// 分块多值 INSERT（二轮复审 P3-2）：逐行 Exec 在千行级事件（长审计多轮）下
	// N 次 RTT 逼近 5s 事务上限，超限=整档回滚且终态已过无重试=usage 永久丢失；
	// 每 100 行一条多值语句，RTT 从 N 降到 N/100+2。SQL/参数构造在
	// usageInsertChunk（纯函数可测：占位编号连续性与列序锁定）。
	const chunk = 100
	for base := 0; base < len(rows); base += chunk {
		end := base + chunk
		if end > len(rows) {
			end = len(rows)
		}
		q, args := usageInsertChunk(taskID, rows[base:end])
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("insert chunk base=%d: %w", base, err)
		}
	}
	return tx.Commit()
}

// queryUsageRows — 按 task 全量读回（seq 升序；聚合在服务层做，行数任务级有界）。
func (st *pgTaskStore) queryUsageRows(taskID string) ([]usageRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := st.db.QueryContext(ctx, `SELECT seq, tenant, "user", session, model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens, recorded_at
		FROM task_usage WHERE task_id = $1 ORDER BY seq ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var r usageRow
		var recorded sql.NullTime
		if err := rows.Scan(&r.Seq, &r.Tenant, &r.User, &r.Session, &r.Model,
			&r.In, &r.Out, &r.CacheR, &r.CacheW, &r.Reason, &r.Total, &recorded); err != nil {
			return nil, err
		}
		if recorded.Valid {
			r.RecordedAt = recorded.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// upsert — 任务实体全量镜像（调用方持 s.mu；错误只记日志不反噬任务流，
// 运行期内存仍是权威，持久化失败属可观测降级而非请求失败）。
// 返回 PG 侧 updated_at（now()，R115 版本守卫的权威钟）：调用方回填内存实体，
// 使 taskByIDRW 的"仅 PG 严格新才换入"比较成立。
func (st *pgTaskStore) upsert(t *pb.ScanTask) (time.Time, error) {
	payload, err := protojson.Marshal(t)
	if err != nil {
		return time.Time{}, fmt.Errorf("marshal: %w", err)
	}
	// R77: persistTaskLocked 调用方持全局写锁——无超时 Exec 在 PG 抖动时会冻结
	// 创建/列表/快照/流式全任务面（R47 同型，慢源换 PG）。锁内写穿封顶 2s；
	// 错误仍只记日志不反噬任务流（R-31 语义不变）。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var updatedAt time.Time
	err = st.db.QueryRowContext(ctx, `INSERT INTO tasks (task_id, project_id, status, created_by, payload, pack_revision_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (task_id) DO UPDATE
		SET status = EXCLUDED.status, payload = EXCLUDED.payload, pack_revision_id = EXCLUDED.pack_revision_id, updated_at = now()
		RETURNING updated_at`,
		t.GetTaskId(), t.GetProjectId(), t.GetStatus().String(), t.GetCreatedBy(), string(payload), t.GetConfig()["pack_revision_id"]).Scan(&updatedAt)
	if err != nil {
		return time.Time{}, err
	}
	return updatedAt, nil
}

// getTask — 单任务回落读（HA P1-c：读口内存 miss→PG；false=确实不存在）。
// R115：盖 updated_at 列值进 proto.UpdatedAt——换入守卫的比较基准（PG 钟）。
func (st *pgTaskStore) getTask(taskID string) (*pb.ScanTask, bool, error) {
	var payload []byte
	var updatedAt time.Time
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := st.db.QueryRowContext(ctx, `SELECT payload, updated_at FROM tasks WHERE task_id=$1`, taskID).Scan(&payload, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	t := &pb.ScanTask{}
	if err := protojson.Unmarshal(payload, t); err != nil {
		return nil, false, fmt.Errorf("getTask %s: %w", taskID, err)
	}
	t.UpdatedAt = timestamppb.New(updatedAt)
	return t, true, nil
}

// insertLog — 日志双写（best-effort：失败 log 不反噬任务流；request_id 冲突=
// 幂等重放，静默吸收）。
func (st *pgTaskStore) insertLog(e *pb.TaskLogEntry, requestID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := st.db.ExecContext(ctx, `INSERT INTO task_logs(task_id, log_id, request_id, ts_ms, level, source, message)
VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		e.GetTaskId(), e.GetLogId(), requestID, e.GetTsMs(), e.GetLevel(), e.GetSource(), e.GetMessage())
	return err
}

// tailLogs — 尾查回落（内存 miss 场景：另一副本的任务；after 游标语义同
// logsAfterLocked；limit<=0=500 缺省）。
func (st *pgTaskStore) tailLogs(taskID string, after int64, limit int32) ([]*pb.TaskLogEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := st.db.QueryContext(ctx, `SELECT log_id, ts_ms, level, source, message FROM task_logs
		WHERE task_id=$1 AND log_id > $2 ORDER BY log_id ASC LIMIT $3`, taskID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.TaskLogEntry
	for rows.Next() {
		e := &pb.TaskLogEntry{TaskId: taskID}
		if err := rows.Scan(&e.LogId, &e.TsMs, &e.Level, &e.Source, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// logByRequest — 幂等回放查（AppendTaskLog 的 PG 档 logIdem 面）。
func (st *pgTaskStore) logByRequest(requestID string) (*pb.TaskLogEntry, error) {
	if requestID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e := &pb.TaskLogEntry{}
	err := st.db.QueryRowContext(ctx,
		`SELECT task_id, log_id, ts_ms, level, source, message FROM task_logs WHERE request_id=$1`, requestID).
		Scan(&e.TaskId, &e.LogId, &e.TsMs, &e.Level, &e.Source, &e.Message)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// hydrateTasks — 启动回放：按创建时间回放全部任务实体到内存 map。
// R139（第四轮审计 P3-6）：QueryContext+30s 上界（原裸 Query 无 ctx——PG 抖动时
// 启动回放/List 同步无界阻塞；30s=单次同步上界，量级对齐网关 grpc_call_timeout_s，
// 非 07 业务阈值）。List 每调全量拉表本身是 HA P1-c 跨副本可见性的既定设计，不在本批改。
func (st *pgTaskStore) hydrateTasks() ([]*pb.ScanTask, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := st.db.QueryContext(ctx, `SELECT payload, updated_at FROM tasks ORDER BY created_at ASC, task_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.ScanTask
	for rows.Next() {
		var payload []byte
		var updatedAt time.Time
		if err := rows.Scan(&payload, &updatedAt); err != nil {
			return nil, err
		}
		t := &pb.ScanTask{}
		if err := protojson.Unmarshal(payload, t); err != nil {
			return nil, fmt.Errorf("hydrate %s: %w", shortID(payload), err)
		}
		t.UpdatedAt = timestamppb.New(updatedAt) // R115：换入守卫的比较基准
		out = append(out, t)
	}
	return out, rows.Err()
}

func shortID(payload []byte) string {
	if len(payload) > 0 {
		return fmt.Sprintf("(%d bytes)", len(payload))
	}
	return "(empty)"
}

// usageInsertChunk — 单块多值 INSERT 的 SQL 与参数（列序：task_id,seq,tenant,
// "user",session,model,in,out,cacheR,cacheW,reason,total,recorded_at；行 i 的
// 值编号 2+12i..13+12i——每行 12 个新值，编号连续无跳号）。抽纯函数供形状
// 锁定测试（无 PG 可测；首版步长误写 13 被该测试当场抓红，先红后绿实录）。
func usageInsertChunk(taskID string, rows []usageRow) (string, []any) {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO task_usage
		(task_id, seq, tenant, "user", session, model, input_tokens, output_tokens,
		 cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens, recorded_at) VALUES `)
	args := make([]any, 0, len(rows)*13+1)
	args = append(args, taskID)
	for i, r := range rows {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "($1,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			i*12+2, i*12+3, i*12+4, i*12+5, i*12+6, i*12+7, i*12+8,
			i*12+9, i*12+10, i*12+11, i*12+12, i*12+13)
		args = append(args, r.Seq, r.Tenant, r.User, r.Session, r.Model,
			r.In, r.Out, r.CacheR, r.CacheW, r.Reason, r.Total, nullTime(r.RecordedAt))
	}
	return sb.String(), args
}

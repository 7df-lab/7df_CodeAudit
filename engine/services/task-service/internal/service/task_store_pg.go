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
	"database/sql"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"

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
	)`);
	err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id)`);
	err != nil {
		return nil, fmt.Errorf("migrate index: %w", err)
	}
	return &pgTaskStore{db: db}, nil
}

// upsert — 任务实体全量镜像（调用方持 s.mu；错误只记日志不反噬任务流，
// 运行期内存仍是权威，持久化失败属可观测降级而非请求失败）。
func (st *pgTaskStore) upsert(t *pb.ScanTask) error {
	payload, err := protojson.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = st.db.Exec(`INSERT INTO tasks (task_id, project_id, status, created_by, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (task_id) DO UPDATE
		SET status = EXCLUDED.status, payload = EXCLUDED.payload, updated_at = now()`,
		t.GetTaskId(), t.GetProjectId(), t.GetStatus().String(), t.GetCreatedBy(), string(payload))
	return err
}

// hydrateTasks — 启动回放：按创建时间回放全部任务实体到内存 map。
func (st *pgTaskStore) hydrateTasks() ([]*pb.ScanTask, error) {
	rows, err := st.db.Query(`SELECT payload FROM tasks ORDER BY created_at ASC, task_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.ScanTask
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		t := &pb.ScanTask{}
		if err := protojson.Unmarshal(payload, t); err != nil {
			return nil, fmt.Errorf("hydrate %s: %w", shortID(payload), err)
		}
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

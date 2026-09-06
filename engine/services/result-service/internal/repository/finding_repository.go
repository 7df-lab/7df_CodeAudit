package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/codeaudit/services/result-service/internal/model"
	_ "github.com/lib/pq"
)

var ErrNotFound = errors.New("not found")

// 依据: codeaudit_common.proto L920-L935 ResultService
type FindingRepository interface {
	Create(finding *model.Finding) error
	GetByID(id string) (*model.Finding, error)
	Update(finding *model.Finding) error
	Delete(id string) error
	List(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error)
	ListByVerdict(verdict string, lastID string, limit int) ([]*model.Finding, string, error)
	GetByRequestIDAndFindingID(requestID string, findingID string) (*model.Finding, error)
	GetStatsByTaskID(taskID string) (*model.ResultStats, error)
	CreateFeedback(feedback *model.FindingFeedback) error
	GetFeedbackByRequestID(requestID string) (*model.FindingFeedback, error)
	DB() *sql.DB
}

// 依据: 09 §1 PostgreSQL 双库：codeaudit_result
type PostgresFindingRepository struct {
	db *sql.DB
}

func NewPostgresFindingRepository(dsn string) (*PostgresFindingRepository, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %v", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %v", err)
	}

	// 依据: ADR-111 建表用启动时 CREATE TABLE IF NOT EXISTS
	if err := createTables(db); err != nil {
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &PostgresFindingRepository{db: db}, nil
}

func createTables(db *sql.DB) error {
	// 依据: codeaudit_result 数据库 schema
	query := `
	CREATE TABLE IF NOT EXISTS findings (
		id VARCHAR(255) PRIMARY KEY,
		task_id VARCHAR(255) NOT NULL,
		tool_name VARCHAR(100) NOT NULL,
		rule_id VARCHAR(100) NOT NULL,
		severity VARCHAR(20) NOT NULL,
		message TEXT,
		file_path TEXT,
		line_number INTEGER,
		source_raw TEXT,
		verdict VARCHAR(20) NOT NULL DEFAULT 'NOT_REVIEWED',
		reasoning TEXT,
		dedup_group VARCHAR(100),
		matched_findings TEXT,
		is_unique BOOLEAN NOT NULL DEFAULT FALSE,
		ai_fix_suggestion TEXT,
		diff_patch TEXT,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		request_id VARCHAR(255),
		UNIQUE(task_id, tool_name, rule_id, file_path, line_number)
	);
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS reasoning TEXT; -- ADR-135 存量表迁移（幂等）
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS dedup_group VARCHAR(100); -- ADR-142 存量表迁移（幂等）
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS matched_findings TEXT;
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS is_unique BOOLEAN NOT NULL DEFAULT FALSE;
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS ai_fix_suggestion TEXT; -- ADR-183 存量表迁移（幂等）
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS diff_patch TEXT; -- ADR-183 存量表迁移（幂等）
	ALTER TABLE findings ALTER COLUMN verdict TYPE VARCHAR(40); -- ADR-198: ai_verdict 枚举串（AI_VERDICT_NEEDS_MANUAL=23）长于旧 20 位宽，PG 模式下静默截断拒写
	-- ADR-201 存量表迁移（幂等）: code_snippet→source_raw 列正名——ADR-141 起该列实际
	-- 承载 proto UnifiedFinding.source_raw 全量原始 JSON，非代码片段，同名直存消除映射暗语
	DO $$ BEGIN
		IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'findings' AND column_name = 'code_snippet')
		   AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'findings' AND column_name = 'source_raw') THEN
			EXECUTE 'ALTER TABLE findings RENAME COLUMN code_snippet TO source_raw';
		END IF;
	END $$;

	CREATE INDEX IF NOT EXISTS idx_findings_task_id ON findings(task_id);
	CREATE INDEX IF NOT EXISTS idx_findings_verdict ON findings(verdict);
	CREATE INDEX IF NOT EXISTS idx_findings_request_id ON findings(request_id);

	CREATE TABLE IF NOT EXISTS finding_feedback (
		id VARCHAR(255) PRIMARY KEY,
		finding_id VARCHAR(255) NOT NULL REFERENCES findings(id),
		-- ADR-135 修复: INSERT 列含 feedback_type（proto L1249 FeedbackType），
		-- 此前 DDL 缺该列 → PG 模式 CreateFeedback 必然失败
		feedback_type VARCHAR(50) NOT NULL,
		is_correct BOOLEAN NOT NULL DEFAULT FALSE,
		comment TEXT,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		request_id VARCHAR(255) UNIQUE
	);

	CREATE INDEX IF NOT EXISTS idx_feedback_finding_id ON finding_feedback(finding_id);
	CREATE INDEX IF NOT EXISTS idx_feedback_request_id ON finding_feedback(request_id);

	-- 存量表迁移（幂等）: 老库缺 feedback_type/is_correct 列时补齐（ADR-135）
	ALTER TABLE finding_feedback ADD COLUMN IF NOT EXISTS feedback_type VARCHAR(50) NOT NULL DEFAULT 'UNKNOWN';
	ALTER TABLE finding_feedback ADD COLUMN IF NOT EXISTS is_correct BOOLEAN NOT NULL DEFAULT FALSE;
	`

	_, err := db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to create tables: %v", err)
	}

	log.Println("Database tables created/verified successfully")
	return nil
}

func (r *PostgresFindingRepository) Create(finding *model.Finding) error {
	query := `
		INSERT INTO findings (id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	`
	_, err := r.db.Exec(query,
		finding.ID, finding.TaskID, finding.ToolName, finding.RuleID,
		finding.Severity, finding.Message, finding.FilePath, finding.LineNumber,
		finding.SourceRaw, finding.Verdict, finding.DedupGroup, finding.MatchedFindings, finding.IsUnique, finding.AiFixSuggestion, finding.DiffPatch, finding.CreatedAt, finding.UpdatedAt,
		finding.RequestID,
	)
	return err
}

func (r *PostgresFindingRepository) GetByID(id string) (*model.Finding, error) {
	query := `
		SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
		FROM findings WHERE id = $1
	`
	finding := &model.Finding{}
	err := r.db.QueryRow(query, id).Scan(
		&finding.ID, &finding.TaskID, &finding.ToolName, &finding.RuleID,
		&finding.Severity, &finding.Message, &finding.FilePath, &finding.LineNumber,
		&finding.SourceRaw, &finding.Verdict, &finding.DedupGroup, &finding.MatchedFindings, &finding.IsUnique, &finding.AiFixSuggestion, &finding.DiffPatch, &finding.CreatedAt, &finding.UpdatedAt,
		&finding.RequestID,
	)
	if err != nil {
		return nil, err
	}
	return finding, nil
}

func (r *PostgresFindingRepository) Update(finding *model.Finding) error {
	query := `
		UPDATE findings
		SET task_id = $2, tool_name = $3, rule_id = $4, severity = $5, message = $6,
		    file_path = $7, line_number = $8, source_raw = $9, verdict = $10,
		    dedup_group = $11, matched_findings = $12, is_unique = $13,
		    ai_fix_suggestion = $16, diff_patch = $17,
		    updated_at = $14, request_id = $15
		WHERE id = $1
	`
	_, err := r.db.Exec(query,
		finding.ID, finding.TaskID, finding.ToolName, finding.RuleID,
		finding.Severity, finding.Message, finding.FilePath, finding.LineNumber,
		finding.SourceRaw, finding.Verdict, finding.DedupGroup, finding.MatchedFindings, finding.IsUnique, finding.UpdatedAt, finding.RequestID,
		finding.AiFixSuggestion, finding.DiffPatch,
	)
	return err
}

func (r *PostgresFindingRepository) Delete(id string) error {
	query := `DELETE FROM findings WHERE id = $1`
	_, err := r.db.Exec(query, id)
	return err
}

// 依据: 03 §5 cursor 分页语义
func (r *PostgresFindingRepository) List(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error) {
	var query string
	var args []interface{}

	if lastID == "" {
		// First page
		if taskID != "" && verdict != "" {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				WHERE task_id = $1 AND verdict = $2
				ORDER BY id
				LIMIT $3
			`
			args = []interface{}{taskID, verdict, limit + 1}
		} else if taskID != "" {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				WHERE task_id = $1
				ORDER BY id
				LIMIT $2
			`
			args = []interface{}{taskID, limit + 1}
		} else if verdict != "" {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				WHERE verdict = $1
				ORDER BY id
				LIMIT $2
			`
			args = []interface{}{verdict, limit + 1}
		} else {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				ORDER BY id
				LIMIT $1
			`
			args = []interface{}{limit + 1}
		}
	} else {
		// Subsequent pages
		if taskID != "" && verdict != "" {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				WHERE id > $1 AND task_id = $2 AND verdict = $3
				ORDER BY id
				LIMIT $4
			`
			args = []interface{}{lastID, taskID, verdict, limit + 1}
		} else if taskID != "" {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				WHERE id > $1 AND task_id = $2
				ORDER BY id
				LIMIT $3
			`
			args = []interface{}{lastID, taskID, limit + 1}
		} else if verdict != "" {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				WHERE id > $1 AND verdict = $2
				ORDER BY id
				LIMIT $3
			`
			args = []interface{}{lastID, verdict, limit + 1}
		} else {
			query = `
				SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
				FROM findings
				WHERE id > $1
				ORDER BY id
				LIMIT $2
			`
			args = []interface{}{lastID, limit + 1}
		}
	}

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var findings []*model.Finding
	for rows.Next() {
		f := &model.Finding{}
		err := rows.Scan(
			&f.ID, &f.TaskID, &f.ToolName, &f.RuleID,
			&f.Severity, &f.Message, &f.FilePath, &f.LineNumber,
			&f.SourceRaw, &f.Verdict, &f.DedupGroup, &f.MatchedFindings, &f.IsUnique, &f.AiFixSuggestion, &f.DiffPatch, &f.CreatedAt, &f.UpdatedAt,
			&f.RequestID,
		)
		if err != nil {
			return nil, "", err
		}
		findings = append(findings, f)
	}

	// Determine if there are more pages
	nextCursor := ""
	if len(findings) > limit {
		// Remove the extra item used for pagination
		findings = findings[:limit]
		nextCursor = findings[len(findings)-1].ID
	}

	return findings, nextCursor, nil
}

func (r *PostgresFindingRepository) ListByVerdict(verdict string, lastID string, limit int) ([]*model.Finding, string, error) {
	return r.List(lastID, limit, "", verdict)
}

func (r *PostgresFindingRepository) GetByRequestIDAndFindingID(requestID string, findingID string) (*model.Finding, error) {
	query := `
		SELECT id, task_id, tool_name, rule_id, severity, message, file_path, line_number, source_raw, verdict, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id
		FROM findings
		WHERE request_id = $1 AND id = $2
	`
	finding := &model.Finding{}
	err := r.db.QueryRow(query, requestID, findingID).Scan(
		&finding.ID, &finding.TaskID, &finding.ToolName, &finding.RuleID,
		&finding.Severity, &finding.Message, &finding.FilePath, &finding.LineNumber,
		&finding.SourceRaw, &finding.Verdict, &finding.DedupGroup, &finding.MatchedFindings, &finding.IsUnique, &finding.AiFixSuggestion, &finding.DiffPatch, &finding.CreatedAt, &finding.UpdatedAt,
		&finding.RequestID,
	)
	if err != nil {
		return nil, err
	}
	return finding, nil
}

func (r *PostgresFindingRepository) GetStatsByTaskID(taskID string) (*model.ResultStats, error) {
	query := `
		SELECT
			COUNT(*) as total,
			COUNT(CASE WHEN verdict = 'AI_VERDICT_TRUE_POSITIVE' THEN 1 END) as true_positives,
			COUNT(CASE WHEN verdict = 'AI_VERDICT_FALSE_POSITIVE' THEN 1 END) as false_positives
		FROM findings
		WHERE task_id = $1
	`
	stats := &model.ResultStats{ // 依据: proto L1242-L1244 ResultStats by_verdict/by_severity/by_cwe
		ByVerdict:  map[string]int32{},
		BySeverity: map[string]int32{},
		ByCwe:      map[string]int32{},
	}
	var trueP, falseP int
	err := r.db.QueryRow(query, taskID).Scan(
		&stats.TotalFindings, &trueP, &falseP,
	)
	if err != nil {
		return nil, err
	}
	stats.ByVerdict["AI_VERDICT_TRUE_POSITIVE"] = int32(trueP)
	stats.ByVerdict["AI_VERDICT_FALSE_POSITIVE"] = int32(falseP)
	stats.ByVerdict["AI_VERDICT_NOT_REVIEWED"] = int32(stats.TotalFindings - trueP - falseP)
	return stats, nil
}

func (r *PostgresFindingRepository) CreateFeedback(feedback *model.FindingFeedback) error {
	query := `
		INSERT INTO finding_feedback (id, finding_id, feedback_type, comment, created_at, request_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	_, err := r.db.Exec(query,
		feedback.ID, feedback.FindingID, feedback.FeedbackType, // proto L1250 feedback_type
		feedback.Comment, feedback.CreatedAt, feedback.RequestID,
	)
	return err
}

func (r *PostgresFindingRepository) GetFeedbackByRequestID(requestID string) (*model.FindingFeedback, error) {
	query := `
		SELECT id, finding_id, feedback_type, comment, created_at, request_id
		FROM finding_feedback
		WHERE request_id = $1
	`
	feedback := &model.FindingFeedback{}
	err := r.db.QueryRow(query, requestID).Scan(
		&feedback.ID, &feedback.FindingID, &feedback.FeedbackType,
		&feedback.Comment, &feedback.CreatedAt, &feedback.RequestID,
	)
	if err != nil {
		return nil, err
	}
	return feedback, nil
}

func (r *PostgresFindingRepository) DB() *sql.DB {
	return r.db
}

package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

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
	// direction/tier 过滤语义：空串=不过滤；否则按 pb 枚举名字面量精确匹配
	//（'VERDICT_DIRECTION_UNSPECIFIED' 字面量=过滤"未判定"——真实可查态）
	List(lastID string, limit int, taskID string, direction string, tier string) ([]*model.Finding, string, error)
	// Count — 三过滤轴（task/direction/tier）与 List 同语义的行数：游标分页本体
	// 不携带总数，PaginationResponse.total（proto L264 "可选：总数"）的服务端口径
	Count(taskID string, direction string, tier string) (int, error)
	ListByVerdict(direction string, tier string, lastID string, limit int) ([]*model.Finding, string, error)
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
		verdict_direction VARCHAR(30) NOT NULL DEFAULT 'VERDICT_DIRECTION_UNSPECIFIED',
		verdict_tier VARCHAR(30) NOT NULL DEFAULT 'VERDICT_TIER_UNSPECIFIED',
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
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS inherited_from_task_id VARCHAR(255); -- ADR-225 增量扫描继承标记（幂等）
	-- 2026-10-09 判定两轴迁移第一步（幂等）：存量库 CREATE TABLE IF NOT EXISTS 被跳过，
	-- 新列必须独立 ALTER 补齐（同 ADR-135/142/183/225 迁移模式；122 实测判例——
	-- 只写在 CREATE TABLE 里时存量库 CREATE INDEX 引用新列即整脚本崩）
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS verdict_direction VARCHAR(30) NOT NULL DEFAULT 'VERDICT_DIRECTION_UNSPECIFIED';
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS verdict_tier VARCHAR(30) NOT NULL DEFAULT 'VERDICT_TIER_UNSPECIFIED';
	-- v0.1.2 持久化补齐（幂等；2026-10-10 GUI 遍历判例）：title/cwe_id 两字段
	-- 此前模型层有、表无列——DSH submit_findings 十项必填含 cwe_id、SAST adapter
	-- 亦填，但落库即丢（INSERT/SELECT 列清单不含），读回恒空串（发现列只剩按钮、
	-- CWE 列恒 —）。存量不回填（描述文本里的 CWE 格式不定，启发式猜测违反诚实
	-- 原则），COALESCE 归零显示 —。
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS title TEXT;
	ALTER TABLE findings ADD COLUMN IF NOT EXISTS cwe_id VARCHAR(32) NOT NULL DEFAULT '';
	-- 2026-10-09 第一性原理重构迁移（幂等）：七值 verdict 单列 → 两轴
	-- verdict_direction/verdict_tier。旧值回填映射（AI_VERDICT_CONFIRMED 为历史写径
	-- 遗留别名，report 桶曾按 TP 计——照实归 (TRUE, DEFINITE)）；UNSPECIFIED/UNCERTAIN/
	-- NEEDS_MANUAL/NOT_REVIEWED → 保持缺省（未判定）。回填+删列包在列存在性守卫内，
	-- 二次启动列已不存在即整块 no-op（裸 UPDATE 引用已删列会报错）。
	DO $$ BEGIN
		IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'findings' AND column_name = 'verdict') THEN
			UPDATE findings SET verdict_direction = 'VERDICT_DIRECTION_TRUE',  verdict_tier = 'VERDICT_TIER_DEFINITE' WHERE verdict IN ('AI_VERDICT_TRUE_POSITIVE', 'AI_VERDICT_CONFIRMED');
			UPDATE findings SET verdict_direction = 'VERDICT_DIRECTION_FALSE', verdict_tier = 'VERDICT_TIER_DEFINITE' WHERE verdict = 'AI_VERDICT_FALSE_POSITIVE';
			UPDATE findings SET verdict_direction = 'VERDICT_DIRECTION_TRUE',  verdict_tier = 'VERDICT_TIER_LIKELY'   WHERE verdict = 'AI_VERDICT_LIKELY_TRUE';
			UPDATE findings SET verdict_direction = 'VERDICT_DIRECTION_FALSE', verdict_tier = 'VERDICT_TIER_LIKELY'   WHERE verdict = 'AI_VERDICT_LIKELY_FALSE';
			ALTER TABLE findings DROP COLUMN verdict;
		END IF;
	END $$;
	-- ADR-201 存量表迁移（幂等）: code_snippet→source_raw 列正名——ADR-141 起该列实际
	-- 承载 proto UnifiedFinding.source_raw 全量原始 JSON，非代码片段，同名直存消除映射暗语
	DO $$ BEGIN
		IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'findings' AND column_name = 'code_snippet')
		   AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'findings' AND column_name = 'source_raw') THEN
			EXECUTE 'ALTER TABLE findings RENAME COLUMN code_snippet TO source_raw';
		END IF;
	END $$;

	CREATE INDEX IF NOT EXISTS idx_findings_task_id ON findings(task_id);
	CREATE INDEX IF NOT EXISTS idx_findings_direction ON findings(verdict_direction);
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
		INSERT INTO findings (id, task_id, tool_name, rule_id, cwe_id, title, severity, message, file_path, line_number, source_raw, verdict_direction, verdict_tier, reasoning, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id, inherited_from_task_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		ON CONFLICT (id) DO NOTHING
	`
	// R135（缺损批 A6 露牙）：跨纪元重报同内容发现（内容稳定 ID）=幂等 no-op——
	// 原裸 INSERT 在 epoch-2 重报时主键碰撞逐条失败（22×duplicate key 实证）。
	_, err := r.db.Exec(query,
		finding.ID, finding.TaskID, finding.ToolName, finding.RuleID, finding.CWE, finding.Title,
		finding.Severity, finding.Message, finding.FilePath, finding.LineNumber,
		finding.SourceRaw, finding.VerdictDirection, finding.VerdictTier, finding.Reasoning, finding.DedupGroup, finding.MatchedFindings, finding.IsUnique, finding.AiFixSuggestion, finding.DiffPatch, finding.CreatedAt, finding.UpdatedAt,
		finding.RequestID, finding.InheritedFrom,
	)
	return err
}

// findingColumns — SELECT 列清单+COALESCE 归一（verdict 两轴缺省 NOT NULL，无需 COALESCE；
// reasoning/inherited_from_task_id/title/cwe_id 老库可空列保持归零）
const findingColumns = `id, task_id, tool_name, rule_id, COALESCE(cwe_id, '') AS cwe_id, COALESCE(title, '') AS title, severity, message, file_path, line_number, source_raw, verdict_direction, verdict_tier, COALESCE(reasoning, '') AS reasoning, dedup_group, matched_findings, is_unique, ai_fix_suggestion, diff_patch, created_at, updated_at, request_id, COALESCE(inherited_from_task_id, '') AS inherited_from_task_id`

func scanFinding(scanner interface {
	Scan(dest ...interface{}) error
}) (*model.Finding, error) {
	f := &model.Finding{}
	err := scanner.Scan(
		&f.ID, &f.TaskID, &f.ToolName, &f.RuleID, &f.CWE, &f.Title,
		&f.Severity, &f.Message, &f.FilePath, &f.LineNumber,
		&f.SourceRaw, &f.VerdictDirection, &f.VerdictTier, &f.Reasoning, &f.DedupGroup, &f.MatchedFindings, &f.IsUnique, &f.AiFixSuggestion, &f.DiffPatch, &f.CreatedAt, &f.UpdatedAt,
		&f.RequestID, &f.InheritedFrom,
	)
	return f, err
}

func (r *PostgresFindingRepository) GetByID(id string) (*model.Finding, error) {
	query := `SELECT ` + findingColumns + ` FROM findings WHERE id = $1`
	return scanFinding(r.db.QueryRow(query, id))
}

func (r *PostgresFindingRepository) Update(finding *model.Finding) error {
	query := `
		UPDATE findings
		SET task_id = $2, tool_name = $3, rule_id = $4, severity = $5, message = $6,
		    file_path = $7, line_number = $8, source_raw = $9, verdict_direction = $10, verdict_tier = $11, reasoning = $19,
		    dedup_group = $12, matched_findings = $13, is_unique = $14,
		    ai_fix_suggestion = $16, diff_patch = $17,
		    updated_at = $15, request_id = $18, cwe_id = $20, title = $21
		WHERE id = $1
	`
	_, err := r.db.Exec(query,
		finding.ID, finding.TaskID, finding.ToolName, finding.RuleID,
		finding.Severity, finding.Message, finding.FilePath, finding.LineNumber,
		finding.SourceRaw, finding.VerdictDirection, finding.VerdictTier, finding.DedupGroup, finding.MatchedFindings, finding.IsUnique, finding.UpdatedAt, finding.RequestID,
		finding.AiFixSuggestion, finding.DiffPatch, finding.Reasoning,
		finding.CWE, finding.Title,
	)
	return err
}

func (r *PostgresFindingRepository) Delete(id string) error {
	query := `DELETE FROM findings WHERE id = $1`
	_, err := r.db.Exec(query, id)
	return err
}

// verdictFilterWhere — 三过滤轴 WHERE 片段（List 与 Count 单源；空串=不过滤，
// 字面量=精确匹配）。游标键（id > lastID）是分页轴非过滤轴，不在此列。
func verdictFilterWhere(taskID string, direction string, tier string) ([]string, []interface{}) {
	where := []string{}
	args := []interface{}{}
	if taskID != "" {
		args = append(args, taskID)
		where = append(where, fmt.Sprintf("task_id = $%d", len(args)))
	}
	if direction != "" {
		args = append(args, direction)
		where = append(where, fmt.Sprintf("verdict_direction = $%d", len(args)))
	}
	if tier != "" {
		args = append(args, tier)
		where = append(where, fmt.Sprintf("verdict_tier = $%d", len(args)))
	}
	return where, args
}

// 依据: 03 §5 cursor 分页语义。verdict 两轴过滤：空串=不过滤，
// 字面量=精确匹配（含 'VERDICT_DIRECTION_UNSPECIFIED' 过滤未判定态）。
// 原 8 分支拷贝式 SQL 收敛为动态 WHERE 组合（同语义同序：ORDER BY id + 游标键 id > lastID）。
func (r *PostgresFindingRepository) List(lastID string, limit int, taskID string, direction string, tier string) ([]*model.Finding, string, error) {
	where := []string{}
	args := []interface{}{}
	if lastID != "" {
		args = append(args, lastID)
		where = append(where, fmt.Sprintf("id > $%d", len(args)))
	}
	filterWhere, filterArgs := verdictFilterWhere(taskID, direction, tier)
	where = append(where, filterWhere...)
	args = append(args, filterArgs...)
	args = append(args, limit+1)

	query := `SELECT ` + findingColumns + ` FROM findings`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += fmt.Sprintf(` ORDER BY id LIMIT $%d`, len(args))

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var findings []*model.Finding
	for rows.Next() {
		f, serr := scanFinding(rows)
		if serr != nil {
			return nil, "", serr
		}
		findings = append(findings, f)
	}
	// ADR-212: 迭代中途网络错误此前被静默吞掉，截断页冒充完整页
	if err := rows.Err(); err != nil {
		return nil, "", err
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

func (r *PostgresFindingRepository) ListByVerdict(direction string, tier string, lastID string, limit int) ([]*model.Finding, string, error) {
	return r.List(lastID, limit, "", direction, tier)
}

// Count — 三过滤轴与 List 同语义的 COUNT（web 共 N 条=B3-1 服务端全量口径；
// 2026-10-10 GUI 遍历经验：total 恒 0 使「共 0 条」与 6 行实体同屏矛盾）
func (r *PostgresFindingRepository) Count(taskID string, direction string, tier string) (int, error) {
	where, args := verdictFilterWhere(taskID, direction, tier)
	query := `SELECT COUNT(*) FROM findings`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	var n int
	if err := r.db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (r *PostgresFindingRepository) GetByRequestIDAndFindingID(requestID string, findingID string) (*model.Finding, error) {
	query := `SELECT ` + findingColumns + ` FROM findings WHERE request_id = $1 AND id = $2`
	return scanFinding(r.db.QueryRow(query, requestID, findingID))
}

func (r *PostgresFindingRepository) GetStatsByTaskID(taskID string) (*model.ResultStats, error) {
	// 桶口径单源=libs/common-go/verdict：确认为真/误报=方向已判
	// 且确定档（SQL 内联同条件——IsConfirmedTrue/IsConfirmedFalse 的存储层投影）
	query := `
		SELECT
			COUNT(*) as total,
			COUNT(CASE WHEN verdict_direction = 'VERDICT_DIRECTION_TRUE' AND verdict_tier = 'VERDICT_TIER_DEFINITE' THEN 1 END) as true_positives,
			COUNT(CASE WHEN verdict_direction = 'VERDICT_DIRECTION_FALSE' AND verdict_tier = 'VERDICT_TIER_DEFINITE' THEN 1 END) as false_positives
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
	stats.ByVerdict["TRUE_DEFINITE"] = int32(trueP)
	stats.ByVerdict["FALSE_DEFINITE"] = int32(falseP)
	stats.ByVerdict["NEEDS_REVIEW"] = int32(stats.TotalFindings - trueP - falseP)
	// R45: by_severity/by_cwe 与内存实现口径对齐（CWE 由 rule_id 承载，表无独立 cwe 列）
	rows, err := r.db.Query(`SELECT severity, COUNT(*) FROM findings WHERE task_id = $1 GROUP BY severity`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sev string
		var n int
		if err := rows.Scan(&sev, &n); err != nil {
			return nil, err
		}
		stats.BySeverity[sev] = int32(n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cweRows, err := r.db.Query(`SELECT rule_id, COUNT(*) FROM findings WHERE task_id = $1 GROUP BY rule_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer cweRows.Close()
	for cweRows.Next() {
		var cwe string
		var n int
		if err := cweRows.Scan(&cwe, &n); err != nil {
			return nil, err
		}
		stats.ByCwe[cwe] = int32(n)
	}
	return stats, cweRows.Err()
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

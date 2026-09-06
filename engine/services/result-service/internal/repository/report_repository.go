package repository

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/codeaudit/services/result-service/internal/model"
)

// 依据: codeaudit_common.proto L942-L948 ReportService
type ReportRepository interface {
	CreateReport(report *model.Report) error
	GetReportByID(id string) (*model.Report, error)
	GetReportByRequestID(requestID string) (*model.Report, error)
	ListReports(lastID string, limit int, taskID string) ([]*model.Report, string, error)
	ListTemplates(limit int) ([]*model.ReportTemplate, error)
	GetTemplateByID(id string) (*model.ReportTemplate, error)
	UpdateReport(report *model.Report) error // ADR-199: 归档 URL 回写
}

// 依据: 09 §1 PostgreSQL 双库：codeaudit_result
type PostgresReportRepository struct {
	db *sql.DB
}

func NewPostgresReportRepository(db *sql.DB) (*PostgresReportRepository, error) {
	r := &PostgresReportRepository{db: db}
	// ADR-198: 构造时建表（与 finding_repository 同口径）——此前 createTables 从未
	// 被调用，PG 模式下 reports/report_templates 缺失，GenerateReport 必失败。
	if err := r.createTables(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *PostgresReportRepository) createTables() error {
	// 依据: codeaudit_result 数据库 schema
	query := `
	CREATE TABLE IF NOT EXISTS reports (
		id VARCHAR(255) PRIMARY KEY,
		task_id VARCHAR(255) NOT NULL,
		template_id VARCHAR(100) NOT NULL,  -- 依据: L1258 template_id
		format VARCHAR(50) NOT NULL,         -- 依据: L1259 ReportFormat
		url TEXT,                            -- 依据: L1263 url
		status VARCHAR(20) NOT NULL DEFAULT 'PENDING',
		content TEXT,
		error_message TEXT,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		completed_at TIMESTAMP WITH TIME ZONE,
		request_id VARCHAR(255) UNIQUE
	);

	CREATE INDEX IF NOT EXISTS idx_reports_task_id ON reports(task_id);
	CREATE INDEX IF NOT EXISTS idx_reports_status ON reports(status);
	CREATE INDEX IF NOT EXISTS idx_reports_request_id ON reports(request_id);

	CREATE TABLE IF NOT EXISTS report_templates (
		id VARCHAR(255) PRIMARY KEY,
		name VARCHAR(100) NOT NULL UNIQUE,
		description TEXT,
		content TEXT NOT NULL,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
	);

	-- Insert default templates if not exist
	INSERT INTO report_templates (id, name, description, content)
	VALUES
		('tpl_default', 'Default Report', 'Standard audit report template', '{"sections": ["summary", "findings", "recommendations"]}'),
		('tpl_executive', 'Executive Summary', 'High-level summary for management', '{"sections": ["executive_summary", "risk_overview", "recommendations"]}'),
		('tpl_detailed', 'Detailed Technical', 'Comprehensive technical report', '{"sections": ["summary", "methodology", "findings", "code_examples", "remediation", "appendix"]}')
	ON CONFLICT (id) DO NOTHING;
	`

	_, err := r.db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to create report tables: %v", err)
	}

	log.Println("Report tables created/verified successfully")
	return nil
}

func (r *PostgresReportRepository) CreateReport(report *model.Report) error {
	query := `
		INSERT INTO reports (id, task_id, template_id, format, url, status, content, error_message, created_at, updated_at, completed_at, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`
	_, err := r.db.Exec(query,
		report.ID, report.TaskID, report.Template, report.Format, report.Url,
		report.Status, report.Content, report.ErrorMessage, report.CreatedAt, report.UpdatedAt,
		report.CompletedAt, report.RequestID,
	)
	return err
}

// UpdateReport — ADR-199: 归档 URL 等字段的回写（仅更新 url/updated_at，内容不动）。
func (r *PostgresReportRepository) UpdateReport(report *model.Report) error {
	query := `UPDATE reports SET url = $2, updated_at = NOW() WHERE id = $1`
	_, err := r.db.Exec(query, report.ID, report.Url)
	return err
}

func (r *PostgresReportRepository) GetReportByID(id string) (*model.Report, error) {
	query := `
		SELECT id, task_id, template_id, format, url, status, content, error_message, created_at, updated_at, completed_at, request_id
		FROM reports WHERE id = $1
	`
	report := &model.Report{}
	err := r.db.QueryRow(query, id).Scan(
		&report.ID, &report.TaskID, &report.Template, &report.Format, &report.Url,
		&report.Status, &report.Content, &report.ErrorMessage, &report.CreatedAt, &report.UpdatedAt,
		&report.CompletedAt, &report.RequestID,
	)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func (r *PostgresReportRepository) GetReportByRequestID(requestID string) (*model.Report, error) {
	query := `
		SELECT id, task_id, template_id, format, url, status, content, error_message, created_at, updated_at, completed_at, request_id
		FROM reports WHERE request_id = $1
	`
	report := &model.Report{}
	err := r.db.QueryRow(query, requestID).Scan(
		&report.ID, &report.TaskID, &report.Template, &report.Format, &report.Url,
		&report.Status, &report.Content, &report.ErrorMessage, &report.CreatedAt, &report.UpdatedAt,
		&report.CompletedAt, &report.RequestID,
	)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func (r *PostgresReportRepository) ListReports(lastID string, limit int, taskID string) ([]*model.Report, string, error) {
	var query string
	var args []interface{}

	if lastID == "" {
		if taskID != "" {
			// ADR-164: 报告中心"最新生成优先"（ADR-142/149 同口径）——DESC + lastID 向更旧翻页
			query = `
				SELECT id, task_id, template_id, format, url, status, content, error_message, created_at, updated_at, completed_at, request_id
				FROM reports
				WHERE task_id = $1
				ORDER BY id DESC
				LIMIT $2
			`
			args = []interface{}{taskID, limit + 1}
		} else {
			query = `
				SELECT id, task_id, template_id, format, url, status, content, error_message, created_at, updated_at, completed_at, request_id
				FROM reports
				ORDER BY id DESC
				LIMIT $1
			`
			args = []interface{}{limit + 1}
		}
	} else {
		if taskID != "" {
			query = `
				SELECT id, task_id, template_id, format, url, status, content, error_message, created_at, updated_at, completed_at, request_id
				FROM reports
				WHERE id < $1 AND task_id = $2
				ORDER BY id DESC
				LIMIT $3
			`
			args = []interface{}{lastID, taskID, limit + 1}
		} else {
			query = `
				SELECT id, task_id, template_id, format, url, status, content, error_message, created_at, updated_at, completed_at, request_id
				FROM reports
				WHERE id < $1
				ORDER BY id DESC
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

	var reports []*model.Report
	for rows.Next() {
		r := &model.Report{}
		err := rows.Scan(
			&r.ID, &r.TaskID, &r.Template, &r.Format, &r.Url, &r.Status,
			&r.Content, &r.ErrorMessage, &r.CreatedAt, &r.UpdatedAt,
			&r.CompletedAt, &r.RequestID,
		)
		if err != nil {
			return nil, "", err
		}
		reports = append(reports, r)
	}

	nextCursor := ""
	if len(reports) > limit {
		reports = reports[:limit]
		nextCursor = reports[len(reports)-1].ID
	}

	return reports, nextCursor, nil
}

func (r *PostgresReportRepository) ListTemplates(limit int) ([]*model.ReportTemplate, error) {
	// 依据: codeaudit_common.proto L1268 ReportTemplate
	query := `
		SELECT id, name, description
		FROM report_templates
		ORDER BY name
		LIMIT $1
	`

	rows, err := r.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []*model.ReportTemplate
	for rows.Next() {
		t := &model.ReportTemplate{}
		err := rows.Scan(
			&t.ID, &t.Name, &t.Description,
		)
		if err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}

	return templates, nil
}

func (r *PostgresReportRepository) GetTemplateByID(id string) (*model.ReportTemplate, error) {
	// 依据: codeaudit_common.proto L1268 ReportTemplate
	query := `
		SELECT id, name, description
		FROM report_templates WHERE id = $1
	`
	template := &model.ReportTemplate{}
	err := r.db.QueryRow(query, id).Scan(
		&template.ID, &template.Name, &template.Description,
	)
	if err != nil {
		return nil, err
	}
	return template, nil
}

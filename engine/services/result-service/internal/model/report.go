package model

import "time"

// 依据: codeaudit_common.proto L942-L948 ReportService
type Report struct {
	ID           string     `json:"id"`
	TaskID       string     `json:"task_id"`
	Template     string     `json:"template_id"` // 依据: L1258 template_id
	Format       string     `json:"format"`      // 依据: L1259 ReportFormat
	Url          string     `json:"url"`         // 依据: L1263 url
	Status       string     `json:"status"`
	Content      string     `json:"content"`
	ErrorMessage string     `json:"error_message"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	RequestID    string     `json:"request_id"` // 幂等键 - 依据: 03 §2
}

// 依据: codeaudit_common.proto L1268 ReportTemplate
type ReportTemplate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

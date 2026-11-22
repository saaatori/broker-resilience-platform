package model

import "time"

// TestReport 报告元数据表（总体设计表 24）：归档附件登记（协议 §2.5：artifacts 相对路径）+ M4 报告主文件。
// day35 每 artifact 一行；M4 生成的 HTML 报告主行同表追加。
type TestReport struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	TaskID      int64     `gorm:"not null;index:idx_report_task" json:"task_id"`                // 表 25：idx(task_id)
	ExecutionID string    `gorm:"type:uuid;not null;index:idx_report_exec" json:"execution_id"` // 表 25：idx(execution_id)
	FilePath    string    `gorm:"size:512;not null" json:"file_path"`
	GeneratedAt time.Time `gorm:"not null" json:"generated_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestReport) TableName() string { return "test_report" }

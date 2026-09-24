package model

import "time"

// One pending delivery per document coalesces duplicate enqueue requests.
type DocumentTaskOutbox struct {
	DocumentID  uint       `gorm:"primaryKey;autoIncrement:false"`
	UserID      uint       `gorm:"not null"`
	Attempts    int        `gorm:"not null;default:0"`
	AvailableAt time.Time  `gorm:"precision:6;not null;index:idx_document_task_available,priority:1"`
	LeaseToken  string     `gorm:"type:varchar(32);not null;default:''"`
	LeaseUntil  *time.Time `gorm:"precision:6;index:idx_document_task_available,priority:2"`
	LastError   string     `gorm:"type:varchar(1000);not null;default:''"`
	CreatedAt   time.Time  `gorm:"precision:6"`
	UpdatedAt   time.Time  `gorm:"precision:6"`
}

func (DocumentTaskOutbox) TableName() string { return "document_task_outbox" }

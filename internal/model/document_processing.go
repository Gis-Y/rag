package model

import "time"

// DocumentProcessingState publishes a complete immutable generation at a time.
// A failed pending generation never replaces the previous active generation.
type DocumentProcessingState struct {
	DocumentID     uint   `gorm:"primaryKey;autoIncrement:false"`
	UserID         uint   `gorm:"not null"`
	FileMD5        string `gorm:"type:varchar(32);not null"`
	ActiveVersion  string `gorm:"type:varchar(64);not null;default:''"`
	PendingVersion string `gorm:"type:varchar(64);not null;default:''"`
	Status         string `gorm:"type:varchar(24);not null"`
	Error          string `gorm:"type:text"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (DocumentProcessingState) TableName() string { return "document_processing_states" }

// DocumentChunk stores both parents and children without embedding the parents.
type DocumentChunk struct {
	DocumentID uint        `gorm:"primaryKey;autoIncrement:false"`
	Version    string      `gorm:"type:varchar(64);primaryKey"`
	ChunkID    string      `gorm:"type:varchar(128);primaryKey"`
	ParentID   string      `gorm:"type:varchar(128);not null;default:''"`
	UserID     uint        `gorm:"not null"`
	IsParent   bool        `gorm:"not null"`
	Data       ParsedChunk `gorm:"serializer:json;type:json;not null"`
	CreatedAt  time.Time
}

func (DocumentChunk) TableName() string { return "document_chunks" }

type ParentRef struct {
	DocumentID uint
	Version    string
	ParentID   string
}

// Package model 定义了与数据库表对应的 Go 结构体。
package model

import "time"

// FileUpload 定义了 file_upload 表的 ORM 模型。
// 它记录了每个上传文件的元数据和状态。
type FileUpload struct {
	ID        uint       `gorm:"primaryKey;autoIncrement"`
	FileMD5   string     `gorm:"type:varchar(32);not null"`
	FileName  string     `gorm:"type:varchar(255);not null"`
	TotalSize int64      `gorm:"not null"`
	Status    int        `gorm:"type:tinyint;not null;default:0"` // 0: uploading, 1: completed, 2: failed, 3: deleting, 4: merging
	UserID    uint       `gorm:"not null"`
	OrgTag    string     `gorm:"type:varchar(50)"`
	IsPublic  bool       `gorm:"not null;default:false"`
	CreatedAt time.Time  `gorm:"autoCreateTime"`
	MergedAt  *time.Time `gorm:"default:null"`
	// The completed token identifies an immutable raw object; expired attempts never share that key.
	MergeToken     string     `gorm:"type:varchar(32);not null;default:''"`
	MergeExpiresAt *time.Time `gorm:"default:null"`
}

// TableName 指定了此模型在数据库中对应的表名。
func (FileUpload) TableName() string {
	return "file_upload"
}

// ChunkInfo 对应于数据库中的 'chunk_info' 表。
// 它记录了每个文件分块的详细信息。
type ChunkInfo struct {
	ID          uint   `gorm:"primaryKey;autoIncrement"`
	DocumentID  uint   `gorm:"not null;index"`
	UserID      uint   `gorm:"not null"`
	FileMD5     string `gorm:"type:varchar(32);not null"`
	ChunkIndex  int    `gorm:"not null"`
	ChunkMD5    string `gorm:"type:varchar(32);not null"`
	StoragePath string `gorm:"type:varchar(255);not null"`
}

// TableName 指定了此模型在数据库中对应的表名。
func (ChunkInfo) TableName() string {
	return "chunk_info"
}

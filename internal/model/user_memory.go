package model

import "time"

// UserMemory stores only user-confirmed, scoped facts and preferences.
type UserMemory struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	UserID    uint       `gorm:"not null;uniqueIndex:uk_user_memory,priority:1" json:"-"`
	Scope     string     `gorm:"size:64;not null;uniqueIndex:uk_user_memory,priority:2" json:"scope"`
	Kind      string     `gorm:"size:16;not null" json:"kind"`
	Key       string     `gorm:"size:64;not null;uniqueIndex:uk_user_memory,priority:3" json:"key"`
	Content   string     `gorm:"type:text;not null" json:"content"`
	Keywords  []string   `gorm:"type:json;serializer:json;not null" json:"keywords"`
	Version   uint64     `gorm:"not null;default:1" json:"version"`
	ExpiresAt *time.Time `gorm:"precision:6;index" json:"expires_at"`
	CreatedAt time.Time  `gorm:"precision:6;autoCreateTime" json:"created_at"`
	UpdatedAt time.Time  `gorm:"precision:6;autoUpdateTime" json:"updated_at"`
}

func (UserMemory) TableName() string { return "user_memories" }

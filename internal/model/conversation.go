// Package model 包含了应用的数据模型定义。
package model

import "time"

// ChatMessage 代表对外展示的单条对话消息。
type ChatMessage struct {
	Role      string           `json:"role"` // "user" 或 "assistant"
	Content   string           `json:"content"`
	Timestamp time.Time        `json:"timestamp"`
	Sources   []SourceCitation `json:"sources,omitempty"`
}

// Conversation 代表一次单独的问答交互。
type Conversation struct {
	ID             uint             `gorm:"primaryKey" json:"id"`
	UserID         uint             `gorm:"index;not null" json:"user_id"`
	ConversationID string           `gorm:"size:64;not null;uniqueIndex:uk_conversation_turn,priority:1;uniqueIndex:uk_conversation_request,priority:1" json:"conversation_id"`
	TurnID         string           `gorm:"size:64;not null;uniqueIndex:uk_conversation_request,priority:2" json:"turn_id"`
	TurnNo         uint64           `gorm:"not null;uniqueIndex:uk_conversation_turn,priority:2" json:"turn_no"`
	Question       string           `gorm:"type:longtext;not null" json:"question"`
	Answer         string           `gorm:"type:longtext;not null" json:"answer"`
	Sources        []SourceCitation `gorm:"serializer:json;type:json" json:"sources,omitempty"`
	CreatedAt      time.Time        `gorm:"precision:6;autoCreateTime" json:"created_at"`
}

// ConversationState is the versioned context of one user's ongoing conversation.
// Raw turns are immutable; summaries only advance SummaryUntil.
type ConversationState struct {
	ID           string    `gorm:"size:64;primaryKey" json:"conversation_id"`
	UserID       uint      `gorm:"uniqueIndex;not null" json:"user_id"`
	Version      uint64    `gorm:"not null;default:0" json:"version"`
	LastTurn     uint64    `gorm:"not null;default:0" json:"last_turn"`
	Summary      string    `gorm:"type:longtext;not null" json:"summary"`
	SummaryUntil uint64    `gorm:"not null;default:0" json:"summary_until"`
	ContextAfter uint64    `gorm:"not null;default:0" json:"context_after"`
	CreatedAt    time.Time `gorm:"precision:6;autoCreateTime" json:"created_at"`
	UpdatedAt    time.Time `gorm:"precision:6;autoUpdateTime" json:"updated_at"`
}

func (ConversationState) TableName() string { return "conversation_states" }

func (Conversation) TableName() string {
	return "conversations"
}

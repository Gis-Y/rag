// Package service 包含了应用的业务逻辑层。
package service

import (
	"context"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
)

// ConversationService 定义了对话业务逻辑的接口。
type ConversationService interface {
	GetConversationHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error)
	GetArchive(ctx context.Context, userID uint, after uint64, limit int) ([]model.Conversation, error)
}

type conversationService struct {
	repo repository.ConversationRepository
}

// NewConversationService 创建一个新的 ConversationService。
func NewConversationService(repo repository.ConversationRepository) ConversationService {
	return &conversationService{repo: repo}
}

// GetConversationHistory 获取用户当前会话的完整消息历史。
func (s *conversationService) GetConversationHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error) {
	conversationID, err := s.repo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.repo.GetConversationHistory(ctx, conversationID)
}

// GetArchive pages immutable complete turns without putting them all into the LLM.
func (s *conversationService) GetArchive(ctx context.Context, userID uint, after uint64, limit int) ([]model.Conversation, error) {
	conversationID, err := s.repo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.repo.GetConversationTurns(ctx, userID, conversationID, after, limit)
}

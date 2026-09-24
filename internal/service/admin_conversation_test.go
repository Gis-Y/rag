package service

import (
	"context"
	"encoding/json"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"reflect"
	"strings"
	"testing"
	"time"
)

type adminConversationRepo struct {
	repository.ConversationRepository
	turns  []model.Conversation
	total  int64
	offset int
	limit  int
}

func (r *adminConversationRepo) FindConversationTurnsPage(_ context.Context, _ *uint, _, _ *time.Time, offset, limit int) ([]model.Conversation, int64, error) {
	r.offset, r.limit = offset, limit
	return r.turns, r.total, nil
}

type adminConversationUserRepo struct {
	repository.UserRepository
	users map[uint]*model.User
}

func (r adminConversationUserRepo) FindByID(id uint) (*model.User, error) {
	return r.users[id], nil
}

func TestAdminConversationPagePreservesSourcesAndEmptyArrays(t *testing.T) {
	old := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	source := model.SourceCitation{Number: 1, DocumentID: 11, Version: "v1", FileName: "报告.pdf"}
	repo := &adminConversationRepo{total: 22, turns: []model.Conversation{
		{UserID: 7, Question: "新问题", Answer: "新答案", Sources: []model.SourceCitation{source}, CreatedAt: newer},
		{UserID: 7, Question: "旧问题", Answer: "旧答案", CreatedAt: old},
	}}
	s := &adminService{conversationRepo: repo, userRepo: adminConversationUserRepo{users: map[uint]*model.User{7: {ID: 7, Username: "alice"}}}}
	userID := uint(7)
	page, err := s.GetAllConversations(context.Background(), &userID, nil, nil, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if repo.offset != 10 || repo.limit != 10 || page.TotalElements != 22 || page.TotalPages != 3 || page.Number != 2 || page.Size != 10 {
		t.Fatalf("wrong pagination: repo=%d/%d page=%+v", repo.offset, repo.limit, page)
	}
	if got := []string{page.Content[0].Content, page.Content[1].Content, page.Content[2].Content, page.Content[3].Content}; !reflect.DeepEqual(got, []string{"旧问题", "旧答案", "新问题", "新答案"}) {
		t.Fatalf("turns are not chronological within page: %v", got)
	}
	if page.Content[1].Sources == nil || len(page.Content[1].Sources) != 0 || !reflect.DeepEqual(page.Content[3].Sources, []model.SourceCitation{source}) {
		t.Fatalf("sources were lost or encoded as null: %+v", page.Content)
	}

	repo.turns, repo.total = nil, 0
	empty, err := s.GetAllConversations(context.Background(), &userID, nil, nil, 1, 10)
	if err != nil || empty.Content == nil || len(empty.Content) != 0 {
		t.Fatalf("empty history must be []: %+v %v", empty, err)
	}
	encoded, err := json.Marshal(empty)
	if err != nil || !strings.Contains(string(encoded), `"content":[]`) {
		t.Fatalf("empty history JSON must be an array: %s %v", encoded, err)
	}
}

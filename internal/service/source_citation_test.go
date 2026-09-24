package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/llm"
)

func TestSourceCitationsFollowSelectedChildAndParentEvidence(t *testing.T) {
	child := model.SearchResponseDTO{DocumentID: 12, Version: "v1", UserID: "7", FileName: "报告(终稿).pdf", ChunkKey: "c1", ParentID: "p1",
		TextContent: "子块", ContextPrefix: "单位：万元\n", HeadingPath: []string{"资产"}, SourceSpans: []model.SourceSpan{{BlockID: "b", End: 2}},
		ParentText: "单位：万元\n子块和完整上下文", ParentSourceSpans: []model.SourceSpan{{BlockID: "b", End: 9}}, ParentRowRange: []int{1, 4}}
	other := child
	other.DocumentID, other.ChunkKey, other.ParentID, other.ParentText = 37, "c2", "", ""
	hits := []model.SearchResponseDTO{child, other, child}
	sources := sourceCitations(hits)
	if len(sources) != 4 || sources[0].DocumentID != 12 || sources[1].DocumentID != 37 || sources[3].Number != 4 || sources[3].Kind != "parent_context" ||
		sources[3].ChunkKey != "p1" || sources[3].SourceSpans[0].End != 9 || !reflect.DeepEqual(sources[3].RowRange, child.ParentRowRange) {
		t.Fatalf("citation identities or full parent range changed: %+v", sources)
	}
	s := &chatService{}
	contextText := s.buildContextText(hits)
	if !strings.Contains(contextText, "单位：万元") || strings.Contains(s.buildSystemMessage(), "(来源#编号: 文件名)") {
		t.Fatalf("context prefix missing or old citation prompt retained: %s", contextText)
	}
	fitted, err := s.fitAnswerEvidence(queryExecution{Results: hits}, func(q queryExecution) bool {
		return len(q.Results) == 2 && q.Results[0].ParentText == ""
	})
	if err != nil {
		t.Fatal(err)
	}
	sources = sourceCitations(fitted.Results)
	if len(sources) != 2 || sources[0].Number != 1 || sources[1].Number != 2 || sources[1].DocumentID != 37 {
		t.Fatalf("dropped evidence remained clickable: %+v", sources)
	}
	if len(sourceCitations([]model.SearchResponseDTO{{FileName: child.FileName}})) != 0 {
		t.Fatal("filename repaired missing source identity")
	}
}

func TestChatEmitsAndPersistsOnlyServerSourcesBeforeAnswer(t *testing.T) {
	for _, fail := range []bool{false, true} {
		repo := &queryTestRepo{}
		var sent []model.SourceCitation
		generated := false
		transportErr := errors.New("source transport closed")
		s := testChat(repo, func(context.Context, string, int, *model.User) ([]model.SearchResponseDTO, error) {
			return []model.SearchResponseDTO{{DocumentID: 12, Version: "v1", FileName: "报告.pdf", TextContent: "收入100", ContextPrefix: "单位：万元\n"}}, nil
		}, func(_ context.Context, messages []llm.Message, _ *llm.GenerationParams, emit llm.ChunkHandler) error {
			generated = true
			if len(sent) != 1 || sent[0].DocumentID != 12 || !strings.Contains(messages[len(messages)-2].Content, "单位：万元") {
				t.Fatal("answer started before trusted sources, or omitted source context")
			}
			return emit([]byte("100万元 [来源#1]"))
		})
		s.queryCfg.Enabled = false
		err := s.StreamResponse(context.Background(), "收入多少", &model.User{ID: 7}, func([]byte) error { return nil }, func(sources []model.SourceCitation) error {
			sent = sources
			if fail {
				return transportErr
			}
			return nil
		}, nil)
		if fail {
			if !errors.Is(err, transportErr) || generated || repo.writes != 0 {
				t.Fatalf("failed source delivery continued generation/save: %v %t %d", err, generated, repo.writes)
			}
		} else if err != nil || !generated || len(repo.saved) != 2 || !reflect.DeepEqual(repo.saved[1].Sources, sent) {
			t.Fatalf("source identity not persisted with answer: %v %+v", err, repo.saved)
		}
	}
}

func TestSourceMetadataDoesNotEnterRecentOrSummaryMemory(t *testing.T) {
	turn := model.Conversation{TurnNo: 1, Question: "简短问题", Answer: "简短回答", Sources: []model.SourceCitation{{Number: 1, DocumentID: 12, Version: "v1", FileName: strings.Repeat("PRIVATE_SOURCE_IDENTITY", 1000)}}}
	if recent := turnsToMessages([]model.Conversation{turn}); len(recent) != 2 || len(recent[1].Sources) != 0 {
		t.Fatalf("citation payload increased recent-history input: %+v", recent)
	}
	s := testChat(&queryTestRepo{}, nil, func(_ context.Context, messages []llm.Message, _ *llm.GenerationParams, emit llm.ChunkHandler) error {
		for _, message := range messages {
			if strings.Contains(message.Content, "PRIVATE_SOURCE_IDENTITY") || strings.Contains(message.Content, `"documentId"`) {
				t.Fatal("persisted citation metadata leaked into summary input")
			}
		}
		return emitJSON(emit, conversationSummary{})
	})
	if _, count, err := s.summarizeTurns(context.Background(), conversationSummary{}, []model.Conversation{turn}); err != nil || count != 1 {
		t.Fatalf("metadata changed summary budget: %d %v", count, err)
	}
}

type sourceStateRepo struct {
	DocumentStateRepository
	states map[uint]model.DocumentProcessingState
}

func (r sourceStateRepo) FindStates(ctx context.Context, _ []uint) (map[uint]model.DocumentProcessingState, error) {
	return r.states, ctx.Err()
}

func TestPublishedDocumentUsesIDAndVersionNeverSameName(t *testing.T) {
	files := []model.FileUpload{{ID: 12, UserID: 7, FileName: "同名(终稿).pdf", Status: 1}, {ID: 37, UserID: 8, FileName: "同名(终稿).pdf", Status: 1}, {ID: 44, UserID: 7, FileName: "同名(终稿).pdf", Status: 3}}
	states := map[uint]model.DocumentProcessingState{12: {DocumentID: 12, UserID: 7, ActiveVersion: "v1"}, 37: {DocumentID: 37, UserID: 8, ActiveVersion: "v2"}}
	s := &documentService{uploadRepo: contextUploadRepo{find: func(ctx context.Context) ([]model.FileUpload, error) { return files, ctx.Err() }},
		userService: NewUserService(nil, nil, nil), processing: DocumentProcessingOptions{Repository: sourceStateRepo{states: states}}}
	user := &model.User{ID: 7}
	for _, version := range []string{"", "v2"} {
		file, active, err := s.findPublishedDocument(context.Background(), user, 37, version)
		if err != nil || file.ID != 37 || file.UserID != 8 || active != "v2" {
			t.Fatalf("same-name document conflated: %+v %s %v", file, active, err)
		}
	}
	for _, tc := range []struct {
		id      uint
		version string
	}{{0, ""}, {99, "v1"}, {44, "v1"}, {37, "v1"}} {
		if _, _, err := s.findPublishedDocument(context.Background(), user, tc.id, tc.version); err == nil {
			t.Fatalf("invalid/deleted/unavailable source accepted: %+v", tc)
		}
	}
	delete(states, 37)
	if _, _, err := s.findPublishedDocument(context.Background(), user, 37, "v2"); !errors.Is(err, repository.ErrDocumentChanged) {
		t.Fatalf("missing publication accepted: %v", err)
	}
	states[37] = model.DocumentProcessingState{DocumentID: 37, UserID: 7, ActiveVersion: "v2"}
	if _, _, err := s.findPublishedDocument(context.Background(), user, 37, "v2"); err == nil {
		t.Fatal("state from different owner accepted")
	}
	files = files[:1] // SQL no longer authorizes the second same-name document.
	if _, err := s.GenerateDownloadURL(context.Background(), 37, "v2", user); err == nil {
		t.Fatal("unauthorized document reached object storage")
	}
	if _, err := s.GetFilePreviewContent(context.Background(), 37, "v2", user); err == nil {
		t.Fatal("unauthorized preview reached object storage")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.findPublishedDocument(ctx, user, 12, "v1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("source ACL lookup lost cancellation: %v", err)
	}
}

func TestDocumentFinalAccessReloadUsesCurrentUser(t *testing.T) {
	users := NewUserService(contextUserRepo{find: func(context.Context) (*model.User, error) {
		return &model.User{ID: 7, Username: "alice", OrgTags: "CURRENT"}, nil
	}}, nil, nil)
	s := &documentService{userService: users}
	current, err := s.reloadAccessUser(context.Background(), &model.User{ID: 7, Username: "alice", OrgTags: "STALE"})
	if err != nil || current.OrgTags != "CURRENT" {
		t.Fatalf("current ACL was not reloaded: %+v %v", current, err)
	}
	if _, err := s.reloadAccessUser(context.Background(), &model.User{ID: 8, Username: "alice"}); err == nil {
		t.Fatal("changed user identity was accepted")
	}
}

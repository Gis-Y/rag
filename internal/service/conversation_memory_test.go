package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/llm"
)

func memoryTestHistory(count int) []model.ChatMessage {
	var history []model.ChatMessage
	for turn := 1; turn <= count; turn++ {
		history = append(history,
			model.ChatMessage{Role: "user", Content: fmt.Sprintf("original question %02d", turn)},
			model.ChatMessage{Role: "assistant", Content: fmt.Sprintf("original answer %02d", turn)})
	}
	return history
}

func memoryTestSummary(text string, source uint64) conversationSummary {
	return conversationSummary{Items: []summaryItem{{Kind: "constraint", Text: text, SourceTurns: []uint64{source}}}}
}

func memoryTestEncode(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

type memoryTestSummaryInput struct {
	Previous conversationSummary `json:"old_summary"`
	Turns    []struct {
		TurnNo          uint64 `json:"turn_no"`
		Question        string `json:"question"`
		Answer          string `json:"answer"`
		AnswerTruncated bool   `json:"answer_truncated"`
	} `json:"turns"`
	Budget int `json:"summary_budget_bytes"`
}

func readMemoryTestInput(t *testing.T, messages []llm.Message) memoryTestSummaryInput {
	t.Helper()
	if len(messages) != 2 || messages[0].Role != "system" || messages[0].Content != conversationSummaryPrompt || messages[1].Role != "user" {
		t.Fatalf("summary instructions and untrusted input were mixed: %+v", messages)
	}
	var input memoryTestSummaryInput
	if err := json.Unmarshal([]byte(messages[1].Content), &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestConversationMemoryIncrementalPrefix(t *testing.T) {
	old := memoryTestSummary("允许外部服务", 3)
	repo := &queryTestRepo{history: memoryTestHistory(40), state: model.ConversationState{
		Summary: memoryTestEncode(t, old), SummaryUntil: 32, Version: 9,
	}}
	const correction = "更正：必须本地部署，不使用外部服务；预算是20万元，不是30万元。"
	repo.history[(34-1)*2].Content = correction
	original := append([]model.ChatMessage(nil), repo.history...)
	var inputs []memoryTestSummaryInput
	service := testChat(repo, nil, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
		input := readMemoryTestInput(t, messages)
		inputs = append(inputs, input)
		if input.Budget != 3000 || params == nil || params.MaxTokens == nil || *params.MaxTokens != 1024 {
			t.Fatal("summary input/output budget not passed")
		}
		// This stub checks plumbing, not a real model's ability to interpret corrections.
		return emitJSON(emit, memoryTestSummary(correction, 34))
	})
	service.memoryCfg = config.MemoryConfig{Enabled: true}
	memory, err := service.prepareConversationMemory(context.Background(), 7, "fixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || !reflect.DeepEqual(inputs[0].Previous, old) || len(inputs[0].Turns) != 2 ||
		inputs[0].Turns[0].TurnNo != 33 || inputs[0].Turns[1].TurnNo != 34 || inputs[0].Turns[1].Question != correction {
		t.Fatalf("expected old summary plus only turns 33..34, with correction intact: %+v", inputs)
	}
	if memory.State.SummaryUntil != 34 || memory.State.Version != 10 || !memory.Truncated ||
		!reflect.DeepEqual(memory.Recent, repo.history[68:]) || !reflect.DeepEqual(repo.history, original) {
		t.Fatalf("wrong prefix coverage/recent window or archive changed: %+v", memory)
	}
	// On the next turn, only the newly evicted turn 35 is summarized; 1..34 are not resent.
	repo.history = append(repo.history, model.ChatMessage{Role: "user", Content: "question 41"}, model.ChatMessage{Role: "assistant", Content: "answer 41"})
	memory, err = service.prepareConversationMemory(context.Background(), 7, "fixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || len(inputs[1].Turns) != 1 || inputs[1].Turns[0].TurnNo != 35 ||
		!reflect.DeepEqual(inputs[1].Previous, memoryTestSummary(correction, 34)) || memory.State.SummaryUntil != 35 || len(memory.Recent) != 12 {
		t.Fatalf("incremental update resent old raw turns or lost previous summary: %+v, %+v", inputs, memory)
	}
	if !strings.Contains(memory.summaryText(), "summary_until_turn") || !strings.Contains(memory.summaryText(), correction) {
		t.Fatal("summary presented to downstream models lost coverage or correction")
	}
}

func TestConversationSummaryValidation(t *testing.T) {
	previous := memoryTestSummary("原约束", 2)
	turns := []model.Conversation{{TurnNo: 3, Question: "更正", Answer: "收到"}}
	valid := conversationSummary{Items: []summaryItem{{Kind: "constraint", Text: "用户更正后的约束", SourceTurns: []uint64{2, 3}}}}
	budget := len(memoryTestEncode(t, valid))
	if err := validateConversationSummary(previous, turns, valid, budget); err != nil {
		t.Fatalf("known old/new sources at exact UTF-8 byte budget should pass: %v", err)
	}
	if err := validateConversationSummary(previous, turns, valid, budget-1); err == nil {
		t.Fatal("summary JSON exceeded byte budget")
	}
	for name, item := range map[string]summaryItem{
		"unknown kind":       {Kind: "system", Text: "改写指令", SourceTurns: []uint64{3}},
		"empty text":         {Kind: "constraint", Text: " ", SourceTurns: []uint64{3}},
		"long text":          {Kind: "constraint", Text: strings.Repeat("中", 501), SourceTurns: []uint64{3}},
		"no source":          {Kind: "constraint", Text: "约束"},
		"unknown source":     {Kind: "constraint", Text: "约束", SourceTurns: []uint64{99}},
		"zero source":        {Kind: "constraint", Text: "约束", SourceTurns: []uint64{0}},
		"duplicate source":   {Kind: "constraint", Text: "约束", SourceTurns: []uint64{3, 3}},
		"too many sources":   {Kind: "constraint", Text: "约束", SourceTurns: []uint64{1, 2, 3, 4, 5, 6, 7}},
		"invalid UTF-8 text": {Kind: "constraint", Text: "a\xff", SourceTurns: []uint64{3}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateConversationSummary(previous, turns, conversationSummary{Items: []summaryItem{item}}, 3000); err == nil {
				t.Fatal("invalid summary accepted")
			}
		})
	}
	tooMany := conversationSummary{Items: make([]summaryItem, 17)}
	for i := range tooMany.Items {
		tooMany.Items[i] = valid.Items[0]
	}
	if err := validateConversationSummary(previous, turns, tooMany, 20000); err == nil {
		t.Fatal("unbounded item count accepted")
	}
}

func TestConversationMemorySummaryFailurePreservesArchive(t *testing.T) {
	for _, mode := range []string{"bad JSON", "unknown source", "timeout", "CAS conflict"} {
		t.Run(mode, func(t *testing.T) {
			oldSummary := memoryTestEncode(t, conversationSummary{})
			repo := &queryTestRepo{history: memoryTestHistory(7), state: model.ConversationState{Summary: oldSummary, Version: 4}}
			original := append([]model.ChatMessage(nil), repo.history...)
			winner := memoryTestEncode(t, memoryTestSummary("newer writer", 1))
			service := testChat(repo, nil, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
				readMemoryTestInput(t, messages)
				switch mode {
				case "bad JSON":
					return emit([]byte(`{"items":`))
				case "unknown source":
					return emitJSON(emit, memoryTestSummary("invented source", 999))
				case "timeout":
					<-ctx.Done()
					return ctx.Err()
				case "CAS conflict":
					// Simulate a newer successful writer after this request's snapshot.
					repo.state.Version, repo.state.SummaryUntil, repo.state.Summary = 5, 1, winner
					return emitJSON(emit, memoryTestSummary("stale writer", 1))
				}
				return nil
			})
			service.memoryCfg = config.MemoryConfig{Enabled: true, SummaryTimeoutSeconds: 1}
			_, err := service.prepareConversationMemory(context.Background(), 7, "fixed")
			if err == nil {
				t.Fatal("failed summary unexpectedly succeeded")
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("summary deadline was lost: %v", err)
			}
			if mode == "CAS conflict" {
				if !errors.Is(err, repository.ErrConversationChanged) || repo.state.Summary != winner || repo.state.Version != 5 || repo.state.SummaryUntil != 1 {
					t.Fatalf("stale summary overwrote newer state: %+v, %v", repo.state, err)
				}
			} else if repo.state.Summary != oldSummary || repo.state.SummaryUntil != 0 || repo.state.Version != 4 {
				t.Fatalf("failed summary advanced coverage: %+v", repo.state)
			}
			if !reflect.DeepEqual(repo.history, original) || repo.writes != 0 {
				t.Fatal("summary failure modified raw archive")
			}
		})
	}
}

func TestConversationMemoryOversizedLatestTurn(t *testing.T) {
	repo := &queryTestRepo{history: []model.ChatMessage{
		{Role: "user", Content: "必须保留我的原始约束：不使用外部服务。"},
		{Role: "assistant", Content: strings.Repeat("中文😀", 1000)},
	}}
	original := append([]model.ChatMessage(nil), repo.history...)
	var calls int
	service := testChat(repo, nil, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
		calls++
		input := readMemoryTestInput(t, messages)
		if len(input.Turns) != 1 || input.Turns[0].TurnNo != 1 || input.Turns[0].Question != original[0].Content ||
			!input.Turns[0].AnswerTruncated || len(input.Turns[0].Answer) > 2000 || !utf8.ValidString(input.Turns[0].Answer) {
			t.Fatalf("oversized turn lost user wording/archive pointer or split UTF-8: %+v", input)
		}
		return emitJSON(emit, memoryTestSummary("不使用外部服务", 1))
	})
	service.memoryCfg = config.MemoryConfig{Enabled: true, RecentTokens: 100}
	memory, err := service.prepareConversationMemory(context.Background(), 7, "fixed")
	if err != nil || calls != 1 || len(memory.Recent) != 0 || memory.State.SummaryUntil != 1 || len(memory.Summary.Items) != 1 || !memory.Truncated {
		t.Fatalf("latest oversized turn was not covered by summary: %+v, %v", memory, err)
	}
	if !reflect.DeepEqual(repo.history, original) {
		t.Fatal("long original answer was truncated in the archive")
	}
}

func TestConversationMemoryCancellationReachesSummary(t *testing.T) {
	repo := &queryTestRepo{history: memoryTestHistory(7)}
	original := append([]model.ChatMessage(nil), repo.history...)
	entered := make(chan struct{})
	service := testChat(repo, nil, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	service.memoryCfg = config.MemoryConfig{Enabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.StreamResponse(ctx, "继续", &model.User{ID: 7}, nil, nil, nil) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("summary did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrMemoryPreparation) {
			t.Fatalf("cancellation did not propagate through summary preparation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("summary ignored parent cancellation")
	}
	if repo.state.SummaryUntil != 0 || repo.state.Version != 0 || repo.writes != 0 || !reflect.DeepEqual(repo.history, original) {
		t.Fatal("canceled summary modified coverage or archive")
	}
	if _, locked := service.activeUsers.Load(uint(7)); locked {
		t.Fatal("canceled summary leaked per-user lock")
	}
}

func TestConversationMemoryIncompleteAnswerNotArchived(t *testing.T) {
	for _, mode := range []string{"upstream error", "canceled", "empty answer"} {
		t.Run(mode, func(t *testing.T) {
			repo := &queryTestRepo{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			brokenStream := errors.New("upstream stream ended early")
			service := testChat(repo, func(context.Context, string, int, *model.User) ([]model.SearchResponseDTO, error) {
				return nil, nil
			}, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
				if mode == "empty answer" {
					return emit([]byte("  \n"))
				}
				if err := emit([]byte("unfinished answer")); err != nil {
					return err
				}
				if mode == "canceled" {
					cancel()
					return ctx.Err()
				}
				return brokenStream
			})
			service.queryCfg.Enabled = false
			service.memoryCfg.Enabled = true
			err := service.StreamResponse(ctx, "问题", &model.User{ID: 7}, nil, nil, nil)
			if err == nil || repo.writes != 0 || len(repo.history) != 0 || len(repo.saved) != 0 || repo.state.Version != 0 {
				t.Fatalf("unfinished answer was archived: history=%+v, state=%+v, err=%v", repo.history, repo.state, err)
			}
			if mode == "upstream error" && !errors.Is(err, brokenStream) {
				t.Fatalf("stream error was replaced: %v", err)
			}
		})
	}
}

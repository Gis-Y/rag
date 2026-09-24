package service

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/llm"
)

func TestContextBudgetEstimates(t *testing.T) {
	for text, want := range map[string]int{"": 0, "test": 4, "中文": 6, "😀": 4, "e\u0301": 3} {
		if got := estimateTextTokens(text); got != want {
			t.Errorf("estimateTextTokens(%q) = %d, want %d", text, got, want)
		}
	}
	if got := estimateMessageTokens(nil); got != 16 {
		t.Fatalf("empty framing = %d, want 16", got)
	}
	messages := []llm.Message{{Role: "user", Content: "中文"}, {Role: "assistant", Content: "😀"}}
	if got := estimateMessageTokens(messages); got != 71 {
		t.Fatalf("message estimate = %d, want 71 including role and framing", got)
	}
}

func TestFitRecentTurns(t *testing.T) {
	turns := []model.Conversation{
		{TurnNo: 3, Question: "c", Answer: "C"},
		{TurnNo: 1, Question: "a", Answer: "A"},
		{TurnNo: 2, Question: "b", Answer: "B"},
	}
	original := append([]model.Conversation(nil), turns...)
	for _, tc := range []struct {
		name          string
		pairs, tokens int
		want          []uint64
		cutoff        uint64
	}{
		{"all", 6, 157, []uint64{1, 2, 3}, 0},
		{"exact two pairs", 6, 110, []uint64{2, 3}, 1},
		{"one byte below two pairs", 6, 109, []uint64{3}, 2},
		{"pair count cap", 1, 1000, []uint64{3}, 2},
		{"exact one pair", 6, 63, []uint64{3}, 2},
		{"framing prevents fit", 6, 62, nil, 3},
		{"zero budget", 6, 0, nil, 3},
		{"negative budget", 6, -1, nil, 3},
		{"zero pairs", 0, 1000, nil, 3},
		{"negative pairs", -1, 1000, nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recent, cutoff := fitRecentTurns(turns, tc.pairs, tc.tokens)
			var got []uint64
			for _, turn := range recent {
				got = append(got, turn.TurnNo)
				if turn.Question != strings.ToLower(turn.Answer) {
					t.Fatalf("question/answer pair changed: %+v", turn)
				}
			}
			if !reflect.DeepEqual(got, tc.want) || cutoff != tc.cutoff {
				t.Fatalf("got turns %v cutoff %d, want %v cutoff %d", got, cutoff, tc.want, tc.cutoff)
			}
			if !reflect.DeepEqual(turns, original) {
				t.Fatal("input turns were reordered or rewritten")
			}
		})
	}
	if recent, cutoff := fitRecentTurns(nil, 6, 1000); len(recent) != 0 || cutoff != 0 {
		t.Fatalf("empty input: %v %d", recent, cutoff)
	}
	turns[0].Question = strings.Repeat("😀", 100)
	if recent, cutoff := fitRecentTurns(turns, 6, 110); len(recent) != 0 || cutoff != 3 {
		t.Fatalf("oversized latest pair must go to summary, not skip to old pairs: %v %d", recent, cutoff)
	}
	turns[0].Question, turns[2].Answer = "c", " "
	if recent, cutoff := fitRecentTurns(turns, 6, 1000); len(recent) != 1 || recent[0].TurnNo != 3 || cutoff != 2 {
		t.Fatalf("incomplete pair was retained or summary coverage has a hole: %v %d", recent, cutoff)
	}
}

func TestTruncateTextBudget(t *testing.T) {
	for _, tc := range []struct {
		text   string
		budget int
		want   string
	}{
		{"中文😀tail", -1, ""},
		{"中文😀tail", 0, ""},
		{"中文😀tail", 1, ""},
		{"中文😀tail", 2, ""},
		{"中文😀tail", 3, "…"},
		{"中文😀tail", 5, "…"},
		{"中文😀tail", 6, "中…"},
		{"中文😀tail", 9, "中文…"},
		{"中文😀tail", 12, "中文…"},
		{"中文😀tail", 13, "中文😀…"},
		{"中文😀tail", 14, "中文😀tail"},
		{"a", 1, "a"},
		{"a\xff中", 4, "a中"},
	} {
		got := truncateTextBudget(tc.text, tc.budget)
		if got != tc.want || !utf8.ValidString(got) || len(got) > max(0, tc.budget) {
			t.Errorf("truncateTextBudget(%q, %d) = %q (%d bytes), want %q", tc.text, tc.budget, got, len(got), tc.want)
		}
	}
}

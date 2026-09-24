package service

import (
	"sort"
	"strings"
	"unicode/utf8"

	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/llm"
)

// ponytail: count UTF-8 bytes conservatively, not characters or exact model tokens.
// Add a model-specific tokenizer only if this deliberately large estimate wastes
// meaningful context; framing overhead below is also an estimate, not a guarantee.
func estimateTextTokens(text string) int { return len(text) }

func estimateMessageTokens(messages []llm.Message) int {
	tokens := 16 // Global chat framing.
	for _, message := range messages {
		tokens += 16 + estimateTextTokens(message.Role) + estimateTextTokens(message.Content)
	}
	return tokens
}

// fitRecentTurns returns an ascending, complete suffix without modifying turns.
// cutoff is the last omitted turn: the summary must cover through this turn,
// including an oversized latest turn when no raw pair fits. Never skip a newer
// pair to squeeze in an older one, which would leave a hole in summary coverage.
func fitRecentTurns(turns []model.Conversation, maxPairs, maxTokens int) (recent []model.Conversation, cutoff uint64) {
	ordered := append([]model.Conversation(nil), turns...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TurnNo < ordered[j].TurnNo })
	start, used := len(ordered), estimateMessageTokens(nil)
	for start > 0 && len(ordered)-start < maxPairs {
		turn := ordered[start-1]
		if strings.TrimSpace(turn.Question) == "" || strings.TrimSpace(turn.Answer) == "" ||
			!utf8.ValidString(turn.Question) || !utf8.ValidString(turn.Answer) {
			break
		}
		cost := estimateMessageTokens([]llm.Message{
			{Role: "user", Content: turn.Question},
			{Role: "assistant", Content: turn.Answer},
		}) - estimateMessageTokens(nil)
		if used+cost > maxTokens {
			break
		}
		used += cost
		start--
	}
	if start > 0 {
		cutoff = ordered[start-1].TurnNo
	}
	if start < len(ordered) {
		recent = ordered[start:]
	}
	return recent, cutoff
}

// truncateTextBudget reserves space for the ellipsis and never splits UTF-8.
func truncateTextBudget(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	text = strings.ToValidUTF8(text, "")
	if estimateTextTokens(text) <= maxTokens {
		return text
	}
	const ellipsis = "…"
	if maxTokens < len(ellipsis) {
		return ""
	}
	end := maxTokens - len(ellipsis)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + ellipsis
}

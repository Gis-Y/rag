package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
)

var ErrMemoryInput = errors.New("invalid confirmed memory")

// UserMemoryInput deliberately has no owner field; authentication supplies ownership.
type UserMemoryInput struct {
	Scope     string     `json:"scope"`
	Kind      string     `json:"kind"`
	Key       string     `json:"key"`
	Content   string     `json:"content"`
	Keywords  []string   `json:"keywords"`
	ExpiresAt *time.Time `json:"expires_at"`
	Confirmed bool       `json:"confirmed"`
	Version   uint64     `json:"version"`
}

type UserMemoryService struct {
	repo         *repository.UserMemoryRepository
	conversation repository.ConversationRepository
	budget       int
}

func NewUserMemoryService(repo *repository.UserMemoryRepository, conversation repository.ConversationRepository, cfg config.MemoryConfig) *UserMemoryService {
	return &UserMemoryService{repo: repo, conversation: conversation, budget: cfg.WithDefaults().LongTermTokens}
}

func (s *UserMemoryService) List(ctx context.Context, userID uint) ([]model.UserMemory, error) {
	if _, err := s.conversation.GetOrCreateConversationID(ctx, userID); err != nil {
		return nil, err
	}
	return s.repo.ListActive(ctx, userID)
}

func (s *UserMemoryService) Save(ctx context.Context, userID, id uint, input UserMemoryInput) (model.UserMemory, error) {
	item, err := validateUserMemory(input, time.Now(), s.budget)
	if err != nil {
		return item, err
	}
	if (id == 0 && input.Version != 0) || (id != 0 && input.Version == 0) {
		return item, fmt.Errorf("%w: 新增无需版本，修改必须携带当前版本", ErrMemoryInput)
	}
	// Migrate legacy history before a reset, so it cannot reappear after forgetting.
	if _, err := s.conversation.GetOrCreateConversationID(ctx, userID); err != nil {
		return item, err
	}
	if id == 0 {
		return s.repo.Create(ctx, userID, item)
	}
	return s.repo.Update(ctx, userID, id, input.Version, item)
}

func (s *UserMemoryService) Delete(ctx context.Context, userID, id uint, version uint64) error {
	if id == 0 || version == 0 {
		return fmt.Errorf("%w: 删除必须携带记录和版本", ErrMemoryInput)
	}
	if _, err := s.conversation.GetOrCreateConversationID(ctx, userID); err != nil {
		return err
	}
	return s.repo.Delete(ctx, userID, id, version)
}

// This catches common accidental credentials, not arbitrary secrets/DLP.
var memoryCredentialPattern = regexp.MustCompile(`(?i)(-----BEGIN [A-Z ]*PRIVATE KEY-----|\bsk-[a-z0-9_-]{16,}|\bBearer\s+[a-z0-9_.-]{16,}|(?:password|passwd|api[_ -]?key|access[_ -]?token|secret|密码|密钥|令牌)\s*[:=：]\s*\S+)`)

func validateUserMemory(input UserMemoryInput, now time.Time, budget int) (model.UserMemory, error) {
	item := model.UserMemory{Scope: strings.TrimSpace(input.Scope), Kind: input.Kind, Key: strings.TrimSpace(input.Key), Content: strings.TrimSpace(input.Content), ExpiresAt: input.ExpiresAt}
	bad := func(message string) (model.UserMemory, error) {
		return model.UserMemory{}, fmt.Errorf("%w: %s", ErrMemoryInput, message)
	}
	if !input.Confirmed {
		return bad("请明确确认要保存的长期记忆")
	}
	if item.Scope == "" {
		item.Scope = "global"
	}
	if !boundedText(item.Scope, 64) || !boundedText(item.Key, 64) || !boundedText(item.Content, 500) {
		return bad("范围和标题最多64字符，内容为1到500字符")
	}
	switch item.Kind {
	case "preference", "project", "decision":
	default:
		return bad("记忆类型必须为preference、project或decision")
	}
	if item.ExpiresAt != nil && !item.ExpiresAt.After(now) {
		return bad("到期时间必须晚于现在")
	}
	if len(input.Keywords) > 8 {
		return bad("最多8个检索关键词")
	}
	seen := make(map[string]bool)
	item.Keywords = []string{}
	for _, keyword := range input.Keywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if !boundedText(keyword, 40) {
			return bad("检索关键词为1到40字符")
		}
		if !seen[keyword] {
			seen[keyword] = true
			item.Keywords = append(item.Keywords, keyword)
		}
	}
	if len(item.Keywords) == 0 && !(item.Scope == "global" && item.Kind == "preference") {
		return bad("项目、决定及限定范围的偏好至少需要一个检索关键词")
	}
	if memoryCredentialPattern.MatchString(item.Scope + " " + item.Key + " " + item.Content + " " + strings.Join(item.Keywords, " ")) {
		return bad("请勿保存密码、密钥或访问令牌")
	}
	encoded, _ := json.Marshal([]recalledMemory{recallItem(item)})
	if estimateTextTokens(string(encoded)) > budget {
		return bad("该条目超出长期记忆预算，请缩短内容或调整long_term_tokens")
	}
	return item, nil
}

type recalledMemory struct {
	ID      uint   `json:"memory_id"`
	Scope   string `json:"scope"`
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Content string `json:"content"`
}

func recallItem(item model.UserMemory) recalledMemory {
	return recalledMemory{item.ID, item.Scope, item.Kind, item.Key, item.Content}
}

// ponytail: at most 50 explicit entries; keyword recall avoids a second vector index.
// Add semantic retrieval only when measured paraphrase misses justify it.
func selectUserMemories(items []model.UserMemory, userID uint, query string, memory conversationMemory, budget int, now time.Time) []recalledMemory {
	lookup := strings.ToLower(query)
	explicitScope := false
	for _, item := range items {
		if item.UserID == userID && item.Scope != "" && item.Scope != "global" && strings.Contains(lookup, strings.ToLower(item.Scope)) {
			explicitScope = true
			break
		}
	}
	if isMemoryFollowup(query) && !explicitScope {
		// The latest user turn is safer than inheriting every old topic or assistant assertion.
		for i := len(memory.Recent) - 1; i >= 0; i-- {
			if memory.Recent[i].Role == "user" {
				lookup += "\n" + strings.ToLower(memory.Recent[i].Content)
				break
			}
		}
		if len(memory.Recent) == 0 {
			for _, item := range memory.Summary.Items {
				if item.Kind == "topic" || item.Kind == "entity" {
					lookup += "\n" + strings.ToLower(item.Text)
				}
			}
		}
	}
	type rankedMemory struct {
		item  model.UserMemory
		score int
	}
	var ranked []rankedMemory
	for _, item := range items {
		if item.UserID != userID || item.ExpiresAt != nil && !item.ExpiresAt.After(now) {
			continue
		}
		if item.Scope != "global" && !strings.Contains(lookup, strings.ToLower(item.Scope)) {
			continue
		}
		score := 0
		for _, keyword := range item.Keywords {
			keyword = strings.ToLower(strings.TrimSpace(keyword))
			if keyword != "" && strings.Contains(lookup, keyword) {
				score += 2
			}
		}
		if item.Scope == "global" && item.Kind == "preference" {
			score++
		}
		if score > 0 {
			ranked = append(ranked, rankedMemory{item, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if !ranked[i].item.UpdatedAt.Equal(ranked[j].item.UpdatedAt) {
			return ranked[i].item.UpdatedAt.After(ranked[j].item.UpdatedAt)
		}
		return ranked[i].item.ID < ranked[j].item.ID
	})
	selected := []recalledMemory{}
	for _, candidate := range ranked {
		trial := append(append([]recalledMemory(nil), selected...), recallItem(candidate.item))
		encoded, _ := json.Marshal(trial)
		if estimateTextTokens(string(encoded)) <= budget {
			selected = trial
		}
		if len(selected) == 5 {
			break
		}
	}
	return selected
}

func isMemoryFollowup(query string) bool {
	// Explicit topic changes do not inherit the previous topic's private preferences.
	if utf8.RuneCountInString(query) > 100 || strings.Contains(query, "换个话题") || strings.Contains(query, "换一个话题") {
		return false
	}
	for _, pronoun := range []string{"它", "他们", "她们", "这个", "那个", "前者", "后者", "上述", "继续", "刚才"} {
		if strings.Contains(query, pronoun) {
			return true
		}
	}
	return false
}

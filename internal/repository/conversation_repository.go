// Package repository provides persistent conversation storage.
package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"pai-smart-go/internal/model"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const conversationTTL = 7 * 24 * time.Hour

var ErrConversationChanged = errors.New("conversation version or owner changed")
var ErrTurnIDConflict = errors.New("turn ID already belongs to a different payload")

// Read the legacy mapping and history atomically before migrating.
// ponytail: standalone Redis scripts; hash-tag keys before adopting Redis Cluster.
const legacyConversationScript = `
local id = redis.call('GET', KEYS[1])
if not id then return {'', ''} end
return {id, redis.call('GET', 'conversation:' .. id) or ''}
`

const cacheConversationScript = `
local version = redis.call('GET', KEYS[3])
if version and tonumber(version) > tonumber(ARGV[3]) then return 0 end
redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[4])
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[4])
redis.call('SET', KEYS[3], ARGV[3], 'EX', ARGV[4])
return 1
`

type ConversationRepository interface {
	GetOrCreateConversationID(ctx context.Context, userID uint) (string, error)
	GetConversationHistory(ctx context.Context, conversationID string) ([]model.ChatMessage, error)
	UpdateConversationHistory(ctx context.Context, userID uint, conversationID string, messages []model.ChatMessage) error
	GetAllUserConversationMappings(ctx context.Context) (map[uint]string, error)
	GetConversationState(ctx context.Context, userID uint, conversationID string) (model.ConversationState, error)
	GetConversationTurns(ctx context.Context, userID uint, conversationID string, after uint64, limit int) ([]model.Conversation, error)
	FindConversationTurnsPage(ctx context.Context, userID *uint, startTime, endTime *time.Time, offset, limit int) ([]model.Conversation, int64, error)
	AppendConversationTurn(ctx context.Context, userID uint, conversationID string, expectedVersion uint64, turn model.Conversation) error
	UpdateConversationSummary(ctx context.Context, userID uint, conversationID string, expectedVersion, until uint64, summary string) error
}

type conversationRepository struct {
	redisClient *redis.Client
	db          *gorm.DB
}

func NewConversationRepository(redisClient *redis.Client, db *gorm.DB) ConversationRepository {
	return &conversationRepository{redisClient: redisClient, db: db}
}

func (r *conversationRepository) GetOrCreateConversationID(ctx context.Context, userID uint) (string, error) {
	if userID == 0 {
		return "", errors.New("user ID is required")
	}
	var state model.ConversationState
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).First(&state).Error
	if err == nil {
		return state.ID, nil // SQL remains authoritative while Redis is down.
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	if r.redisClient == nil {
		return "", errors.New("Redis is required to check unmigrated conversation history")
	}
	legacy, err := r.redisClient.Eval(ctx, legacyConversationScript, []string{fmt.Sprintf("user:%d:current_conversation", userID)}).StringSlice()
	if err != nil {
		return "", fmt.Errorf("read legacy conversation before migration: %w", err)
	}
	if len(legacy) != 2 {
		return "", errors.New("invalid legacy conversation snapshot")
	}
	conversationID := legacy[0]
	if conversationID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", err
		}
		conversationID = fmt.Sprintf("%x", id)
	}
	if !validConversationID(conversationID) {
		return "", errors.New("invalid legacy conversation ID")
	}
	var messages []model.ChatMessage
	if legacy[1] != "" {
		if !utf8.ValidString(legacy[1]) {
			return "", errors.New("invalid legacy UTF-8 history; original Redis data preserved")
		}
		if err := json.Unmarshal([]byte(legacy[1]), &messages); err != nil {
			return "", fmt.Errorf("invalid legacy history; original Redis data preserved: %w", err)
		}
	}
	turns, err := turnsFromMessages(userID, conversationID, messages)
	if err != nil {
		return "", fmt.Errorf("invalid legacy history; original Redis data preserved: %w", err)
	}
	state = model.ConversationState{ID: conversationID, UserID: userID, Summary: "{}", LastTurn: uint64(len(turns))}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&state).Error; err != nil {
			return err
		}
		if len(turns) > 0 {
			return tx.Create(&turns).Error
		}
		return nil
	})
	if err != nil {
		// A concurrent first request may have already migrated this user.
		var existing model.ConversationState
		if findErr := r.db.WithContext(ctx).Where("user_id = ?", userID).First(&existing).Error; findErr == nil {
			return existing.ID, nil
		}
		return "", fmt.Errorf("archive legacy conversation: %w", err)
	}
	r.cacheHistory(ctx, state, messages)
	return state.ID, nil
}

func (r *conversationRepository) GetConversationState(ctx context.Context, userID uint, conversationID string) (model.ConversationState, error) {
	var state model.ConversationState
	if userID == 0 || !validConversationID(conversationID) {
		return state, errors.New("user ID and conversation ID are required")
	}
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", conversationID, userID).First(&state).Error
	return state, err
}

// GetConversationTurns returns a bounded chronological archive page, never a summary.
func (r *conversationRepository) GetConversationTurns(ctx context.Context, userID uint, conversationID string, after uint64, limit int) ([]model.Conversation, error) {
	if userID == 0 || !validConversationID(conversationID) || limit < 1 || limit > 100 {
		return nil, errors.New("invalid conversation archive query (limit must be 1..100)")
	}
	turns := []model.Conversation{}
	err := r.db.WithContext(ctx).Where("user_id = ? AND conversation_id = ? AND turn_no > ?", userID, conversationID, after).
		Order("turn_no ASC").Limit(limit).Find(&turns).Error
	return turns, err
}

// FindConversationTurnsPage reads the immutable SQL archive directly. Admin
// history must not use GetConversationHistory, which intentionally exposes only
// the newest ten turns for the interactive chat view.
func (r *conversationRepository) FindConversationTurnsPage(ctx context.Context, userID *uint, startTime, endTime *time.Time, offset, limit int) ([]model.Conversation, int64, error) {
	if offset < 0 || limit < 1 || limit > 100 || (userID != nil && *userID == 0) ||
		(startTime != nil && endTime != nil && startTime.After(*endTime)) {
		return nil, 0, errors.New("invalid conversation page query")
	}
	applyFilters := func(db *gorm.DB) *gorm.DB {
		if userID != nil {
			db = db.Where("user_id = ?", *userID)
		}
		if startTime != nil {
			db = db.Where("created_at >= ?", *startTime)
		}
		if endTime != nil {
			db = db.Where("created_at <= ?", *endTime)
		}
		return db
	}

	var total int64
	if err := applyFilters(r.db.WithContext(ctx).Model(&model.Conversation{})).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	turns := []model.Conversation{}
	if total == 0 {
		return turns, 0, nil
	}
	err := applyFilters(r.db.WithContext(ctx)).Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&turns).Error
	return turns, total, err
}

func (r *conversationRepository) AppendConversationTurn(ctx context.Context, userID uint, conversationID string, expectedVersion uint64, turn model.Conversation) error {
	if err := validateTurn(userID, conversationID, turn); err != nil {
		return err
	}
	turn.UserID, turn.ConversationID, turn.ID = userID, conversationID, 0
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}
	turn.CreatedAt = turn.CreatedAt.UTC().Truncate(time.Microsecond)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state model.ConversationState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", conversationID, userID).First(&state).Error; err != nil {
			return err
		}
		var existing model.Conversation
		err := tx.Where("conversation_id = ? AND turn_id = ? AND user_id = ?", conversationID, turn.TurnID, userID).First(&existing).Error
		if err == nil {
			if existing.Question != turn.Question || existing.Answer != turn.Answer || !reflect.DeepEqual(existing.Sources, turn.Sources) || (turn.TurnNo != 0 && existing.TurnNo != turn.TurnNo) {
				return ErrTurnIDConflict
			}
			return nil // Identical requests remain idempotent after the state version advanced.
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if state.Version != expectedVersion || (turn.TurnNo != 0 && turn.TurnNo != state.LastTurn+1) {
			return ErrConversationChanged
		}
		turn.TurnNo = state.LastTurn + 1
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		return updateConversationState(tx, state, map[string]interface{}{"last_turn": turn.TurnNo})
	})
	if err == nil {
		r.refreshCache(ctx, userID, conversationID) // Only cache committed archive data.
	}
	return err
}

func (r *conversationRepository) UpdateConversationSummary(ctx context.Context, userID uint, conversationID string, expectedVersion, until uint64, summary string) error {
	if userID == 0 || !validConversationID(conversationID) || len(summary) > 65536 || !utf8.ValidString(summary) || !json.Valid([]byte(summary)) {
		return errors.New("invalid conversation summary")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state model.ConversationState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", conversationID, userID).First(&state).Error; err != nil {
			return err
		}
		if state.Version != expectedVersion {
			return ErrConversationChanged
		}
		if until <= state.SummaryUntil || until > state.LastTurn || until < state.ContextAfter {
			return errors.New("summary watermark must advance within archived turns")
		}
		return updateConversationState(tx, state, map[string]interface{}{"summary": summary, "summary_until": until})
	})
}

func updateConversationState(tx *gorm.DB, state model.ConversationState, values map[string]interface{}) error {
	values["version"] = state.Version + 1
	result := tx.Model(&model.ConversationState{}).Where("id = ? AND user_id = ? AND version = ?", state.ID, state.UserID, state.Version).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrConversationChanged
	}
	return nil
}

// GetConversationHistory keeps the existing UI contract: the newest 10 whole turns.
func (r *conversationRepository) GetConversationHistory(ctx context.Context, conversationID string) ([]model.ChatMessage, error) {
	if !validConversationID(conversationID) {
		return nil, errors.New("conversation ID is required")
	}
	turns, err := recentConversationTurns(r.db.WithContext(ctx), conversationID)
	if err != nil {
		return nil, err
	}
	return messagesFromTurns(turns), nil
}

// UpdateConversationHistory is append-only compatibility, never snapshot replacement.
// Require the complete recent snapshot plus whole new turns; reject stale or edited history.
func (r *conversationRepository) UpdateConversationHistory(ctx context.Context, userID uint, conversationID string, messages []model.ChatMessage) error {
	turns, err := turnsFromMessages(userID, conversationID, messages)
	if err != nil {
		return err
	}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state model.ConversationState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", conversationID, userID).First(&state).Error; err != nil {
			return err
		}
		recent, err := recentConversationTurns(tx, conversationID)
		if err != nil {
			return err
		}
		if err := validateHistoryAppend(recent, turns); err != nil {
			return err
		}
		pending := turns[len(recent):]
		if len(pending) == 0 {
			return nil
		}
		for i := range pending {
			pending[i].TurnNo = state.LastTurn + uint64(i) + 1
			pending[i].TurnID = fmt.Sprintf("compat-%d", pending[i].TurnNo)
		}
		if err := tx.Create(&pending).Error; err != nil {
			return err
		}
		return updateConversationState(tx, state, map[string]interface{}{"last_turn": state.LastTurn + uint64(len(pending))})
	})
	if err == nil {
		r.refreshCache(ctx, userID, conversationID)
	}
	return err
}

func validateHistoryAppend(recent, snapshot []model.Conversation) error {
	if len(snapshot) < len(recent) {
		return ErrConversationChanged
	}
	for i, turn := range recent {
		if snapshot[i].Question != turn.Question || snapshot[i].Answer != turn.Answer || !reflect.DeepEqual(snapshot[i].Sources, turn.Sources) {
			return ErrConversationChanged
		}
	}
	return nil
}

func (r *conversationRepository) GetAllUserConversationMappings(ctx context.Context) (map[uint]string, error) {
	var states []model.ConversationState
	if err := r.db.WithContext(ctx).Select("id", "user_id").Find(&states).Error; err != nil {
		return nil, err
	}
	result := make(map[uint]string, len(states))
	for _, state := range states {
		result[state.UserID] = state.ID
	}
	return result, nil
}

func recentConversationTurns(db *gorm.DB, conversationID string) ([]model.Conversation, error) {
	turns := []model.Conversation{}
	if err := db.Where("conversation_id = ?", conversationID).Order("turn_no DESC").Limit(10).Find(&turns).Error; err != nil {
		return nil, err
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, nil
}

func messagesFromTurns(turns []model.Conversation) []model.ChatMessage {
	messages := make([]model.ChatMessage, 0, len(turns)*2)
	for _, turn := range turns {
		messages = append(messages, model.ChatMessage{Role: "user", Content: turn.Question, Timestamp: turn.CreatedAt},
			model.ChatMessage{Role: "assistant", Content: turn.Answer, Timestamp: turn.CreatedAt, Sources: turn.Sources})
	}
	return messages
}

func turnsFromMessages(userID uint, conversationID string, messages []model.ChatMessage) ([]model.Conversation, error) {
	if userID == 0 || !validConversationID(conversationID) || len(messages)%2 != 0 {
		return nil, errors.New("history must contain complete user/assistant pairs")
	}
	turns := make([]model.Conversation, 0, len(messages)/2)
	for i := 0; i < len(messages); i += 2 {
		q, a := messages[i], messages[i+1]
		if q.Role != "user" || a.Role != "assistant" {
			return nil, errors.New("history roles must alternate user/assistant")
		}
		payload, err := json.Marshal(messages[i : i+2])
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(payload)
		turn := model.Conversation{UserID: userID, ConversationID: conversationID, TurnID: fmt.Sprintf("legacy-%d-%x", i/2+1, digest[:16]),
			TurnNo: uint64(i/2 + 1), Question: q.Content, Answer: a.Content, Sources: a.Sources, CreatedAt: q.Timestamp}
		if turn.CreatedAt.IsZero() {
			turn.CreatedAt = time.Now().UTC()
		}
		turn.CreatedAt = turn.CreatedAt.UTC().Truncate(time.Microsecond)
		if err := validateTurn(userID, conversationID, turn); err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, nil
}

func validConversationID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if c < '!' || c > '~' {
			return false // IDs use the database's case-sensitive ASCII columns.
		}
	}
	return true
}

func validateTurn(userID uint, conversationID string, turn model.Conversation) error {
	if userID == 0 || !validConversationID(conversationID) || !validConversationID(turn.TurnID) ||
		(turn.UserID != 0 && turn.UserID != userID) || (turn.ConversationID != "" && turn.ConversationID != conversationID) {
		return errors.New("invalid turn identity")
	}
	if strings.TrimSpace(turn.Question) == "" || strings.TrimSpace(turn.Answer) == "" || !utf8.ValidString(turn.Question) || !utf8.ValidString(turn.Answer) {
		return errors.New("turn must contain a valid complete question and answer")
	}
	if len(turn.Sources) > 24 {
		return errors.New("too many conversation sources")
	}
	seen := make(map[int]bool)
	for _, source := range turn.Sources {
		if source.Number < 1 || source.Number > 24 || seen[source.Number] || source.DocumentID == 0 || source.Version == "" || len(source.Version) > 64 || !utf8.ValidString(source.FileName) {
			return errors.New("invalid conversation source identity")
		}
		seen[source.Number] = true
	}
	return nil
}

func trimConversationHistory(messages []model.ChatMessage) []model.ChatMessage {
	if len(messages) > 20 {
		messages = messages[len(messages)-20:]
		for len(messages) > 0 && messages[0].Role == "assistant" {
			messages = messages[1:]
		}
	}
	return messages
}

func (r *conversationRepository) refreshCache(ctx context.Context, userID uint, conversationID string) {
	if r.redisClient == nil {
		return
	}
	cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	state, err := r.GetConversationState(cacheCtx, userID, conversationID)
	if err != nil {
		return
	}
	messages, err := r.GetConversationHistory(cacheCtx, conversationID)
	if err == nil {
		r.cacheHistory(cacheCtx, state, messages)
	}
}

func (r *conversationRepository) cacheHistory(ctx context.Context, state model.ConversationState, messages []model.ChatMessage) {
	if r.redisClient == nil {
		return
	}
	data, err := json.Marshal(trimConversationHistory(messages))
	if err != nil {
		return
	}
	cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	key := "conversation:" + state.ID
	_ = r.redisClient.Eval(cacheCtx, cacheConversationScript,
		[]string{fmt.Sprintf("user:%d:current_conversation", state.UserID), key, key + ":version"},
		state.ID, data, state.Version, int64(conversationTTL/time.Second)).Err()
}

package repository

import (
	"context"
	"errors"
	"pai-smart-go/internal/model"
	"strings"
	"time"
	"unicode/utf8"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrMemoryConflict = errors.New("memory version or key conflict")
var ErrMemoryLimit = errors.New("at most 50 active memories per user")
var ErrMemoryNotFound = errors.New("memory not found")

type UserMemoryRepository struct{ db *gorm.DB }

func NewUserMemoryRepository(db *gorm.DB) *UserMemoryRepository { return &UserMemoryRepository{db: db} }

// ListActive expires old memories under the same lock used by edits, so their
// statements cannot survive in an automatic summary or a recent-history window.
func (r *UserMemoryRepository) ListActive(ctx context.Context, userID uint) ([]model.UserMemory, error) {
	items, _, err := r.Snapshot(ctx, userID)
	return items, err
}

// Snapshot binds retrieved memories to the same state version under one SQL lock.
// Chat must compare this version before using the snapshot and CAS again on save.
func (r *UserMemoryRepository) Snapshot(ctx context.Context, userID uint) ([]model.UserMemory, uint64, error) {
	items := []model.UserMemory{}
	var version uint64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := lockMemoryState(tx, userID)
		if err != nil {
			return err
		}
		removed, err := expireUserMemories(tx, userID)
		if err != nil {
			return err
		}
		if removed {
			if err := resetMemoryContext(tx, state); err != nil {
				return err
			}
			state.Version++
		}
		version = state.Version
		return tx.Where("user_id = ?", userID).Order("updated_at DESC, id DESC").Find(&items).Error
	})
	return items, version, err
}

func (r *UserMemoryRepository) Create(ctx context.Context, userID uint, item model.UserMemory) (model.UserMemory, error) {
	if err := validateUserMemory(userID, item); err != nil {
		return model.UserMemory{}, err
	}
	item.ID, item.UserID, item.Version = 0, userID, 1
	item.CreatedAt, item.UpdatedAt = time.Time{}, time.Time{}
	if item.Keywords == nil {
		item.Keywords = []string{}
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := lockMemoryState(tx, userID)
		if err != nil {
			return err
		}
		if _, err := expireUserMemories(tx, userID); err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.UserMemory{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
			return err
		}
		if count >= 50 {
			return ErrMemoryLimit
		}
		if err := tx.Create(&item).Error; err != nil {
			return memoryWriteError(err)
		}
		return resetMemoryContext(tx, state)
	})
	return item, err
}

func (r *UserMemoryRepository) Update(ctx context.Context, userID, id uint, expectedVersion uint64, item model.UserMemory) (model.UserMemory, error) {
	if err := validateUserMemory(userID, item); err != nil {
		return model.UserMemory{}, err
	}
	if id == 0 || expectedVersion == 0 {
		return model.UserMemory{}, ErrMemoryNotFound
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := lockMemoryState(tx, userID)
		if err != nil {
			return err
		}
		if _, err := expireUserMemories(tx, userID); err != nil {
			return err
		}
		existing, err := findUserMemory(tx, userID, id, expectedVersion)
		if err != nil {
			return err
		}
		item.ID, item.UserID, item.Version = id, userID, existing.Version+1
		item.CreatedAt, item.UpdatedAt = existing.CreatedAt, time.Now().UTC().Truncate(time.Microsecond)
		if item.Keywords == nil {
			item.Keywords = []string{}
		}
		// Struct Updates handles the installed JSON serializer; Select also writes nil expiry.
		result := tx.Model(&item).Where("id = ? AND user_id = ? AND version = ?", id, userID, expectedVersion).
			Select("scope", "kind", "key", "content", "keywords", "expires_at", "version", "updated_at").Updates(&item)
		if result.Error != nil {
			return memoryWriteError(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrMemoryConflict
		}
		return resetMemoryContext(tx, state)
	})
	return item, err
}

func (r *UserMemoryRepository) Delete(ctx context.Context, userID, id uint, expectedVersion uint64) error {
	if userID == 0 || id == 0 || expectedVersion == 0 {
		return ErrMemoryNotFound
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := lockMemoryState(tx, userID)
		if err != nil {
			return err
		}
		if _, err := expireUserMemories(tx, userID); err != nil {
			return err
		}
		if _, err := findUserMemory(tx, userID, id, expectedVersion); err != nil {
			return err
		}
		result := tx.Where("id = ? AND user_id = ? AND version = ?", id, userID, expectedVersion).Delete(&model.UserMemory{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMemoryConflict
		}
		return resetMemoryContext(tx, state)
	})
}

func lockMemoryState(tx *gorm.DB, userID uint) (model.ConversationState, error) {
	var state model.ConversationState
	if userID == 0 {
		return state, errors.New("user ID is required")
	}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", userID).First(&state).Error
	return state, err
}

func expireUserMemories(tx *gorm.DB, userID uint) (bool, error) {
	result := tx.Where("user_id = ? AND expires_at IS NOT NULL AND expires_at <= ?", userID, time.Now().UTC()).Delete(&model.UserMemory{})
	return result.RowsAffected > 0, result.Error
}

func findUserMemory(tx *gorm.DB, userID, id uint, expectedVersion uint64) (model.UserMemory, error) {
	var item model.UserMemory
	err := tx.Where("id = ? AND user_id = ?", id, userID).First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return item, ErrMemoryNotFound
	}
	if err != nil {
		return item, err
	}
	if item.Version != expectedVersion {
		return item, ErrMemoryConflict
	}
	return item, nil
}

func resetMemoryContext(tx *gorm.DB, state model.ConversationState) error {
	// Preserve the full archive for explicit recall, but never automatically reuse
	// pre-edit conversation wording to resurrect a deleted or corrected memory.
	return updateConversationState(tx, state, map[string]interface{}{
		"summary": "{}", "summary_until": state.LastTurn, "context_after": state.LastTurn,
	})
}

func validateUserMemory(userID uint, item model.UserMemory) error {
	if userID == 0 || (item.UserID != 0 && item.UserID != userID) {
		return errors.New("invalid memory owner")
	}
	for _, value := range []string{item.Scope, item.Key} {
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 64 {
			return errors.New("memory scope and key must contain 1..64 characters")
		}
	}
	if item.Kind != "preference" && item.Kind != "project" && item.Kind != "decision" {
		return errors.New("invalid memory kind")
	}
	if strings.TrimSpace(item.Content) == "" || !utf8.ValidString(item.Content) || utf8.RuneCountInString(item.Content) > 500 {
		return errors.New("memory content must contain 1..500 characters")
	}
	if item.ExpiresAt != nil && !item.ExpiresAt.After(time.Now()) {
		return errors.New("memory expiry must be in the future")
	}
	if len(item.Keywords) > 16 {
		return errors.New("at most 16 memory keywords")
	}
	for _, keyword := range item.Keywords {
		if strings.TrimSpace(keyword) == "" || !utf8.ValidString(keyword) || utf8.RuneCountInString(keyword) > 64 {
			return errors.New("memory keywords must contain 1..64 characters")
		}
	}
	return nil
}

func memoryWriteError(err error) error {
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return ErrMemoryConflict
	}
	return err
}

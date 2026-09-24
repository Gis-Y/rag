package repository

import (
	"context"
	"errors"
	"time"

	"pai-smart-go/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type DocumentTaskOutboxRepository struct{ db *gorm.DB }

func NewDocumentTaskOutboxRepository(db *gorm.DB) *DocumentTaskOutboxRepository {
	return &DocumentTaskOutboxRepository{db: db}
}

func enqueueDocumentTask(tx *gorm.DB, documentID, userID uint, now time.Time) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.DocumentTaskOutbox{
		DocumentID: documentID, UserID: userID, AvailableAt: now,
	}).Error
}

// Enqueue is also used for explicit reprocessing. A duplicate pending request is coalesced.
func (r *DocumentTaskOutboxRepository) Enqueue(ctx context.Context, documentID, userID uint) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockProcessingDocument(tx, documentID, userID); err != nil {
			return err
		}
		return enqueueDocumentTask(tx, documentID, userID, time.Now())
	})
}

func pendingDelivery(db *gorm.DB, now time.Time) *gorm.DB {
	return db.Where("available_at <= ? AND (lease_until IS NULL OR lease_until <= ?)", now, now)
}

// Claim locks the upload first, matching merge/deletion lock order. The claim CAS
// allows several dispatchers without holding SQL locks during a Kafka call.
func (r *DocumentTaskOutboxRepository) Claim(ctx context.Context, token string, now time.Time) (*model.DocumentTaskOutbox, *model.FileUpload, error) {
	if len(token) != 32 {
		return nil, nil, errors.New("invalid outbox lease token")
	}
	var entry model.DocumentTaskOutbox
	err := pendingDelivery(r.db.WithContext(ctx), now).Order("available_at, document_id").First(&entry).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var file *model.FileUpload
	claimed := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		file, err = lockProcessingDocument(tx, entry.DocumentID, entry.UserID)
		if errors.Is(err, ErrDocumentChanged) {
			return tx.Where("document_id = ? AND user_id = ?", entry.DocumentID, entry.UserID).Delete(&model.DocumentTaskOutbox{}).Error
		}
		if err != nil {
			return err
		}
		until := now.Add(30 * time.Second)
		result := pendingDelivery(tx.Model(&model.DocumentTaskOutbox{}), now).
			Where("document_id = ? AND user_id = ?", entry.DocumentID, entry.UserID).
			Updates(map[string]any{"lease_token": token, "lease_until": until, "attempts": gorm.Expr("attempts + 1")})
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		entry.LeaseToken, entry.LeaseUntil, entry.Attempts = token, &until, entry.Attempts+1
		return nil
	})
	if err != nil || !claimed {
		return nil, nil, err
	}
	return &entry, file, nil
}

func (r *DocumentTaskOutboxRepository) Acknowledge(ctx context.Context, documentID, userID uint, token string) error {
	return r.db.WithContext(ctx).Where("document_id = ? AND user_id = ? AND lease_token = ?", documentID, userID, token).Delete(&model.DocumentTaskOutbox{}).Error
}

func (r *DocumentTaskOutboxRepository) Retry(ctx context.Context, documentID, userID uint, token, message string, availableAt time.Time) error {
	runes := []rune(message)
	if len(runes) > 1000 {
		message = string(runes[:1000])
	}
	return r.db.WithContext(ctx).Model(&model.DocumentTaskOutbox{}).
		Where("document_id = ? AND user_id = ? AND lease_token = ?", documentID, userID, token).
		Updates(map[string]any{"lease_token": "", "lease_until": nil, "available_at": availableAt, "last_error": message}).Error
}

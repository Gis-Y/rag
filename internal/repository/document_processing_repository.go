package repository

import (
	"context"
	"errors"
	"fmt"
	"pai-smart-go/internal/model"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrDocumentChanged = errors.New("document was removed, is not ready, or processing generation changed")

type DocumentProcessingRepository struct{ db *gorm.DB }

func NewDocumentProcessingRepository(db *gorm.DB) *DocumentProcessingRepository {
	return &DocumentProcessingRepository{db: db}
}

// Every write locks the upload row first. Deletion changes that same row before
// touching external storage, so an old task cannot resurrect a removed document.
func lockProcessingDocument(tx *gorm.DB, documentID, userID uint) (*model.FileUpload, error) {
	var file model.FileUpload
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", documentID, userID, 1).First(&file).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDocumentChanged
	}
	return &file, err
}

func (r *DocumentProcessingRepository) Begin(ctx context.Context, documentID, userID uint, md5, version string) (*model.FileUpload, error) {
	if documentID == 0 || userID == 0 || md5 == "" || version == "" || len(version) > 64 {
		return nil, errors.New("invalid document processing identity")
	}
	var file *model.FileUpload
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		file, err = lockProcessingDocument(tx, documentID, userID)
		if err != nil {
			return err
		}
		if !strings.EqualFold(file.FileMD5, md5) {
			return ErrDocumentChanged
		}
		state := model.DocumentProcessingState{DocumentID: documentID, UserID: userID, FileMD5: file.FileMD5, PendingVersion: version, Status: "parsing"}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "document_id"}}, DoUpdates: clause.AssignmentColumns([]string{"pending_version", "status", "error", "updated_at"})}).Create(&state).Error
	})
	return file, err
}

func requirePending(tx *gorm.DB, documentID, userID uint, version string) error {
	if _, err := lockProcessingDocument(tx, documentID, userID); err != nil {
		return err
	}
	var state model.DocumentProcessingState
	if err := tx.Where("document_id = ? AND user_id = ? AND pending_version = ?", documentID, userID, version).First(&state).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrDocumentChanged
		}
		return err
	}
	return nil
}

func (r *DocumentProcessingRepository) Stage(ctx context.Context, documentID, userID uint, version string, chunks []model.DocumentChunk) error {
	if len(chunks) == 0 {
		return errors.New("no document chunks")
	}
	for _, chunk := range chunks {
		if chunk.DocumentID != documentID || chunk.UserID != userID || chunk.Version != version || chunk.ChunkID == "" || len(chunk.ChunkID) > 128 || chunk.ChunkID != chunk.Data.ChunkID || chunk.ParentID != chunk.Data.ParentID {
			return errors.New("invalid staged chunk identity")
		}
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requirePending(tx, documentID, userID, version); err != nil {
			return err
		}
		if err := tx.CreateInBatches(chunks, 100).Error; err != nil {
			return err
		}
		return tx.Model(&model.DocumentProcessingState{}).Where("document_id = ? AND user_id = ? AND pending_version = ?", documentID, userID, version).Update("status", "embedding").Error
	})
}

func (r *DocumentProcessingRepository) Publish(ctx context.Context, documentID, userID uint, version string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requirePending(tx, documentID, userID, version); err != nil {
			return err
		}
		result := tx.Model(&model.DocumentProcessingState{}).Where("document_id = ? AND user_id = ? AND pending_version = ? AND status = ?", documentID, userID, version, "embedding").Updates(map[string]interface{}{"active_version": version, "pending_version": "", "status": "ready", "error": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrDocumentChanged
		}
		return nil
	})
}

func (r *DocumentProcessingRepository) Fail(ctx context.Context, documentID, userID uint, version, message string) error {
	// Only a matching pending attempt may record failure. The active generation is
	// deliberately left untouched, including when a newer attempt already began.
	runes := []rune(message)
	if len(runes) > 1000 {
		message = string(runes[:1000])
	}
	result := r.db.WithContext(ctx).Model(&model.DocumentProcessingState{}).Where("document_id = ? AND user_id = ? AND pending_version = ?", documentID, userID, version).Updates(map[string]interface{}{"status": "failed", "error": message})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrDocumentChanged
	}
	return nil
}

// DiscardVersion executes exact-version external cleanup while the processing
// state is locked, then removes staged SQL chunks. A published version is never
// passed to cleanup, including after a lost Publish response.
func (r *DocumentProcessingRepository) DiscardVersion(ctx context.Context, documentID, userID uint, version string, cleanup func() error) (bool, error) {
	if documentID == 0 || userID == 0 || version == "" || cleanup == nil {
		return false, errors.New("invalid document version cleanup")
	}
	discarded := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state model.DocumentProcessingState
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("document_id = ? AND user_id = ?", documentID, userID).First(&state).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && state.ActiveVersion == version {
			return nil
		}
		if err := cleanup(); err != nil {
			return err
		}
		if err := tx.Where("document_id = ? AND user_id = ? AND version = ?", documentID, userID, version).Delete(&model.DocumentChunk{}).Error; err != nil {
			return err
		}
		discarded = true
		return nil
	})
	return discarded, err
}

func (r *DocumentProcessingRepository) FindStates(ctx context.Context, documentIDs []uint) (map[uint]model.DocumentProcessingState, error) {
	states := make(map[uint]model.DocumentProcessingState, len(documentIDs))
	if len(documentIDs) == 0 {
		return states, nil
	}
	var rows []model.DocumentProcessingState
	err := r.db.WithContext(ctx).Table("document_processing_states AS s").Select("s.*").Joins("JOIN file_upload AS f ON f.id = s.document_id AND f.user_id = s.user_id AND f.status = 1").Where("s.document_id IN ?", documentIDs).Find(&rows).Error
	for _, state := range rows {
		states[state.DocumentID] = state
	}
	return states, err
}

// DeleteArtifacts requires a tombstone. Keeping the upload row until external
// cleanup succeeds makes deletion retryable without allowing new publications.
func (r *DocumentProcessingRepository) DeleteArtifacts(ctx context.Context, documentID, userID uint) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var file model.FileUpload
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", documentID, userID, 3).First(&file).Error; err != nil {
			return err
		}
		if err := tx.Where("document_id = ? AND user_id = ?", documentID, userID).Delete(&model.DocumentChunk{}).Error; err != nil {
			return err
		}
		return tx.Where("document_id = ? AND user_id = ?", documentID, userID).Delete(&model.DocumentProcessingState{}).Error
	})
}

func (r *DocumentProcessingRepository) FindParents(ctx context.Context, refs []model.ParentRef, userID uint, orgTags []string) ([]model.DocumentChunk, error) {
	if len(refs) == 0 {
		return []model.DocumentChunk{}, nil
	}
	if len(refs) > 1000 {
		return nil, fmt.Errorf("too many parent references: %d", len(refs))
	}
	query := r.db.WithContext(ctx).Table("document_chunks AS c").Select("c.*").Joins("JOIN file_upload AS f ON f.id = c.document_id AND f.user_id = c.user_id AND f.status = 1").Joins("JOIN document_processing_states AS s ON s.document_id = c.document_id AND s.user_id = c.user_id AND s.active_version = c.version").Where("c.is_parent = ?", true)
	access := r.db.Where("f.user_id = ? OR f.is_public = ?", userID, true)
	if len(orgTags) > 0 {
		access = access.Or("f.org_tag IN ?", orgTags)
	}
	query = query.Where(access)
	var requested *gorm.DB
	for _, ref := range refs {
		if ref.DocumentID == 0 || ref.Version == "" || ref.ParentID == "" {
			return nil, errors.New("invalid parent reference")
		}
		if requested == nil {
			requested = r.db.Where("c.document_id = ? AND c.version = ? AND c.chunk_id = ?", ref.DocumentID, ref.Version, ref.ParentID)
		} else {
			requested = requested.Or("c.document_id = ? AND c.version = ? AND c.chunk_id = ?", ref.DocumentID, ref.Version, ref.ParentID)
		}
	}
	var chunks []model.DocumentChunk
	err := query.Where(requested).Find(&chunks).Error
	return chunks, err
}

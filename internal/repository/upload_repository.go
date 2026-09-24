// Package repository 定义了与数据库进行数据交换的接口和实现。
package repository

import (
	"context"
	"errors"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"pai-smart-go/internal/model"
	"strconv"
	"strings"
	"time"
)

// UploadRepository 接口定义了文件上传相关的数据持久化操作。
type UploadRepository interface {
	// FileUpload operations
	CreateFileUploadRecord(record *model.FileUpload) error
	GetFileUploadRecord(fileMD5 string, userID uint) (*model.FileUpload, error)
	GetFileUploadByID(ctx context.Context, documentID uint) (*model.FileUpload, error)
	BeginMerge(ctx context.Context, documentID, userID uint, token string, expiresAt time.Time) (*model.FileUpload, error)
	CompleteMerge(ctx context.Context, documentID, userID uint, token string) error
	ReleaseMerge(ctx context.Context, documentID, userID uint, token string) (bool, error)
	UpdateFileUploadStatus(recordID uint, status int) error
	FindFilesByUserID(userID uint) ([]model.FileUpload, error)
	FindAccessibleFiles(ctx context.Context, userID uint, orgTags []string) ([]model.FileUpload, error)
	DeleteFileUploadByID(ctx context.Context, documentID, userID uint) error

	// FinalizeChunk serializes canonical object publication with merge/deletion.
	FinalizeChunk(ctx context.Context, record *model.ChunkInfo, publish func() error) error

	// Chunk status operations (Redis)
	GetUploadedChunksFromRedis(ctx context.Context, documentID, userID uint, totalChunks int) ([]int, error)
	DeleteUploadMark(ctx context.Context, documentID, userID uint) error
}

// uploadRepository 是 UploadRepository 接口的 GORM+Redis 实现。
type uploadRepository struct {
	db          *gorm.DB
	redisClient *redis.Client
}

// NewUploadRepository 创建一个新的 UploadRepository 实例。
func NewUploadRepository(db *gorm.DB, redisClient *redis.Client) UploadRepository {
	return &uploadRepository{db: db, redisClient: redisClient}
}

// getRedisUploadKey generates the redis key for upload status.
func (r *uploadRepository) getRedisUploadKey(documentID, userID uint) string {
	return "upload:" + strconv.FormatUint(uint64(userID), 10) + ":" + strconv.FormatUint(uint64(documentID), 10)
}

// CreateFileUploadRecord 在数据库中创建一个新的文件上传总记录。
func (r *uploadRepository) CreateFileUploadRecord(record *model.FileUpload) error {
	return r.db.Create(record).Error
}

// GetFileUploadRecord 根据文件 MD5 和用户 ID 检索文件上传记录。
func (r *uploadRepository) GetFileUploadRecord(fileMD5 string, userID uint) (*model.FileUpload, error) {
	var record model.FileUpload
	err := r.db.Where("file_md5 = ? AND user_id = ?", fileMD5, userID).First(&record).Error
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *uploadRepository) GetFileUploadByID(ctx context.Context, documentID uint) (*model.FileUpload, error) {
	var record model.FileUpload
	err := r.db.WithContext(ctx).Where("id = ?", documentID).First(&record).Error
	return &record, err
}

var ErrMergeInProgress = errors.New("document merge is already in progress")

func (r *uploadRepository) BeginMerge(ctx context.Context, documentID, userID uint, token string, expiresAt time.Time) (*model.FileUpload, error) {
	if documentID == 0 || userID == 0 || len(token) != 32 || !expiresAt.After(time.Now()) {
		return nil, errors.New("invalid merge identity or lease")
	}
	var record model.FileUpload
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", documentID, userID).First(&record).Error; err != nil {
			return err
		}
		if record.Status == 1 {
			return nil
		}
		if record.Status == 3 {
			return ErrDocumentChanged
		}
		if record.Status == 4 && record.MergeExpiresAt != nil && record.MergeExpiresAt.After(time.Now()) {
			return ErrMergeInProgress
		}
		if record.Status != 0 && record.Status != 2 && record.Status != 4 {
			return ErrDocumentChanged
		}
		if err := tx.Model(&record).Updates(map[string]any{"status": 4, "merge_token": token, "merge_expires_at": expiresAt}).Error; err != nil {
			return err
		}
		record.Status, record.MergeToken, record.MergeExpiresAt = 4, token, &expiresAt
		return nil
	})
	return &record, err
}

func (r *uploadRepository) CompleteMerge(ctx context.Context, documentID, userID uint, token string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record model.FileUpload
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", documentID, userID).First(&record).Error; err != nil {
			return err
		}
		if record.Status == 1 && record.MergeToken == token {
			return nil // A lost commit response must not publish a second task.
		}
		now := time.Now()
		if record.Status != 4 || record.MergeToken != token || record.MergeExpiresAt == nil || !record.MergeExpiresAt.After(now) {
			return ErrDocumentChanged
		}
		result := tx.Model(&model.FileUpload{}).Where("id = ? AND user_id = ? AND status = ? AND merge_token = ?", documentID, userID, 4, token).
			Updates(map[string]any{"status": 1, "merged_at": now, "merge_expires_at": nil})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrDocumentChanged
		}
		return enqueueDocumentTask(tx, documentID, userID, now)
	})
}

func (r *uploadRepository) ReleaseMerge(ctx context.Context, documentID, userID uint, token string) (bool, error) {
	result := r.db.WithContext(ctx).Model(&model.FileUpload{}).
		Where("id = ? AND user_id = ? AND status = ? AND merge_token = ?", documentID, userID, 4, token).
		Updates(map[string]any{"status": 0, "merge_token": "", "merge_expires_at": nil})
	return result.RowsAffected == 1, result.Error
}

// UpdateFileUploadStatus 更新指定文件上传记录的状态。
func (r *uploadRepository) UpdateFileUploadStatus(recordID uint, status int) error {
	query := r.db.Model(&model.FileUpload{}).Where("id = ?", recordID)
	if status != 3 {
		query = query.Where("status <> ?", 3)
	}
	result := query.Update("status", status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.Model(&model.FileUpload{}).Where("id = ? AND status = ?", recordID, status).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrDocumentChanged
		}
	}
	return nil
}

// FindFilesByUserID 查找指定用户上传的所有文件。
func (r *uploadRepository) FindFilesByUserID(userID uint) ([]model.FileUpload, error) {
	var files []model.FileUpload
	err := r.db.Where("user_id = ?", userID).Find(&files).Error
	return files, err
}

// FindAccessibleFiles 查找用户可访问的所有文件。
// 包括：用户自己的文件、公开文件、用户所属组织的文件。
func (r *uploadRepository) FindAccessibleFiles(ctx context.Context, userID uint, orgTags []string) ([]model.FileUpload, error) {
	var files []model.FileUpload
	access := r.db.Where("user_id = ?", userID).Or("is_public = ?", true)
	if len(orgTags) > 0 {
		access = access.Or("org_tag IN ?", orgTags)
	}
	err := r.db.WithContext(ctx).Where("status = ?", 1).Where(access).Find(&files).Error
	return files, err
}

func (r *uploadRepository) DeleteFileUploadByID(ctx context.Context, documentID, userID uint) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record model.FileUpload
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", documentID, userID, 3).First(&record).Error; err != nil {
			return err
		}
		if err := tx.Where("document_id = ? AND user_id = ?", documentID, userID).Delete(&model.ChunkInfo{}).Error; err != nil {
			return err
		}
		if err := tx.Where("document_id = ? AND user_id = ?", documentID, userID).Delete(&model.DocumentTaskOutbox{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND user_id = ? AND status = ?", documentID, userID, 3).Delete(&model.FileUpload{}).Error
	})
}

func (r *uploadRepository) FinalizeChunk(ctx context.Context, record *model.ChunkInfo, publish func() error) error {
	if record == nil || record.DocumentID == 0 || record.UserID == 0 || record.ChunkIndex < 0 || len(record.FileMD5) != 32 || len(record.ChunkMD5) != 32 || record.StoragePath == "" || publish == nil {
		return errors.New("invalid chunk publication")
	}
	if r.redisClient == nil {
		return errors.New("upload progress store is unavailable")
	}
	progressKey := r.getRedisUploadKey(record.DocumentID, record.UserID)
	progressOffset := int64(record.ChunkIndex)
	// ponytail: one upload-row lock serializes finalization; use per-chunk leases
	// only if parallel copy throughput becomes a measured bottleneck.
	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var file model.FileUpload
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", record.DocumentID, record.UserID, 0).First(&file).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrDocumentChanged
		}
		if err != nil {
			return err
		}
		if file.Status != 0 || !strings.EqualFold(file.FileMD5, record.FileMD5) {
			return ErrDocumentChanged
		}
		// Invalidate an earlier successful upload of the same chunk before replacing
		// its SQL identity. Merge cannot observe old progress for new bytes.
		if err := r.redisClient.SetBit(ctx, progressKey, progressOffset, 0).Err(); err != nil {
			return err
		}
		if err := tx.Where("document_id = ? AND user_id = ? AND chunk_index = ?", record.DocumentID, record.UserID, record.ChunkIndex).Delete(&model.ChunkInfo{}).Error; err != nil {
			return err
		}
		return tx.Create(record).Error
	}); err != nil {
		return err
	}

	// SQL is durable before the canonical object and completion bit are exposed.
	// Re-lock and match the exact record so a newer retransmission, merge or delete
	// cannot be overwritten by this attempt between the two phases.
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var file model.FileUpload
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", record.DocumentID, record.UserID, 0).First(&file).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrDocumentChanged
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(file.FileMD5, record.FileMD5) {
			return ErrDocumentChanged
		}
		var persisted model.ChunkInfo
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("document_id = ? AND user_id = ? AND chunk_index = ? AND file_md5 = ? AND chunk_md5 = ? AND storage_path = ?", record.DocumentID, record.UserID, record.ChunkIndex, record.FileMD5, record.ChunkMD5, record.StoragePath).
			First(&persisted).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrDocumentChanged
		}
		if err != nil {
			return err
		}
		if err := publish(); err != nil {
			return err
		}
		return r.redisClient.SetBit(ctx, progressKey, progressOffset, 1).Err()
	})
}

// GetUploadedChunksFromRedis retrieves the list of uploaded chunk indexes from Redis bitmap.
func (r *uploadRepository) GetUploadedChunksFromRedis(ctx context.Context, documentID, userID uint, totalChunks int) ([]int, error) {
	if totalChunks == 0 {
		return []int{}, nil
	}
	key := r.getRedisUploadKey(documentID, userID)
	bitmap, err := r.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return []int{}, nil // Key doesn't exist, no chunks uploaded
		}
		return nil, err
	}

	uploaded := make([]int, 0)
	for i := 0; i < totalChunks; i++ {
		byteIndex := i / 8
		bitIndex := i % 8
		if byteIndex < len(bitmap) && (bitmap[byteIndex]>>(7-bitIndex))&1 == 1 {
			uploaded = append(uploaded, i)
		}
	}
	return uploaded, nil
}

// DeleteUploadMark deletes the upload status key from Redis.
func (r *uploadRepository) DeleteUploadMark(ctx context.Context, documentID, userID uint) error {
	key := r.getRedisUploadKey(documentID, userID)
	return r.redisClient.Del(ctx, key).Err()
}

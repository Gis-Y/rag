// Package service 包含了应用的业务逻辑层。
package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/storage"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
)

var (
	ErrInvalidUpload  = errors.New("invalid upload")
	ErrUploadTooLarge = errors.New("upload exceeds document processing limit")
)

const (
	// DefaultChunkSize 定义了用于计算总分片数的默认分片大小 (5MB)，与 Java 版本保持一致。
	DefaultChunkSize = 5 * 1024 * 1024
)

// UploadService 接口定义了文件上传相关的业务操作。
type UploadService interface {
	CheckFile(ctx context.Context, fileMD5 string, userID uint) (bool, []int, error)
	UploadChunk(ctx context.Context, fileMD5, fileName string, totalSize int64, chunkIndex int, file io.Reader, userID uint, orgTag string, isPublic bool) (uploadedChunks []int, totalChunks int, err error)
	MergeChunks(ctx context.Context, fileMD5, fileName string, userID uint) (string, error)
	GetUploadStatus(ctx context.Context, fileMD5 string, userID uint) (fileName string, fileType string, uploadedChunks []int, totalChunks int, err error)
	GetSupportedFileTypes() (map[string]interface{}, error)
	FastUpload(ctx context.Context, fileMD5 string, userID uint) (bool, error)
}

type UploadObjectStore interface {
	Put(context.Context, string, io.Reader, int64) error
	Copy(context.Context, string, string) error
	Compose(context.Context, string, []string) error
	VerifyMD5(context.Context, string, string) error
	Remove(context.Context, string) error
	RemoveDocumentChunks(context.Context, uint, uint) error
	PresignedGetURL(context.Context, string, time.Duration) (string, error)
}

type uploadService struct {
	uploadRepo   repository.UploadRepository
	userRepo     repository.UserRepository
	objectStore  UploadObjectStore
	maxFileBytes int64
}

// NewUploadService 创建一个新的 UploadService 实例。
func NewUploadService(uploadRepo repository.UploadRepository, userRepo repository.UserRepository, objectStore UploadObjectStore, maxFileBytes int64) UploadService {
	if maxFileBytes <= 0 || maxFileBytes > 1<<30 {
		panic("upload requires the configured document processing size limit")
	}
	return &uploadService{
		uploadRepo:   uploadRepo,
		userRepo:     userRepo,
		objectStore:  objectStore,
		maxFileBytes: maxFileBytes,
	}
}

// CheckFile 检查文件是否已上传（秒传逻辑）。
func (s *uploadService) CheckFile(ctx context.Context, fileMD5 string, userID uint) (bool, []int, error) {
	log.Infof("[CheckFile] 开始秒传检查，文件MD5: %s, 用户ID: %d", fileMD5, userID)

	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			log.Infof("[CheckFile] 文件记录不存在，需要进行普通上传。文件MD5: %s", fileMD5)
			return false, nil, nil
		}
		log.Errorf("[CheckFile] 秒传检查失败：查询文件记录时出错, error: %v", err)
		return false, nil, err
	}

	if record.Status == 1 {
		log.Infof("[CheckFile] 文件已存在且状态为已完成，秒传成功。文件MD5: %s", fileMD5)
		return true, nil, nil
	}

	totalChunks := s.calculateTotalChunks(record.TotalSize)
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, record.ID, userID, totalChunks)
	if err != nil {
		log.Errorf("[CheckFile] 秒传检查失败：从Redis获取已上传分片列表时出错, error: %v", err)
		return false, nil, err
	}
	log.Infof("[CheckFile] 文件记录已存在但未完成，返回已上传的分片列表。文件MD5: %s, 已上传分片数: %d", fileMD5, len(uploadedIndexes))
	return false, uploadedIndexes, nil
}

// UploadChunk 处理单个分片的上传。
func (s *uploadService) UploadChunk(ctx context.Context, fileMD5, fileName string, totalSize int64, chunkIndex int, file io.Reader, userID uint, orgTag string, isPublic bool) ([]int, int, error) {
	if totalSize > s.maxFileBytes {
		return nil, 0, fmt.Errorf("%w: maximum %d bytes", ErrUploadTooLarge, s.maxFileBytes)
	}
	if totalSize <= 0 || chunkIndex < 0 || chunkIndex >= s.calculateTotalChunks(totalSize) || file == nil || userID == 0 {
		return nil, 0, fmt.Errorf("%w: invalid size, chunk index, owner or content", ErrInvalidUpload)
	}
	if _, err := hex.DecodeString(fileMD5); len(fileMD5) != 32 || err != nil || fileMD5 != strings.ToLower(fileMD5) {
		return nil, 0, fmt.Errorf("%w: expected lowercase MD5", ErrInvalidUpload)
	}
	if strings.TrimSpace(fileName) == "" || strings.ContainsAny(fileName, "/\\\x00\r\n") {
		return nil, 0, fmt.Errorf("%w: invalid filename", ErrInvalidUpload)
	}
	log.Infof("[UploadChunk] 开始上传分片，文件MD5: %s, 分片序号: %d, 用户ID: %d", fileMD5, chunkIndex, userID)

	// Every chunk is validated: uploads may arrive out of order.
	supportedTypes, _ := s.GetSupportedFileTypes()
	validType := false
	for _, ext := range supportedTypes["supportedExtensions"].([]string) {
		if strings.ToLower(filepath.Ext(fileName)) == ext {
			validType = true
			break
		}
	}
	if !validType {
		return nil, 0, fmt.Errorf("%w: unsupported file type", ErrInvalidUpload)
	}
	expectedSize := min(int64(DefaultChunkSize), totalSize-int64(chunkIndex)*DefaultChunkSize)
	content, err := io.ReadAll(io.LimitReader(file, expectedSize+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read upload chunk: %w", err)
	}
	if int64(len(content)) != expectedSize {
		return nil, 0, fmt.Errorf("%w: chunk length does not match declared size", ErrInvalidUpload)
	}

	// 1. 检查或创建 FileUpload 记录
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		log.Infof("[UploadChunk] 文件上传记录不存在，为文件MD5: %s 创建新记录", fileMD5)
		user, userErr := s.userRepo.FindByID(userID)
		if userErr != nil {
			return nil, 0, userErr
		}
		// 未指定时使用主组织；指定时必须是用户所属组织，不能信任客户端透传值。
		if orgTag == "" {
			orgTag = user.PrimaryOrg
		}
		if orgTag != "" && !hasOrgTag(user.OrgTags, orgTag) {
			return nil, 0, errors.New("user does not belong to this organization")
		}

		newRecord := &model.FileUpload{
			FileMD5:   fileMD5,
			FileName:  fileName,
			TotalSize: totalSize,
			Status:    0, // 上传中
			UserID:    userID,
			OrgTag:    orgTag,
			IsPublic:  isPublic, // 保存 isPublic 状态
		}
		if err := s.uploadRepo.CreateFileUploadRecord(newRecord); err != nil {
			log.Errorf("[UploadChunk] 创建文件上传记录失败, error: %v", err)
			return nil, 0, err
		}
		record = newRecord // use the new record for subsequent logic
	} else if err != nil {
		log.Errorf("[UploadChunk] 查询文件上传记录失败, error: %v", err)
		return nil, 0, err
	}

	if record.Status == 3 {
		return nil, 0, errors.New("文件正在删除，请完成删除后重新上传")
	}
	if record.TotalSize != totalSize || record.FileName != fileName {
		return nil, 0, fmt.Errorf("%w: chunk metadata does not match this upload", ErrInvalidUpload)
	}
	if record.Status == 1 {
		totalChunks := s.calculateTotalChunks(record.TotalSize)
		return completedChunkIndexes(totalChunks), totalChunks, nil
	}
	if record.Status == 4 {
		return nil, 0, repository.ErrMergeInProgress
	}

	// Every explicit upload is a replacement attempt. A Redis bit is progress,
	// not proof that the stored bytes are the bytes the client is retrying.
	var attemptRandom [16]byte
	if _, err := rand.Read(attemptRandom[:]); err != nil {
		return nil, 0, err
	}
	attemptToken := hex.EncodeToString(attemptRandom[:])
	attemptObject := storage.ChunkAttemptObjectKey(userID, record.ID, chunkIndex, attemptToken)
	objectName := storage.ChunkObjectKey(userID, record.ID, chunkIndex)
	err = s.objectStore.Put(ctx, attemptObject, bytes.NewReader(content), int64(len(content)))
	if err != nil {
		log.Errorf("[UploadChunk] 上传分片attempt到MinIO失败, objectName: %s, error: %v", attemptObject, err)
		return nil, 0, err
	}
	removeAttempt := func() error {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		return s.objectStore.Remove(cleanupCtx, attemptObject)
	}
	chunkDigest := md5.Sum(content)
	chunkRecord := &model.ChunkInfo{
		DocumentID:  record.ID,
		UserID:      userID,
		FileMD5:     fileMD5,
		ChunkIndex:  chunkIndex,
		ChunkMD5:    hex.EncodeToString(chunkDigest[:]),
		StoragePath: objectName, // 保存存储路径
	}
	err = s.uploadRepo.FinalizeChunk(ctx, chunkRecord, func() error {
		return s.objectStore.Copy(ctx, objectName, attemptObject)
	})
	if err != nil {
		cleanupErr := removeAttempt()
		log.Errorf("[UploadChunk] 发布分片失败, error: %v", err)
		return nil, 0, errors.Join(err, cleanupErr)
	}
	if err := removeAttempt(); err != nil {
		// A successful merge sweeps the whole document chunk prefix, including
		// abandoned private attempts, so this does not weaken publication safety.
		log.Warnf("[UploadChunk] 清理分片attempt失败, objectName: %s, error: %v", attemptObject, err)
	}

	// 6. 获取最新的已上传分片列表并计算总分片数
	totalChunks := s.calculateTotalChunks(record.TotalSize)
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, record.ID, userID, totalChunks)
	if err != nil {
		log.Errorf("[UploadChunk] 上传成功后从Redis获取最新分片列表失败, error: %v", err)
		return nil, 0, err
	}

	log.Infof("[UploadChunk] 分片上传成功。文件MD5: %s, 分片序号: %d, 总进度: %d/%d", fileMD5, chunkIndex, len(uploadedIndexes), totalChunks)
	return uploadedIndexes, totalChunks, nil
}

// MergeChunks 合并所有分片。
func (s *uploadService) MergeChunks(ctx context.Context, fileMD5, fileName string, userID uint) (objectURL string, resultErr error) {
	log.Infof("[MergeChunks] 开始合并文件分片，文件MD5: %s, 用户ID: %d", fileMD5, userID)
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	if err != nil {
		log.Errorf("[MergeChunks] 合并分片失败：获取文件记录时出错, error: %v", err)
		return "", err
	}

	// 1. 检查分片是否已全部上传 (Redis)，这是快速检查
	if record.Status == 3 {
		return "", errors.New("文件正在删除，不能合并")
	}
	if record.TotalSize > s.maxFileBytes {
		return "", fmt.Errorf("%w: maximum %d bytes", ErrUploadTooLarge, s.maxFileBytes)
	}
	if record.TotalSize <= 0 {
		return "", fmt.Errorf("%w: invalid total size", ErrInvalidUpload)
	}
	if fileName != record.FileName {
		return "", fmt.Errorf("%w: filename does not match upload", ErrInvalidUpload)
	}
	if record.Status == 1 {
		return s.completedUploadURL(ctx, record)
	}
	const mergeLease = 5 * time.Minute
	ctx, cancel := context.WithTimeout(ctx, mergeLease)
	defer cancel()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	record, err = s.uploadRepo.BeginMerge(ctx, record.ID, userID, token, time.Now().Add(mergeLease))
	if err != nil {
		return "", err
	}
	if record.Status == 1 {
		return s.completedUploadURL(ctx, record)
	}
	// This object is private to this attempt until CompleteMerge publishes its
	// token. Failed or stale attempts can never overwrite an already published raw.
	destObjectName := storage.DocumentObjectKey(userID, record.ID, token)
	completed := false
	defer func() {
		if completed {
			return
		}
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		released, err := s.uploadRepo.ReleaseMerge(cleanupCtx, record.ID, userID, token)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release merge attempt: %w", err))
			return
		}
		// A commit response can be lost. Only delete after CAS proves this token
		// was still an unpublished attempt; otherwise leave it for scoped cleanup.
		if released {
			if err := s.objectStore.Remove(cleanupCtx, destObjectName); err != nil {
				log.Warnf("清理未发布合并对象失败: document=%d error=%v", record.ID, err)
			}
		}
	}()
	totalChunks := s.calculateTotalChunks(record.TotalSize)
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, record.ID, userID, totalChunks)
	if err != nil {
		log.Errorf("[MergeChunks] 合并分片失败：从Redis检查分片完整性时出错, error: %v", err)
		return "", fmt.Errorf("failed to get uploaded chunks from redis: %w", err)
	}
	if len(uploadedIndexes) < totalChunks {
		log.Warnf("[MergeChunks] 拒绝合并请求：分片未完全上传。文件MD5: %s, 期望分片数: %d, 实际分片数: %d", fileMD5, totalChunks, len(uploadedIndexes))
		return "", fmt.Errorf("分片未全部上传，无法合并 (期望: %d, 实际: %d)", totalChunks, len(uploadedIndexes))
	}

	// 2. 根据分片数量选择合并策略
	sourceKeys := make([]string, 0, totalChunks)
	for i := 0; i < totalChunks; i++ {
		sourceKeys = append(sourceKeys, storage.ChunkObjectKey(userID, record.ID, i))
	}

	if totalChunks == 1 {
		err = s.objectStore.Copy(ctx, destObjectName, sourceKeys[0])
		if err != nil {
			log.Errorf("[MergeChunks] 单分片文件复制失败, error: %v", err)
			return "", fmt.Errorf("failed to copy single chunk object: %w", err)
		}
		log.Infof("[MergeChunks] 单分片文件复制成功。")
	} else {
		err = s.objectStore.Compose(ctx, destObjectName, sourceKeys)
		if err != nil {
			log.Errorf("[MergeChunks] 多分片文件合并失败, error: %v", err)
			return "", err
		}
		log.Infof("[MergeChunks] 多分片文件合并成功。")
	}
	if err := s.objectStore.VerifyMD5(ctx, destObjectName, fileMD5); err != nil {
		if errors.Is(err, storage.ErrMD5Mismatch) {
			resetCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			resetErr := s.uploadRepo.DeleteUploadMark(resetCtx, record.ID, userID)
			stop()
			if resetErr != nil {
				return "", errors.Join(fmt.Errorf("合并文件校验失败: %w", err), fmt.Errorf("重置错误分片进度失败: %w", resetErr))
			}
		}
		return "", fmt.Errorf("合并文件校验失败: %w", err)
	}
	// Publish the verified raw identity and its durable task in one SQL transaction.
	if err := s.uploadRepo.CompleteMerge(ctx, record.ID, userID, token); err != nil {
		return "", err
	}
	completed = true
	return s.completedUploadURL(ctx, record)
}

func (s *uploadService) completedUploadURL(ctx context.Context, record *model.FileUpload) (string, error) {
	cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer stop()
	cleanupErr := errors.Join(
		s.uploadRepo.DeleteUploadMark(cleanupCtx, record.ID, record.UserID),
		s.objectStore.RemoveDocumentChunks(cleanupCtx, record.UserID, record.ID),
	)
	if cleanupErr != nil {
		return "", fmt.Errorf("清理已合并分片失败，可重试: %w", cleanupErr)
	}
	return s.objectStore.PresignedGetURL(ctx, storage.DocumentObjectKey(record.UserID, record.ID, record.MergeToken), time.Hour)
}

// GetUploadStatus 获取文件的上传状态。
func (s *uploadService) GetUploadStatus(ctx context.Context, fileMD5 string, userID uint) (string, string, []int, int, error) {
	log.Infof("[GetUploadStatus] 开始获取文件上传状态。文件MD5: %s", fileMD5)
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	if err != nil {
		log.Errorf("[GetUploadStatus] 获取文件上传状态失败：查询文件记录时出错, error: %v", err)
		return "", "", nil, 0, err
	}

	totalChunks := s.calculateTotalChunks(record.TotalSize)
	if record.Status == 1 {
		return record.FileName, getFileType(record.FileName), completedChunkIndexes(totalChunks), totalChunks, nil
	}
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, record.ID, userID, totalChunks)
	if err != nil {
		log.Errorf("[GetUploadStatus] 获取文件上传状态失败：从Redis获取已上传分片列表时出错, error: %v", err)
		return "", "", nil, 0, err
	}

	fileType := getFileType(record.FileName)
	log.Infof("[GetUploadStatus] 成功获取文件上传状态。文件MD5: %s", fileMD5)
	return record.FileName, fileType, uploadedIndexes, totalChunks, nil
}

// GetSupportedFileTypes 返回系统支持的文件类型。
func (s *uploadService) GetSupportedFileTypes() (map[string]interface{}, error) {
	log.Info("[GetSupportedFileTypes] 开始获取系统支持的文件类型")
	// 在 Go 中，这些通常是硬编码的，因为它们与编译后的代码能力相关。
	typeMapping := map[string]string{
		".pdf":  "PDF文档",
		".doc":  "Word文档",
		".docx": "Word文档",
		".xls":  "Excel表格",
		".xlsx": "Excel表格",
		".ppt":  "PowerPoint演示文稿",
		".pptx": "PowerPoint演示文稿",
		".txt":  "文本文件",
		".md":   "Markdown文档",
	}

	supportedExtensions := make([]string, 0, len(typeMapping))
	supportedTypes := make([]string, 0, len(typeMapping))
	// Use a map to handle unique types like "Word文档"
	uniqueTypes := make(map[string]struct{})

	for ext, t := range typeMapping {
		supportedExtensions = append(supportedExtensions, ext)
		if _, exists := uniqueTypes[t]; !exists {
			uniqueTypes[t] = struct{}{}
			supportedTypes = append(supportedTypes, t)
		}
	}

	description := "系统支持的文档类型文件，这些文件可以被解析并进行向量化处理"

	data := map[string]interface{}{
		"supportedExtensions": supportedExtensions,
		"supportedTypes":      supportedTypes,
		"description":         description,
		"maxFileBytes":        s.maxFileBytes,
	}
	log.Info("[GetSupportedFileTypes] 成功获取系统支持的文件类型。")
	return data, nil
}

// FastUpload provides a dedicated check for fast upload.
func (s *uploadService) FastUpload(ctx context.Context, fileMD5 string, userID uint) (bool, error) {
	log.Infof("[FastUpload] 开始秒传（快速上传）检查。文件MD5: %s", fileMD5)
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			log.Info("[FastUpload] 秒传检查：文件记录不存在，无法秒传。")
			return false, nil
		}
		log.Errorf("[FastUpload] 秒传检查失败：查询数据库时出错, error: %v", err)
		return false, err
	}
	log.Infof("[FastUpload] 秒传检查：文件记录已存在，状态为 %d。", record.Status)
	return record.Status == 1, nil
}

// calculateTotalChunks 根据文件总大小和默认分片大小计算总分片数。
func (s *uploadService) calculateTotalChunks(totalSize int64) int {
	if totalSize <= 0 {
		return 0
	}
	return int((totalSize-1)/DefaultChunkSize + 1)
}

func completedChunkIndexes(total int) []int {
	indexes := make([]int, total)
	for i := range indexes {
		indexes[i] = i
	}
	return indexes
}

// getFileType 根据文件名推断文件类型描述 (private helper)
func getFileType(fileName string) string {
	if fileName == "" {
		return "未知类型"
	}
	parts := strings.Split(fileName, ".")
	if len(parts) < 2 {
		return "未知类型"
	}
	ext := "." + strings.ToLower(parts[len(parts)-1])

	typeMapping := map[string]string{
		".pdf":  "PDF文档",
		".doc":  "Word文档",
		".docx": "Word文档",
		".xls":  "Excel表格",
		".xlsx": "Excel表格",
		".ppt":  "PowerPoint演示文稿",
		".pptx": "PowerPoint演示文稿",
		".txt":  "文本文件",
		".md":   "Markdown文档",
	}
	if t, ok := typeMapping[ext]; ok {
		return t
	}
	return strings.ToUpper(ext[1:]) + "文件"
}

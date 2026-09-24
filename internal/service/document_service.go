// Package service 包含业务逻辑。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/storage"
	"strings"
	"time"
)

// FileUploadDTO 是显式 API DTO，不嵌入 ORM 模型。
type FileUploadDTO struct {
	ID        uint       `json:"id"`
	FileMD5   string     `json:"fileMd5"`
	FileName  string     `json:"fileName"`
	TotalSize int64      `json:"totalSize"`
	Status    int        `json:"status"`
	UserID    uint       `json:"userId"`
	OrgTag    string     `json:"orgTag"`
	IsPublic  bool       `json:"isPublic"`
	CreatedAt time.Time  `json:"createdAt"`
	MergedAt  *time.Time `json:"mergedAt"`
}

// UploadedFileDTO 保持上传列表原有的组织名称字段。
type UploadedFileDTO struct {
	FileUploadDTO
	OrgTagName string `json:"orgTagName"`
}

type DownloadInfoDTO struct {
	DocumentID  uint   `json:"documentId"`
	Version     string `json:"version"`
	FileName    string `json:"fileName"`
	DownloadURL string `json:"downloadUrl"`
	FileSize    int64  `json:"fileSize"`
}

type PreviewInfoDTO struct {
	DocumentID uint   `json:"documentId"`
	Version    string `json:"version"`
	FileName   string `json:"fileName"`
	Content    string `json:"content"`
	FileSize   int64  `json:"fileSize"`
}

type DocumentService interface {
	ListAccessibleFiles(ctx context.Context, user *model.User) ([]FileUploadDTO, error)
	ListUploadedFiles(userID uint) ([]UploadedFileDTO, error)
	DeleteDocument(ctx context.Context, documentID uint, user *model.User) error
	GenerateDownloadURL(ctx context.Context, documentID uint, version string, user *model.User) (*DownloadInfoDTO, error)
	GetFilePreviewContent(ctx context.Context, documentID uint, version string, user *model.User) (*PreviewInfoDTO, error)
}

type documentService struct {
	uploadRepo  repository.UploadRepository
	orgTagRepo  repository.OrgTagRepository
	userService UserService
	objectStore *storage.Client
	processing  DocumentProcessingOptions
}

type DocumentIndexCleaner interface {
	DeleteDocument(context.Context, uint, uint) error
}

type DocumentProcessingOptions struct {
	Repository DocumentStateRepository
	Index      DocumentIndexCleaner
}

type DocumentStateRepository interface {
	FindStates(context.Context, []uint) (map[uint]model.DocumentProcessingState, error)
	DeleteArtifacts(context.Context, uint, uint) error
}

func NewDocumentService(uploadRepo repository.UploadRepository, orgTagRepo repository.OrgTagRepository, userService UserService, objectStore *storage.Client, options DocumentProcessingOptions) DocumentService {
	if uploadRepo == nil || orgTagRepo == nil || userService == nil || objectStore == nil || options.Repository == nil || options.Index == nil {
		panic("document service requires repositories, user access, storage and index cleanup")
	}
	return &documentService{
		uploadRepo:  uploadRepo,
		orgTagRepo:  orgTagRepo,
		userService: userService,
		objectStore: objectStore,
		processing:  options,
	}
}

func (s *documentService) ListAccessibleFiles(ctx context.Context, user *model.User) ([]FileUploadDTO, error) {
	files, err := s.findAccessibleFiles(ctx, user)
	if err != nil {
		return nil, err
	}
	return mapFileUploads(files), nil
}

func (s *documentService) ListUploadedFiles(userID uint) ([]UploadedFileDTO, error) {
	files, err := s.uploadRepo.FindFilesByUserID(userID)
	if err != nil {
		return nil, err
	}
	return s.mapUploadedFiles(files)
}

func (s *documentService) DeleteDocument(ctx context.Context, documentID uint, user *model.User) error {
	if documentID == 0 || user == nil || user.ID == 0 {
		return errors.New("无效的文档或用户身份")
	}
	record, err := s.uploadRepo.GetFileUploadByID(ctx, documentID)
	if err != nil {
		return errors.New("文件不存在或无权删除")
	}
	if record == nil || record.ID != documentID || (record.UserID != user.ID && user.Role != "ADMIN") {
		return errors.New("文件不存在或无权删除")
	}

	// Tombstone before any external cleanup; a retry can continue from status 3.
	if err := s.uploadRepo.UpdateFileUploadStatus(record.ID, 3); err != nil {
		return err
	}
	if err := s.objectStore.RemoveDocumentUpload(ctx, record.UserID, record.ID); err != nil {
		return fmt.Errorf("删除文件对象失败: %w", err)
	}
	if err := s.processing.Index.DeleteDocument(ctx, record.ID, record.UserID); err != nil {
		return fmt.Errorf("删除文档索引失败，可重试: %w", err)
	}
	if err := s.objectStore.RemoveDocumentIR(ctx, record.UserID, record.ID); err != nil {
		return fmt.Errorf("删除文档IR失败，可重试: %w", err)
	}
	if err := s.processing.Repository.DeleteArtifacts(ctx, record.ID, record.UserID); err != nil {
		return err
	}
	if err := s.uploadRepo.DeleteUploadMark(ctx, record.ID, record.UserID); err != nil {
		return err
	}
	return s.uploadRepo.DeleteFileUploadByID(ctx, record.ID, record.UserID)
}

func (s *documentService) GenerateDownloadURL(ctx context.Context, documentID uint, version string, user *model.User) (*DownloadInfoDTO, error) {
	targetFile, active, err := s.findPublishedDocument(ctx, user, documentID, version)
	if err != nil {
		return nil, err
	}
	objectKey := storage.DocumentObjectKey(targetFile.UserID, targetFile.ID, targetFile.MergeToken)
	presignedURL, err := s.objectStore.PresignedGetURL(ctx, objectKey, time.Hour)
	if err != nil {
		return nil, err
	}
	currentUser, err := s.reloadAccessUser(ctx, user)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.findPublishedDocument(ctx, currentUser, documentID, active); err != nil {
		return nil, err
	}
	return &DownloadInfoDTO{DocumentID: targetFile.ID, Version: active, FileName: targetFile.FileName, DownloadURL: presignedURL, FileSize: targetFile.TotalSize}, nil
}

func (s *documentService) GetFilePreviewContent(ctx context.Context, documentID uint, version string, user *model.User) (*PreviewInfoDTO, error) {
	targetFile, active, err := s.findPublishedDocument(ctx, user, documentID, version)
	if err != nil {
		return nil, err
	}
	object, err := s.objectStore.Get(ctx, storage.DocumentIRObjectKey(targetFile.UserID, targetFile.ID, active))
	if err != nil {
		return nil, err
	}
	defer object.Close()

	content, err := readDocumentPreview(object, targetFile.ID, active)
	if err != nil {
		return nil, err
	}
	// Do not return a stale artifact if deletion, publication or access changed
	// while object storage was serving it. Preview never reparses the raw upload.
	currentUser, err := s.reloadAccessUser(ctx, user)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.findPublishedDocument(ctx, currentUser, documentID, active); err != nil {
		return nil, err
	}
	return &PreviewInfoDTO{DocumentID: targetFile.ID, Version: active, FileName: targetFile.FileName, Content: content, FileSize: targetFile.TotalSize}, nil
}

func readDocumentPreview(reader io.Reader, documentID uint, version string) (string, error) {
	// The parser response is bounded to 64 MiB; allow its small identity envelope.
	const maxArtifactBytes = (64 << 20) + 4096
	data, err := io.ReadAll(io.LimitReader(reader, maxArtifactBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxArtifactBytes {
		return "", errors.New("document IR exceeds preview limit")
	}
	var artifact struct {
		DocumentID uint   `json:"document_id"`
		Version    string `json:"version"`
		Document   struct {
			SchemaVersion string `json:"schema_version"`
			IR            struct {
				Blocks []struct {
					Text         string `json:"text"`
					TableID      string `json:"table_id"`
					TableCaption string `json:"table_caption"`
				} `json:"blocks"`
			} `json:"ir"`
		} `json:"document"`
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		return "", fmt.Errorf("invalid document IR: %w", err)
	}
	if artifact.DocumentID != documentID || artifact.Version != version || artifact.Document.SchemaVersion != "document-v1" {
		return "", errors.New("document IR identity does not match active version")
	}
	var content strings.Builder
	captions := make(map[string]bool)
	for _, block := range artifact.Document.IR.Blocks {
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		if block.TableCaption != "" && !captions[block.TableID] {
			content.WriteString(block.TableCaption + "\n")
			captions[block.TableID] = true
		}
		content.WriteString(block.Text)
	}
	if content.Len() == 0 {
		return "", errors.New("document IR has no preview text")
	}
	return content.String(), nil
}

func (s *documentService) findAccessibleFiles(ctx context.Context, user *model.User) ([]model.FileUpload, error) {
	tags, err := s.userService.GetUserEffectiveOrgTags(ctx, user)
	if err != nil {
		return nil, err
	}
	return s.uploadRepo.FindAccessibleFiles(ctx, user.ID, tags)
}

func (s *documentService) reloadAccessUser(ctx context.Context, expected *model.User) (*model.User, error) {
	if expected == nil || expected.ID == 0 || strings.TrimSpace(expected.Username) == "" {
		return nil, errors.New("无效的用户身份")
	}
	current, err := s.userService.GetProfile(ctx, expected.Username)
	if err != nil {
		return nil, err
	}
	if current == nil || current.ID != expected.ID {
		return nil, errors.New("用户身份已变更")
	}
	return current, nil
}

func (s *documentService) findPublishedDocument(ctx context.Context, user *model.User, documentID uint, version string) (*model.FileUpload, string, error) {
	if documentID == 0 || user == nil || user.ID == 0 || len(version) > 64 {
		return nil, "", errors.New("无效的文档身份")
	}
	files, err := s.findAccessibleFiles(ctx, user)
	if err != nil {
		return nil, "", err
	}
	for i := range files {
		if files[i].ID == documentID && files[i].Status == 1 {
			states, err := s.processing.Repository.FindStates(ctx, []uint{documentID})
			if err != nil {
				return nil, "", err
			}
			state, exists := states[documentID]
			if !exists || state.DocumentID != documentID || state.UserID != files[i].UserID || state.ActiveVersion == "" || (version != "" && state.ActiveVersion != version) {
				return nil, "", repository.ErrDocumentChanged
			}
			return &files[i], state.ActiveVersion, nil
		}
	}
	return nil, "", errors.New("文件不存在或无权访问")
}

func (s *documentService) mapUploadedFiles(files []model.FileUpload) ([]UploadedFileDTO, error) {
	if len(files) == 0 {
		return []UploadedFileDTO{}, nil
	}
	tagIDs := make(map[string]struct{})
	for _, file := range files {
		if file.OrgTag != "" {
			tagIDs[file.OrgTag] = struct{}{}
		}
	}
	ids := make([]string, 0, len(tagIDs))
	for id := range tagIDs {
		ids = append(ids, id)
	}
	tags, err := s.orgTagRepo.FindBatchByIDs(ids)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(tags))
	for _, tag := range tags {
		names[tag.TagID] = tag.Name
	}

	dtos := make([]UploadedFileDTO, 0, len(files))
	for _, file := range mapFileUploads(files) {
		dtos = append(dtos, UploadedFileDTO{FileUploadDTO: file, OrgTagName: names[file.OrgTag]})
	}
	return dtos, nil
}

func mapFileUploads(files []model.FileUpload) []FileUploadDTO {
	dtos := make([]FileUploadDTO, 0, len(files))
	for _, file := range files {
		dtos = append(dtos, FileUploadDTO{
			ID: file.ID, FileMD5: file.FileMD5, FileName: file.FileName,
			TotalSize: file.TotalSize, Status: file.Status, UserID: file.UserID,
			OrgTag: file.OrgTag, IsPublic: file.IsPublic,
			CreatedAt: file.CreatedAt, MergedAt: file.MergedAt,
		})
	}
	return dtos
}

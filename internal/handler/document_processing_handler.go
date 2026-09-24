package handler

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/token"
)

type DocumentProcessingStateReader interface {
	FindStates(context.Context, []uint) (map[uint]model.DocumentProcessingState, error)
}

type DocumentTaskQueue interface {
	Enqueue(context.Context, uint, uint) error
}

type DocumentProcessingHandler struct {
	uploads repository.UploadRepository
	states  DocumentProcessingStateReader
	queue   DocumentTaskQueue
	users   service.UserService
}

func NewDocumentProcessingHandler(uploads repository.UploadRepository, states DocumentProcessingStateReader, queue DocumentTaskQueue, users service.UserService) *DocumentProcessingHandler {
	return &DocumentProcessingHandler{uploads: uploads, states: states, queue: queue, users: users}
}

func (h *DocumentProcessingHandler) Status(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	file, state, ok := h.loadDocumentState(ctx, c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "message": "success", "data": gin.H{
		"documentId": file.ID, "fileMd5": file.FileMD5, "fileName": file.FileName,
		"status": state.Status, "activeVersion": state.ActiveVersion, "pendingVersion": state.PendingVersion,
		"searchable": state.ActiveVersion != "", "error": state.Error, "updatedAt": state.UpdatedAt,
	}})
}

func (h *DocumentProcessingHandler) Retry(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	file, state, ok := h.loadDocumentState(ctx, c)
	if !ok {
		return
	}
	// This endpoint has no client-controlled task fields. Empty bodies and {} are
	// accepted; reject invented paths, owners, versions and ACLs instead of ignoring them.
	if c.Request.Body != nil {
		decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1024))
		decoder.DisallowUnknownFields()
		var input struct{}
		err := decoder.Decode(&input)
		if (err != nil && err != io.EOF) || (err == nil && decoder.Decode(new(any)) != io.EOF) {
			processingHTTPError(c, http.StatusBadRequest, "重处理请求不接受文档、路径、所有者或权限参数", nil)
			return
		}
	}
	if processingInProgress(state, time.Now()) {
		processingHTTPError(c, http.StatusConflict, "文档正在处理中，请稍后查询状态", nil)
		return
	}
	// Persist the server-selected identity before acknowledging; the dispatcher
	// retries broker failures independently of this HTTP request.
	if err := h.queue.Enqueue(ctx, file.ID, file.UserID); err != nil {
		processingHTTPError(c, http.StatusServiceUnavailable, "任务入队失败，请重试", err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"code": http.StatusAccepted, "message": "任务已入队，尚未处理完成，请查询处理状态", "data": gin.H{
		"documentId": file.ID, "fileMd5": file.FileMD5, "status": "queued", "activeVersion": state.ActiveVersion,
	}})
}

func processingInProgress(state model.DocumentProcessingState, now time.Time) bool {
	if state.PendingVersion == "" || !state.UpdatedAt.After(now.Add(-5*time.Minute)) {
		return false
	}
	switch state.Status {
	case "queued", "parsing", "normalizing", "chunking", "embedding", "indexing":
		return true
	default:
		return false
	}
}

func (h *DocumentProcessingHandler) loadDocumentState(ctx context.Context, c *gin.Context) (*model.FileUpload, model.DocumentProcessingState, bool) {
	var empty model.DocumentProcessingState
	value, _ := c.Get("claims")
	claims, ok := value.(*token.CustomClaims)
	if !ok || claims == nil || claims.UserID == 0 || claims.Username == "" {
		processingHTTPError(c, http.StatusUnauthorized, "请先登录", nil)
		return nil, empty, false
	}
	user, err := h.users.GetProfile(ctx, claims.Username)
	if err != nil {
		processingHTTPError(c, http.StatusServiceUnavailable, "无法读取用户信息", err)
		return nil, empty, false
	}
	if user == nil || user.ID != claims.UserID {
		processingHTTPError(c, http.StatusUnauthorized, "用户身份已失效", nil)
		return nil, empty, false
	}
	ownerID := user.ID
	if rawOwner, supplied := c.GetQuery("userId"); supplied {
		if user.Role != "ADMIN" {
			processingHTTPError(c, http.StatusForbidden, "没有权限指定文件所有者", nil)
			return nil, empty, false
		}
		parsed, err := strconv.ParseUint(rawOwner, 10, 32)
		if err != nil || parsed == 0 {
			processingHTTPError(c, http.StatusBadRequest, "无效的用户 ID", nil)
			return nil, empty, false
		}
		ownerID = uint(parsed)
	}
	md5 := c.Param("fileMd5")
	if _, err := hex.DecodeString(md5); len(md5) != 32 || err != nil {
		processingHTTPError(c, http.StatusBadRequest, "无效的文件 MD5", nil)
		return nil, empty, false
	}
	// ponytail: reuse the existing context-aware read, then require exact ownership;
	// add a scoped single-record read when owners have large document inventories.
	files, err := h.uploads.FindAccessibleFiles(ctx, ownerID, nil)
	if err != nil {
		processingHTTPError(c, http.StatusServiceUnavailable, "无法读取文档", err)
		return nil, empty, false
	}
	var file *model.FileUpload
	for i := range files {
		if files[i].UserID == ownerID && files[i].FileMD5 == md5 && files[i].ID != 0 && files[i].Status == 1 {
			file = &files[i]
			break
		}
	}
	if file == nil {
		processingHTTPError(c, http.StatusNotFound, "文档不存在、未上传完成或已删除", nil)
		return nil, empty, false
	}
	states, err := h.states.FindStates(ctx, []uint{file.ID})
	if err != nil {
		processingHTTPError(c, http.StatusServiceUnavailable, "无法读取文档处理状态", err)
		return nil, empty, false
	}
	state, exists := states[file.ID]
	if !exists {
		state = model.DocumentProcessingState{DocumentID: file.ID, UserID: file.UserID, FileMD5: file.FileMD5, Status: "not_processed"}
	} else if state.DocumentID != file.ID || state.UserID != file.UserID || state.FileMD5 != file.FileMD5 {
		processingHTTPError(c, http.StatusServiceUnavailable, "文档处理状态不一致，请刷新后重试", nil)
		return nil, empty, false
	}
	if err := ctx.Err(); err != nil {
		processingHTTPError(c, http.StatusServiceUnavailable, "读取文档状态超时，请重试", err)
		return nil, empty, false
	}
	return file, state, true
}

func processingHTTPError(c *gin.Context, status int, message string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		status, message = http.StatusGatewayTimeout, "请求超时，请重试"
	} else if errors.Is(err, context.Canceled) {
		status, message = http.StatusRequestTimeout, "请求已取消"
	}
	c.JSON(status, gin.H{"code": status, "message": message, "data": nil})
}

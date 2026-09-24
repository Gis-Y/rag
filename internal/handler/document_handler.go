// Package handler 包含了处理 HTTP 请求的控制器逻辑。
package handler

import (
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/token"
	"strconv"
)

// DocumentHandler 负责处理所有与文档管理相关的 API 请求。
type DocumentHandler struct {
	docService  service.DocumentService
	userService service.UserService
}

// NewDocumentHandler 创建一个新的 DocumentHandler 实例。
func NewDocumentHandler(docService service.DocumentService, userService service.UserService) *DocumentHandler {
	return &DocumentHandler{
		docService:  docService,
		userService: userService,
	}
}

// ListAccessibleFiles 处理获取可访问文件列表的请求。
func (h *DocumentHandler) ListAccessibleFiles(c *gin.Context) {
	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	files, err := h.docService.ListAccessibleFiles(c.Request.Context(), user)
	if err != nil {
		log.Error("ListAccessibleFiles: failed", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取文件列表失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "获取可访问文件列表成功",
		"data":    files,
	})
}

// ListUploadedFiles 处理获取用户已上传文件列表的请求。
func (h *DocumentHandler) ListUploadedFiles(c *gin.Context) {
	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	files, err := h.docService.ListUploadedFiles(user.ID)
	if err != nil {
		log.Error("ListUploadedFiles: failed", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取文件列表失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "获取用户上传文件列表成功",
		"data":    files,
	})
}

// DeleteDocument 处理删除文档的请求。
func (h *DocumentHandler) DeleteDocument(c *gin.Context) {
	id, parseErr := strconv.ParseUint(c.Param("documentId"), 10, 32)
	if parseErr != nil || id == 0 || c.Query("userId") != "" || c.Query("fileMd5") != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "删除必须使用文档 ID，所有者由服务端校验"})
		return
	}

	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	err = h.docService.DeleteDocument(c.Request.Context(), uint(id), user)
	if err != nil {
		log.Warnf("DeleteDocument: failed for user %s, document %d, err: %v", user.Username, id, err)
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "文档删除成功",
	})
}

// GenerateDownloadURL 处理生成文件下载链接的请求。
func (h *DocumentHandler) GenerateDownloadURL(c *gin.Context) {
	documentID, version, ok := documentRequestIdentity(c)
	if !ok {
		return
	}

	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	downloadInfo, err := h.docService.GenerateDownloadURL(c.Request.Context(), documentID, version, user)
	if err != nil {
		documentAccessError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "文件下载链接生成成功",
		"data":    downloadInfo,
	})
}

// PreviewFile 处理获取文件预览内容的请求。
func (h *DocumentHandler) PreviewFile(c *gin.Context) {
	documentID, version, ok := documentRequestIdentity(c)
	if !ok {
		return
	}

	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	previewInfo, err := h.docService.GetFilePreviewContent(c.Request.Context(), documentID, version, user)
	if err != nil {
		documentAccessError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "文件预览内容获取成功",
		"data":    previewInfo,
	})
}

// getUserFromContext 是一个辅助函数，用于从 Gin 上下文中获取完整的用户模型。
func (h *DocumentHandler) getUserFromContext(c *gin.Context) (*model.User, error) {
	claimsValue, _ := c.Get("claims")
	claims, ok := claimsValue.(*token.CustomClaims)
	if !ok || claims == nil || claims.UserID == 0 || claims.Username == "" {
		return nil, errors.New("missing authenticated user")
	}
	user, err := h.userService.GetProfile(c.Request.Context(), claims.Username)
	if err != nil {
		return nil, err
	}
	if user == nil || user.ID != claims.UserID {
		return nil, errors.New("authenticated user changed")
	}
	return user, nil
}

func documentRequestIdentity(c *gin.Context) (uint, string, bool) {
	id, err := strconv.ParseUint(c.Query("documentId"), 10, 32)
	version := c.Query("version")
	if err != nil || id == 0 || len(version) > 64 || c.Query("fileName") != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": http.StatusBadRequest, "message": "必须使用documentId和可选version定位文档，不能按文件名选择", "data": nil})
		return 0, "", false
	}
	return uint(id), version, true
}

func documentAccessError(c *gin.Context, err error) {
	status, message := http.StatusNotFound, "文档不存在或无权访问"
	if errors.Is(err, repository.ErrDocumentChanged) {
		status, message = http.StatusConflict, "该来源版本已失效或文档尚未处理完成，请重新查询"
	}
	c.JSON(status, gin.H{"code": status, "message": message, "data": nil})
}

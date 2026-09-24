// Package handler 包含了处理 HTTP 请求的控制器逻辑。
package handler

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/token"
	"strconv"
)

// ConversationHandler 处理与对话相关的 API 请求。
type ConversationHandler struct {
	service service.ConversationService
}

// GetArchive uses the authenticated owner, never an owner supplied by the request.
func (h *ConversationHandler) GetArchive(c *gin.Context) {
	claims := c.MustGet("claims").(*token.CustomClaims)
	after, err := strconv.ParseUint(c.DefaultQuery("after", "0"), 10, 64)
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limitErr != nil || limit < 1 || limit > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "after必须为非负轮次，limit须为1到100"})
		return
	}
	turns, err := h.service.GetArchive(c.Request.Context(), claims.UserID, after, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "读取对话归档失败"})
		return
	}
	next := after
	if len(turns) > 0 {
		next = turns[len(turns)-1].TurnNo
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success", "data": gin.H{"turns": turns, "next_after": next}})
}

// NewConversationHandler 创建一个新的 ConversationHandler。
func NewConversationHandler(service service.ConversationService) *ConversationHandler {
	return &ConversationHandler{service: service}
}

// GetConversations 处理获取用户对话历史的请求。
func (h *ConversationHandler) GetConversations(c *gin.Context) {
	claims := c.MustGet("claims").(*token.CustomClaims)

	history, err := h.service.GetConversationHistory(c.Request.Context(), claims.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "Failed to retrieve conversation history",
			"data":    nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "success",
		"data":    history,
	})
}

package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"pai-smart-go/internal/repository"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/token"
)

type UserMemoryHandler struct{ service *service.UserMemoryService }

func NewUserMemoryHandler(service *service.UserMemoryService) *UserMemoryHandler {
	return &UserMemoryHandler{service: service}
}

func (h *UserMemoryHandler) List(c *gin.Context) {
	items, err := h.service.List(c.Request.Context(), c.MustGet("claims").(*token.CustomClaims).UserID)
	if err != nil {
		memoryError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success", "data": items})
}

func (h *UserMemoryHandler) Save(c *gin.Context) {
	var id uint64
	var err error
	if c.Param("id") != "" {
		id, err = strconv.ParseUint(c.Param("id"), 10, strconv.IntSize)
		if err != nil || id == 0 {
			memoryError(c, service.ErrMemoryInput)
			return
		}
	}
	var input service.UserMemoryInput
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&input); err != nil {
		memoryError(c, service.ErrMemoryInput)
		return
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		memoryError(c, service.ErrMemoryInput)
		return
	}
	item, err := h.service.Save(c.Request.Context(), c.MustGet("claims").(*token.CustomClaims).UserID, uint(id), input)
	if err != nil {
		memoryError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success", "data": item})
}

func (h *UserMemoryHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, strconv.IntSize)
	version, versionErr := strconv.ParseUint(c.Query("version"), 10, 64)
	if err != nil || versionErr != nil || id == 0 || version == 0 {
		memoryError(c, service.ErrMemoryInput)
		return
	}
	if err = h.service.Delete(c.Request.Context(), c.MustGet("claims").(*token.CustomClaims).UserID, uint(id), version); err != nil {
		memoryError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success", "data": nil})
}

func memoryError(c *gin.Context, err error) {
	status, message := http.StatusInternalServerError, "记忆操作失败，请稍后重试"
	switch {
	case errors.Is(err, service.ErrMemoryInput):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, repository.ErrMemoryNotFound):
		status, message = http.StatusNotFound, "记忆不存在或已失效"
	case errors.Is(err, repository.ErrMemoryConflict):
		status, message = http.StatusConflict, "记忆已被修改或同范围标题重复，请刷新后重试"
	case errors.Is(err, repository.ErrMemoryLimit):
		status, message = http.StatusConflict, "最多保存50条长期记忆，请先删除不再需要的条目"
	}
	c.JSON(status, gin.H{"code": status, "message": message, "data": nil})
}

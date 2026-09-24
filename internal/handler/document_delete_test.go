package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/token"
)

type deleteIdentityService struct {
	service.DocumentService
	deleted *uint
}

func (s deleteIdentityService) DeleteDocument(_ context.Context, id uint, user *model.User) error {
	*s.deleted = id
	return nil
}

func TestDeleteHandlerRequiresSQLDocumentID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var deleted uint
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("claims", &token.CustomClaims{UserID: 1, Username: "alice"}); c.Next() })
	h := NewDocumentHandler(deleteIdentityService{deleted: &deleted}, chatTestUsers{})
	router.DELETE("/documents/:documentId", h.DeleteDocument)
	for _, tc := range []struct {
		path   string
		status int
		id     uint
	}{
		{"37", 200, 37}, {"0", 400, 0}, {"-1", 400, 0},
		{"0123456789abcdef0123456789abcdef", 400, 0},
		{"37?userId=8", 400, 0}, {"37?fileMd5=same", 400, 0},
	} {
		deleted = 0
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/documents/"+tc.path, nil))
		if response.Code != tc.status || deleted != tc.id {
			t.Fatalf("%s status=%d deleted=%d", tc.path, response.Code, deleted)
		}
	}
}

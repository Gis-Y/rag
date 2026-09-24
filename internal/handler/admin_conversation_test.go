package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"pai-smart-go/internal/service"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type adminConversationService struct {
	service.AdminService
	page int
	size int
}

func (s *adminConversationService) GetAllConversations(_ context.Context, _ *uint, _, _ *time.Time, page, size int) (*service.AdminConversationPage, error) {
	s.page, s.size = page, size
	return &service.AdminConversationPage{Content: []service.AdminConversationMessage{}, Size: size, Number: page}, nil
}

func TestAdminConversationPaginationDefaultsAndBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		query      string
		status     int
		page, size int
	}{
		{"", http.StatusOK, 1, 20},
		{"?page=3&size=7", http.StatusOK, 3, 7},
		{"?page=0", http.StatusBadRequest, 0, 0},
		{"?size=101", http.StatusBadRequest, 0, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			fake := &adminConversationService{}
			h := &AdminHandler{adminService: fake}
			router := gin.New()
			router.GET("/admin/conversation", h.GetAllConversations)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/admin/conversation"+tc.query, nil)
			router.ServeHTTP(response, request)
			if response.Code != tc.status || fake.page != tc.page || fake.size != tc.size {
				t.Fatalf("status=%d page=%d size=%d body=%s", response.Code, fake.page, fake.size, response.Body.String())
			}
		})
	}
}

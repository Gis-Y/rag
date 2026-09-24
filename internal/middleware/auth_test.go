package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/token"
	"testing"

	"github.com/gin-gonic/gin"
)

type authTestUsers struct {
	service.UserService
	revoked   bool
	profileID uint
}

func (u authTestUsers) IsTokenRevoked(context.Context, string) (bool, error) {
	return u.revoked, nil
}

func (u authTestUsers) GetProfile(context.Context, string) (*model.User, error) {
	if u.profileID == 0 {
		u.profileID = 1
	}
	return &model.User{ID: u.profileID, Username: "alice", Role: "USER"}, nil
}

func TestAuthMiddlewareAcceptsOnlyActiveAccessTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := token.NewJWTManager("middleware-test-secret", 1, 1)
	access, _ := manager.GenerateToken(1, "alice", "USER")
	refresh, _ := manager.GenerateRefreshToken(1, "alice", "USER")

	for name, credential := range map[string]struct {
		token     string
		revoked   bool
		profileID uint
		status    int
	}{
		"access":            {access, false, 1, http.StatusNoContent},
		"refresh":           {refresh, false, 1, http.StatusUnauthorized},
		"revoked":           {access, true, 1, http.StatusUnauthorized},
		"recreated-account": {access, false, 2, http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			router := gin.New()
			router.Use(AuthMiddleware(manager, authTestUsers{revoked: credential.revoked, profileID: credential.profileID}))
			router.GET("/private", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			request := httptest.NewRequest(http.MethodGet, "/private", nil)
			request.Header.Set("Authorization", "Bearer "+credential.token)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != credential.status {
				t.Fatalf("status = %d, want %d", response.Code, credential.status)
			}
		})
	}
}

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"pai-smart-go/internal/config"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/token"
)

func TestUserMemoryHTTPRejectsUntrustedInput(t *testing.T) {
	h := NewUserMemoryHandler(service.NewUserMemoryService(nil, nil, config.MemoryConfig{}))
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("claims", &token.CustomClaims{UserID: 7}); c.Next() })
	r.POST("/memories", h.Save)
	r.PUT("/memories/:id", h.Save)
	r.DELETE("/memories/:id", h.Delete)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/memories", `{"user_id":8}`},
		{"POST", "/memories", `{"confirmed":false}`},
		{"POST", "/memories", `{} {}`},
		{"POST", "/memories", `{"content":"` + strings.Repeat("x", 9000) + `"}`},
		{"POST", "/memories", `{"confirmed":true,"kind":"preference","key":"密码","content":"password: secret"}`},
		{"PUT", "/memories/0", `{}`},
		{"PUT", "/memories/nope", `{}`},
		{"PUT", "/memories/1", `{"confirmed":true,"kind":"preference","key":"语言","content":"中文"}`},
		{"DELETE", "/memories/1", ""},
		{"DELETE", "/memories/1?version=0", ""},
		{"DELETE", "/memories/-1?version=2", ""},
	} {
		t.Run(tc.method+tc.path+tc.body[:min(len(tc.body), 30)], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			r.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("request reached storage: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

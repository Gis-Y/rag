package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/llm"
	"pai-smart-go/pkg/token"

	"github.com/gin-gonic/gin"
)

type sourceDocumentService struct {
	service.DocumentService
	read func(context.Context, uint, string, *model.User) error
}

func (s sourceDocumentService) GenerateDownloadURL(ctx context.Context, id uint, version string, user *model.User) (*service.DownloadInfoDTO, error) {
	return &service.DownloadInfoDTO{DocumentID: id, Version: version}, s.read(ctx, id, version, user)
}

func (s sourceDocumentService) GetFilePreviewContent(ctx context.Context, id uint, version string, user *model.User) (*service.PreviewInfoDTO, error) {
	return &service.PreviewInfoDTO{DocumentID: id, Version: version}, s.read(ctx, id, version, user)
}

func TestDocumentSourceHandlersRequireIdentityAndForwardVersion(t *testing.T) {
	for _, endpoint := range []string{"download", "preview"} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			var failure error
			docs := sourceDocumentService{read: func(ctx context.Context, id uint, version string, user *model.User) error {
				calls++
				if id != 37 || version != "v2" || user.ID != 1 || ctx == nil {
					t.Fatalf("identity changed in handler: %d %q %+v", id, version, user)
				}
				return failure
			}}
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("claims", &token.CustomClaims{UserID: 1, Username: "alice"}); c.Next() })
			h := NewDocumentHandler(docs, chatTestUsers{})
			router.GET("/download", h.GenerateDownloadURL)
			router.GET("/preview", h.PreviewFile)
			for _, query := range []string{"fileName=same.pdf", "documentId=0", "documentId=-1", "documentId=37&fileName=same.pdf", "documentId=37&version=" + strings.Repeat("a", 65)} {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+endpoint+"?"+query, nil))
				if w.Code != http.StatusBadRequest || calls != 0 {
					t.Fatalf("filename/invalid identity reached service: status=%d calls=%d", w.Code, calls)
				}
			}
			for _, tc := range []struct {
				err    error
				status int
			}{{nil, 200}, {repository.ErrDocumentChanged, 409}, {errors.New("denied"), 404}} {
				failure = tc.err
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+endpoint+"?documentId=37&version=v2&userId=999", nil))
				if w.Code != tc.status {
					t.Fatalf("source status=%d want=%d body=%s", w.Code, tc.status, w.Body)
				}
			}
		})
	}
}

type sourceChatService struct{}

func (sourceChatService) StreamResponse(ctx context.Context, query string, user *model.User, emit llm.ChunkHandler, sources service.SourceHandler, stop func() bool) error {
	if err := sources([]model.SourceCitation{{Number: 1, DocumentID: 37, Version: "v2", FileName: "同名(终稿).pdf"}}); err != nil {
		return err
	}
	return emit([]byte("回答 [来源#1]"))
}

func TestChatSourcesFramePrecedesTextAndCompletion(t *testing.T) {
	conn := dialChatTest(t, startChatTest(t, sourceChatService{}, chatTestUsers{}))
	sendChatTest(t, conn, "问题")
	frame := readChatTest(t, conn)
	if frame["type"] != "sources" {
		t.Fatalf("missing trusted sources event: %+v", frame)
	}
	if got := fmt.Sprint(frame["sources"]); !strings.Contains(got, "documentId:37") || !strings.Contains(got, "version:v2") {
		t.Fatalf("sources frame lost document identity: %s", got)
	}
	if frame := readChatTest(t, conn); frame["chunk"] != "回答 [来源#1]" {
		t.Fatalf("text order changed: %+v", frame)
	}
	requireCompletion(t, conn, false)
}

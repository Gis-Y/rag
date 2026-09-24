package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/token"
)

const processingTestMD5 = "0123456789abcdef0123456789abcdef"

type processingTestUploads struct {
	repository.UploadRepository
	find func(context.Context, uint, []string) ([]model.FileUpload, error)
}

func (r processingTestUploads) FindAccessibleFiles(ctx context.Context, user uint, tags []string) ([]model.FileUpload, error) {
	return r.find(ctx, user, tags)
}

type processingTestStates func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error)

func (f processingTestStates) FindStates(ctx context.Context, ids []uint) (map[uint]model.DocumentProcessingState, error) {
	return f(ctx, ids)
}

type processingTestQueue func(context.Context, uint, uint) error

func (f processingTestQueue) Enqueue(ctx context.Context, documentID, userID uint) error {
	return f(ctx, documentID, userID)
}

func processingTestRouter(h *DocumentProcessingHandler, claims *token.CustomClaims) *gin.Engine {
	router := gin.New()
	if claims != nil {
		router.Use(func(c *gin.Context) { c.Set("claims", claims); c.Next() })
	}
	router.GET("/documents/:fileMd5/processing", h.Status)
	router.POST("/documents/:fileMd5/reprocess", h.Retry)
	return router
}

func TestDocumentProcessingOwnerAdminAndDeletionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, role, ownerQuery string
		fileOwner, profileID   uint
		fileStatus, expected   int
		noClaims               bool
	}{
		{"owner", "USER", "", 7, 7, 1, 200, false},
		{"admin selects owner", "ADMIN", "?userId=8", 8, 7, 1, 200, false},
		{"user cannot specify owner", "USER", "?userId=8", 8, 7, 1, 403, false},
		{"public is not ownership", "USER", "", 8, 7, 1, 404, false},
		{"deleted", "USER", "", 7, 7, 3, 404, false},
		{"upload incomplete", "USER", "", 7, 7, 0, 404, false},
		{"invalid owner", "ADMIN", "?userId=0", 7, 7, 1, 400, false},
		{"identity changed", "USER", "", 7, 8, 1, 401, false},
		{"unauthenticated", "USER", "", 7, 7, 1, 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := model.FileUpload{ID: 11, FileMD5: processingTestMD5, FileName: "doc.pdf", UserID: tc.fileOwner, Status: tc.fileStatus, IsPublic: true}
			stateCalls := 0
			uploads := processingTestUploads{find: func(ctx context.Context, owner uint, tags []string) ([]model.FileUpload, error) {
				if _, ok := ctx.Deadline(); !ok || len(tags) != 0 {
					t.Fatal("missing deadline or unexpected shared organization access")
				}
				wantOwner := uint(7)
				if tc.ownerQuery == "?userId=8" && tc.role == "ADMIN" {
					wantOwner = 8
				}
				if owner != wantOwner {
					t.Fatalf("wrong SQL owner: %d", owner)
				}
				return []model.FileUpload{file}, nil
			}}
			states := processingTestStates(func(ctx context.Context, ids []uint) (map[uint]model.DocumentProcessingState, error) {
				stateCalls++
				if !reflect.DeepEqual(ids, []uint{11}) {
					t.Fatalf("client controlled document IDs: %v", ids)
				}
				return nil, nil
			})
			users := chatTestUsers{profile: func(context.Context, string) (*model.User, error) {
				return &model.User{ID: tc.profileID, Role: tc.role}, nil
			}}
			h := NewDocumentProcessingHandler(uploads, states, nil, users)
			claims := &token.CustomClaims{UserID: 7, Username: "alice", Role: "ADMIN"} // DB role, not claim role, authorizes override.
			if tc.noClaims {
				claims = nil
			}
			router := processingTestRouter(h, claims)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/documents/"+processingTestMD5+"/processing"+tc.ownerQuery, nil))
			if response.Code != tc.expected {
				t.Fatalf("wrong status: %d %s", response.Code, response.Body.String())
			}
			if tc.expected != 200 && stateCalls != 0 {
				t.Fatal("unauthorized or deleted file reached processing-state read")
			}
			if tc.expected == 200 && !strings.Contains(response.Body.String(), `"status":"not_processed"`) {
				t.Fatalf("complete upload falsely reported ready: %s", response.Body.String())
			}
		})
	}
}

func TestDocumentReprocessUsesSQLFieldsAndReportsQueueOnly(t *testing.T) {
	file := model.FileUpload{ID: 11, UserID: 7, FileMD5: processingTestMD5, FileName: "真实文档.pdf", OrgTag: "ORG", Status: 1}
	state := model.DocumentProcessingState{DocumentID: 11, UserID: 7, FileMD5: processingTestMD5, ActiveVersion: "old", PendingVersion: "failed", Status: "failed", Error: "parser failed", UpdatedAt: time.Now()}
	for _, tc := range []struct {
		name, body string
		produceErr error
		expected   int
	}{
		{"empty body", "", nil, 202},
		{"empty object", "{}", nil, 202},
		{"producer failed", "", errors.New("unavailable"), 503},
		{"producer deadline", "", context.DeadlineExceeded, 504},
		{"untrusted task", `{"document_id":99,"user_id":8,"file_name":"evil.pdf","object_url":"https://invalid","is_public":true}`, nil, 400},
		{"trailing JSON", "{} {}", nil, 400},
		{"oversized body", `{"x":"` + strings.Repeat("x", 1200) + `"}`, nil, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			queue := processingTestQueue(func(ctx context.Context, documentID, userID uint) error {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("producer has no request deadline")
				}
				if documentID != file.ID || userID != file.UserID {
					t.Fatalf("task identity is not SQL-derived: %d %d", documentID, userID)
				}
				return tc.produceErr
			})
			h := NewDocumentProcessingHandler(processingTestUploads{find: func(context.Context, uint, []string) ([]model.FileUpload, error) {
				return []model.FileUpload{file}, nil
			}}, processingTestStates(func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error) {
				return map[uint]model.DocumentProcessingState{11: state}, nil
			}), queue, chatTestUsers{profile: func(context.Context, string) (*model.User, error) { return &model.User{ID: 7, Role: "USER"}, nil }})
			router := processingTestRouter(h, &token.CustomClaims{UserID: 7, Username: "alice"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/documents/"+processingTestMD5+"/reprocess?documentId=99&fileName=evil&isPublic=true", strings.NewReader(tc.body)))
			if response.Code != tc.expected || (tc.expected == 400 && calls != 0) || (tc.expected != 400 && calls != 1) {
				t.Fatalf("wrong enqueue result: %d calls=%d %s", response.Code, calls, response.Body.String())
			}
			if tc.expected == 202 && (!strings.Contains(response.Body.String(), `"status":"queued"`) || strings.Contains(response.Body.String(), `"status":"ready"`)) {
				t.Fatalf("enqueue acknowledgement falsely claims processing completion: %s", response.Body.String())
			}
			status := httptest.NewRecorder()
			router.ServeHTTP(status, httptest.NewRequest("GET", "/documents/"+processingTestMD5+"/processing", nil))
			var output struct {
				Data struct {
					Status, ActiveVersion, PendingVersion, Error string
					Searchable                                   bool
				}
			}
			if err := json.Unmarshal(status.Body.Bytes(), &output); err != nil || output.Data.Status != "failed" || output.Data.ActiveVersion != "old" || output.Data.PendingVersion != "failed" || !output.Data.Searchable || output.Data.Error != "parser failed" {
				t.Fatalf("status discarded old active version or invented publication: %s %v", status.Body.String(), err)
			}
		})
	}
}

func TestDocumentReprocessRecentPendingAndExpiredRetry(t *testing.T) {
	for _, stale := range []bool{false, true} {
		file := model.FileUpload{ID: 11, UserID: 7, FileMD5: processingTestMD5, Status: 1}
		state := model.DocumentProcessingState{DocumentID: 11, UserID: 7, FileMD5: processingTestMD5, PendingVersion: "pending", Status: "embedding", UpdatedAt: time.Now()}
		if stale {
			state.UpdatedAt = state.UpdatedAt.Add(-6 * time.Minute)
		}
		calls := 0
		h := NewDocumentProcessingHandler(processingTestUploads{find: func(context.Context, uint, []string) ([]model.FileUpload, error) {
			return []model.FileUpload{file}, nil
		}}, processingTestStates(func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error) {
			return map[uint]model.DocumentProcessingState{11: state}, nil
		}), processingTestQueue(func(context.Context, uint, uint) error { calls++; return nil }), chatTestUsers{profile: func(context.Context, string) (*model.User, error) { return &model.User{ID: 7}, nil }})
		router := processingTestRouter(h, &token.CustomClaims{UserID: 7, Username: "alice"})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("POST", "/documents/"+processingTestMD5+"/reprocess", nil))
		if (!stale && (response.Code != 409 || calls != 0)) || (stale && (response.Code != 202 || calls != 1)) {
			t.Fatalf("wrong pending duplicate policy stale=%v code=%d calls=%d", stale, response.Code, calls)
		}
	}
}

func TestDocumentProcessingCancellationAndStateReadFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		file := model.FileUpload{ID: 11, UserID: 7, FileMD5: processingTestMD5, Status: 1}
		h := NewDocumentProcessingHandler(processingTestUploads{find: func(ctx context.Context, owner uint, tags []string) ([]model.FileUpload, error) {
			if canceled {
				cancel()
				return nil, ctx.Err()
			}
			return []model.FileUpload{file}, nil
		}}, processingTestStates(func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error) {
			return nil, errors.New("SQL unavailable")
		}), nil, chatTestUsers{profile: func(context.Context, string) (*model.User, error) { return &model.User{ID: 7}, nil }})
		router := processingTestRouter(h, &token.CustomClaims{UserID: 7, Username: "alice"})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/documents/"+processingTestMD5+"/reprocess", nil).WithContext(ctx))
		cancel()
		if (!canceled && response.Code != 503) || (canceled && response.Code != 408) {
			t.Fatalf("lookup failure/cancellation must not enqueue: %d %s", response.Code, response.Body.String())
		}
	}
}

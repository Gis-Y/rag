package service

import (
	"context"
	"errors"
	"io"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"testing"
	"time"
)

type contextUserRepo struct {
	repository.UserRepository
	find func(context.Context) (*model.User, error)
}

func (r contextUserRepo) FindByUsername(ctx context.Context, _ string) (*model.User, error) {
	return r.find(ctx)
}

type contextOrgRepo struct {
	repository.OrgTagRepository
	find func(context.Context) ([]model.OrganizationTag, error)
}

func (r contextOrgRepo) FindAll(ctx context.Context) ([]model.OrganizationTag, error) {
	return r.find(ctx)
}

type contextUploadRepo struct {
	repository.UploadRepository
	find       func(context.Context) ([]model.FileUpload, error)
	findAccess func(context.Context, uint, []string) ([]model.FileUpload, error)
}

func (r contextUploadRepo) FindAccessibleFiles(ctx context.Context, userID uint, tags []string) ([]model.FileUpload, error) {
	if r.findAccess != nil {
		return r.findAccess(ctx, userID, tags)
	}
	return r.find(ctx)
}

func testCurrentUserService(id uint, username, orgTags string) UserService {
	return NewUserService(contextUserRepo{find: func(context.Context) (*model.User, error) {
		return &model.User{ID: id, Username: username, OrgTags: orgTags}, nil
	}}, contextOrgRepo{find: func(context.Context) ([]model.OrganizationTag, error) {
		return []model.OrganizationTag{}, nil
	}}, nil)
}

func TestChatPermissionServicesForwardCancellation(t *testing.T) {
	for _, stage := range []string{"profile", "organization-tags", "accessible-files"} {
		t.Run(stage, func(t *testing.T) {
			started := make(chan struct{})
			wait := func(ctx context.Context) error {
				close(started)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
					return errors.New("service did not forward cancellation")
				}
			}
			users := NewUserService(contextUserRepo{find: func(ctx context.Context) (*model.User, error) {
				return nil, wait(ctx)
			}}, contextOrgRepo{find: func(ctx context.Context) ([]model.OrganizationTag, error) {
				if stage == "organization-tags" {
					return nil, wait(ctx)
				}
				return nil, nil
			}}, nil)
			search, err := NewSearchService(nil, nil, users, contextUploadRepo{find: func(ctx context.Context) ([]model.FileUpload, error) {
				return nil, wait(ctx)
			}}, testDocumentSearchOptions(nil))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				var err error
				if stage == "profile" {
					_, err = users.GetProfile(ctx, "alice")
				} else {
					_, err = search.HybridSearch(ctx, "question", 5, &model.User{ID: 1, Username: "alice", OrgTags: "TEAM"})
				}
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("permission query did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation was not preserved: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("permission service ignored cancellation")
			}
		})
	}
}

type contextEmbedding struct{}

func (contextEmbedding) CreateEmbedding(context.Context, string) ([]float32, error) {
	return []float32{1}, nil
}

type retrySearchIndex struct {
	calls int
	err   error
}

func (s *retrySearchIndex) Search(context.Context, io.Reader) ([]model.SearchHit, error) {
	s.calls++
	if s.calls == 2 {
		return nil, s.err
	}
	return nil, nil
}

func TestSearchRetryPreservesFailure(t *testing.T) {
	for _, failure := range []error{errors.New("search unavailable"), context.Canceled} {
		index := &retrySearchIndex{err: failure}
		users := NewUserService(nil, nil, nil)
		uploads := contextUploadRepo{find: func(context.Context) ([]model.FileUpload, error) {
			return []model.FileUpload{{ID: 11, UserID: 1, FileMD5: "doc", FileName: "doc.txt"}}, nil
		}}
		search, err := NewSearchService(contextEmbedding{}, index, users, uploads, testDocumentSearchOptions(map[uint]model.DocumentProcessingState{11: {ActiveVersion: "v1"}}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = search.HybridSearch(context.Background(), "请问这个项目是什么？", 5, &model.User{ID: 1, Username: "alice"})
		if !errors.Is(err, failure) || index.calls != 2 {
			t.Fatalf("retry error was treated as empty hits: err=%v calls=%d", err, index.calls)
		}
	}
}

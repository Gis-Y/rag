package service

import (
	"context"
	"encoding/json"
	"io"
	"pai-smart-go/internal/model"
	"reflect"
	"strings"
	"testing"
)

func TestVersionedSearchAccessSeparatesSameMD5ByOwner(t *testing.T) {
	files := []model.FileUpload{
		{ID: 11, UserID: 1, FileMD5: "same", FileName: "one.pdf"},
		{ID: 12, UserID: 2, FileMD5: "same", FileName: "two.pdf"},
	}
	filter := buildVersionedSearchAccess(files, map[uint]model.DocumentProcessingState{11: {ActiveVersion: "v1"}, 12: {ActiveVersion: "v2"}})

	encoded, err := json.Marshal(filter)
	if err != nil {
		t.Fatal(err)
	}
	query := string(encoded)
	for _, want := range []string{`"user_id":1`, `"user_id":2`, `"file_md5":"same"`, `"document_id":11`, `"document_id":12`, `"version":"v1"`, `"version":"v2"`} {
		if !strings.Contains(query, want) {
			t.Fatalf("access filter %s does not contain %s", query, want)
		}
	}
}

type authorizedSearchIndex func(context.Context, io.Reader) ([]model.SearchHit, error)

func (f authorizedSearchIndex) Search(ctx context.Context, body io.Reader) ([]model.SearchHit, error) {
	return f(ctx, body)
}

func TestHybridSearchKeepsACLForEverySubquery(t *testing.T) {
	uploads := contextUploadRepo{find: func(context.Context) ([]model.FileUpload, error) {
		return []model.FileUpload{{ID: 11, UserID: 7, FileMD5: "same", FileName: "allowed.txt"}}, nil
	}}
	index := authorizedSearchIndex(func(ctx context.Context, body io.Reader) ([]model.SearchHit, error) {
		var query map[string]json.RawMessage
		if err := json.NewDecoder(body).Decode(&query); err != nil {
			return nil, err
		}
		for _, section := range []string{"knn", "query"} {
			text := string(query[section])
			if !strings.Contains(text, `"filter"`) || !strings.Contains(text, `"user_id":7`) || !strings.Contains(text, `"same"`) {
				t.Errorf("missing ACL in %s: %s", section, text)
			}
		}
		return []model.SearchHit{
			{Source: model.EsDocument{DocumentID: 11, Version: "v1", ModelVersion: "embedding-v1", UserID: 7, FileMD5: "same", TextContent: "authorized"}},
			{Source: model.EsDocument{DocumentID: 11, Version: "v1", ModelVersion: "embedding-v1", UserID: 8, FileMD5: "same", TextContent: "other tenant"}},
		}, nil
	})
	search, err := NewSearchService(contextEmbedding{}, index, testCurrentUserService(7, "alice", ""), uploads, testDocumentSearchOptions(map[uint]model.DocumentProcessingState{11: {ActiveVersion: "v1"}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{"星海负责人", "李四的履历"} {
		hits, err := search.HybridSearch(context.Background(), question, 5, &model.User{ID: 7, Username: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].UserID != "7" || hits[0].FileName != "allowed.txt" {
			t.Fatalf("cross-tenant hit escaped: %+v", hits)
		}
	}
}

func TestHybridSearchReloadsCurrentUserBeforeFinalACL(t *testing.T) {
	accessChecks := 0
	uploads := contextUploadRepo{findAccess: func(_ context.Context, userID uint, tags []string) ([]model.FileUpload, error) {
		accessChecks++
		if userID != 7 {
			t.Fatalf("wrong user: %d", userID)
		}
		if accessChecks == 1 {
			if !reflect.DeepEqual(tags, []string{"TEAM"}) {
				t.Fatalf("initial ACL did not use request user: %v", tags)
			}
			return []model.FileUpload{{ID: 11, UserID: 8, FileMD5: "team", FileName: "team.pdf"}}, nil
		}
		if len(tags) != 0 {
			t.Fatalf("final ACL reused revoked tags: %v", tags)
		}
		return []model.FileUpload{}, nil
	}}
	users := NewUserService(contextUserRepo{find: func(context.Context) (*model.User, error) {
		return &model.User{ID: 7, Username: "alice", OrgTags: ""}, nil
	}}, contextOrgRepo{find: func(context.Context) ([]model.OrganizationTag, error) {
		return []model.OrganizationTag{{TagID: "TEAM"}}, nil
	}}, nil)
	index := authorizedSearchIndex(func(context.Context, io.Reader) ([]model.SearchHit, error) {
		return []model.SearchHit{{Source: model.EsDocument{DocumentID: 11, Version: "v1", ModelVersion: "embedding-v1", UserID: 8, FileMD5: "team", TextContent: "secret"}}}, nil
	})
	search, err := NewSearchService(contextEmbedding{}, index, users, uploads, testDocumentSearchOptions(map[uint]model.DocumentProcessingState{11: {ActiveVersion: "v1"}}))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := search.HybridSearch(context.Background(), "query", 5, &model.User{ID: 7, Username: "alice", OrgTags: "TEAM"})
	if err != nil || len(hits) != 0 || accessChecks != 2 {
		t.Fatalf("revoked organization remained searchable: hits=%+v checks=%d err=%v", hits, accessChecks, err)
	}
}

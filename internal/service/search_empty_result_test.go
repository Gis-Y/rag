package service

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"pai-smart-go/internal/model"
)

func TestSearchFilteredResultsSerializeAsEmptyArray(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		accessChecks, stateChecks := 0, 0
		files := []model.FileUpload{{ID: 11, UserID: 7, FileMD5: "doc", Status: 1}}
		uploads := contextUploadRepo{find: func(context.Context) ([]model.FileUpload, error) {
			accessChecks++
			if revoked && accessChecks > 1 {
				return nil, nil
			}
			return files, nil
		}}
		repo := documentSearchRepo{states: func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error) {
			stateChecks++
			version := "v1"
			if stateChecks > 1 {
				version = "v2"
			}
			return map[uint]model.DocumentProcessingState{11: {ActiveVersion: version}}, nil
		}}
		index := authorizedSearchIndex(func(context.Context, io.Reader) ([]model.SearchHit, error) {
			return []model.SearchHit{{Source: model.EsDocument{DocumentID: 11, UserID: 7, FileMD5: "doc", Version: "v1", ModelVersion: "embedding-v1", TextContent: "old"}}}, nil
		})
		s, err := NewSearchService(contextEmbedding{}, index, testCurrentUserService(7, "alice", ""), uploads, DocumentSearchOptions{Repository: repo, ModelVersion: "embedding-v1"})
		if err != nil {
			t.Fatal(err)
		}
		hits, err := s.HybridSearch(context.Background(), "query", 10, &model.User{ID: 7, Username: "alice"})
		encoded, marshalErr := json.Marshal(hits)
		if err != nil || marshalErr != nil || string(encoded) != "[]" {
			t.Fatalf("filtered results must be [], revoked=%t got=%s error=%v marshal=%v", revoked, encoded, err, marshalErr)
		}
	}
}

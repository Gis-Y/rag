package es

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
)

func TestVersionedBulkAndCleanup(t *testing.T) {
	if _, err := NewClient(config.ElasticsearchConfig{}); err == nil {
		t.Fatal("accepted missing vector dimensions")
	}
	bulkResult := `{"errors":false,"items":[{"index":{"_id":"v1","status":201}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "HEAD":
			w.WriteHeader(200)
		case strings.HasSuffix(r.URL.Path, "_mapping"):
			io.WriteString(w, `{"test":{"mappings":{"properties":{"vector":{"dims":2}}}}}`)
		case strings.HasSuffix(r.URL.Path, "_bulk"):
			if r.URL.Query().Get("refresh") != "wait_for" {
				t.Error("bulk did not wait for visibility")
			}
			body, _ := io.ReadAll(r.Body)
			if len(strings.Split(strings.TrimSpace(string(body)), "\n")) != 2 {
				t.Error("invalid bulk NDJSON")
			}
			io.WriteString(w, bulkResult)
		case strings.HasSuffix(r.URL.Path, "_delete_by_query"):
			var query map[string]any
			_ = json.NewDecoder(r.Body).Decode(&query)
			body, _ := json.Marshal(query)
			for _, key := range []string{"document_id", "user_id", "version"} {
				if !strings.Contains(string(body), key) {
					t.Errorf("unscoped delete: missing %s", key)
				}
			}
			io.WriteString(w, `{"failures":[],"timed_out":false}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	client, err := NewClient(config.ElasticsearchConfig{Addresses: server.URL, IndexName: "test", Dimensions: 2})
	if err != nil {
		t.Fatal(err)
	}
	documents := []model.EsDocument{{VectorID: "v1", Vector: []float32{1, 2}}}
	if err := client.BulkIndexDocuments(context.Background(), documents); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"errors":true,"items":[{"index":{"_id":"v1","status":400}}]}`, `{"errors":false,"items":[]}`, `{"errors":false,"items":[{"index":{"_id":"wrong","status":201}}]}`} {
		bulkResult = bad
		if err := client.BulkIndexDocuments(context.Background(), documents); err == nil {
			t.Fatal("accepted failed/missing bulk item")
		}
	}
	if err := client.DeleteDocumentVersion(context.Background(), 1, 2, "generation"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteDocument(context.Background(), 0, 2); err == nil {
		t.Fatal("accepted unscoped delete")
	}
	if _, err := NewClient(config.ElasticsearchConfig{Addresses: server.URL, IndexName: "test", Dimensions: 3}); err == nil {
		t.Fatal("accepted incompatible existing index")
	}
}

func TestClientStartupHasTotalDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	started := time.Now()
	if _, err := newClient(config.ElasticsearchConfig{Addresses: server.URL, IndexName: "test", Dimensions: 2}, 50*time.Millisecond); err == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("startup deadline was not enforced: duration=%s err=%v", time.Since(started), err)
	}
}

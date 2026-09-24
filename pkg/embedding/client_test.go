package embedding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"pai-smart-go/internal/config"
	"pai-smart-go/pkg/log"
)

func TestBatchEmbeddingValidation(t *testing.T) {
	log.Init("error", "json", "")
	for _, test := range []struct {
		name, body string
		ok         bool
	}{
		{"reordered", `{"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}]}`, true},
		{"missing", `{"data":[{"index":0,"embedding":[1,2]}]}`, false},
		{"duplicate", `{"data":[{"index":0,"embedding":[1,2]},{"index":0,"embedding":[3,4]}]}`, false},
		{"wrong dimensions", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[3,4]}]}`, false},
		{"no index", `{"data":[{"embedding":[1,2]},{"embedding":[3,4]}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			client := NewClient(config.EmbeddingConfig{BaseURL: server.URL, Model: "fixture", Dimensions: 2})
			vectors, err := client.CreateEmbeddings(context.Background(), []string{"first", "second"})
			if (err == nil) != test.ok {
				t.Fatalf("unexpected result: %v", err)
			}
			if test.ok && (vectors[0][0] != 1 || vectors[1][0] != 3) {
				t.Fatal("lost response ordering")
			}
		})
	}
}

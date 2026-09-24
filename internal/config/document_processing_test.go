package config

import (
	"strings"
	"testing"
)

func TestDocumentProcessingConfiguration(t *testing.T) {
	if err := (DocumentProcessingConfig{}).Validate(EmbeddingConfig{}); err == nil {
		t.Fatal("missing document processing configuration must fail")
	}
	cfg := DocumentProcessingConfig{WorkerURL: "http://127.0.0.1:8091", WorkerToken: "test-worker-token", EmbeddingModel: "fixture", TokenizerID: "fixture", TokenizerRevision: strings.Repeat("a", 40)}
	embedding := EmbeddingConfig{BaseURL: "http://127.0.0.1:8000/v1", Model: "fixture", Dimensions: 1024}
	if err := cfg.Validate(embedding); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"file:///tmp/parser", "http://user:secret@localhost/parser", "https://example.org/?token=secret"} {
		copy := cfg
		copy.WorkerURL = bad
		if copy.Validate(embedding) == nil {
			t.Fatal("accepted invalid worker URL")
		}
	}
	for _, mutate := range []func(*DocumentProcessingConfig, *EmbeddingConfig){
		func(c *DocumentProcessingConfig, _ *EmbeddingConfig) { c.WorkerToken = " " },
		func(c *DocumentProcessingConfig, _ *EmbeddingConfig) { c.TokenizerID = "other" },
		func(c *DocumentProcessingConfig, _ *EmbeddingConfig) { c.EmbeddingModel = "other" },
		func(c *DocumentProcessingConfig, _ *EmbeddingConfig) { c.TokenizerRevision = strings.Repeat("A", 40) },
		func(c *DocumentProcessingConfig, e *EmbeddingConfig) {
			c.TokenizerID, c.EmbeddingModel, e.Model = " ", " ", " "
		},
		func(_ *DocumentProcessingConfig, e *EmbeddingConfig) { e.BaseURL = "" },
		func(_ *DocumentProcessingConfig, e *EmbeddingConfig) { e.BaseURL = "file:///model" },
		func(_ *DocumentProcessingConfig, e *EmbeddingConfig) { e.Dimensions = 0 },
	} {
		copy, model := cfg, embedding
		mutate(&copy, &model)
		if copy.Validate(model) == nil {
			t.Fatal("accepted incomplete or inconsistent document processing configuration")
		}
	}
	cfg.TokenizerRevision = "main"
	if cfg.Validate(embedding) == nil {
		t.Fatal("accepted moving tokenizer revision")
	}
}

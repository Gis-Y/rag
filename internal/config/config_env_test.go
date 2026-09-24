package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAllowsEnvironmentToReplaceTrackedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("jwt:\n  secret: tracked-placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_SECRET", "environment-secret")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JWT.Secret != "environment-secret" {
		t.Fatalf("environment override was ignored: %q", cfg.JWT.Secret)
	}
}

func TestLoadBindsDocumentProcessingEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "document_processing:\n" +
		"  worker_token: tracked-token\n" +
		"  tokenizer_id: tracked-tokenizer\n" +
		"  tokenizer_revision: tracked-revision\n" +
		"  embedding_model: tracked-model\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCUMENT_WORKER_TOKEN", "shared-worker-secret")
	t.Setenv("DOCUMENT_TOKENIZER_ID", "environment-tokenizer")
	t.Setenv("DOCUMENT_TOKENIZER_REVISION", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("DOCUMENT_EMBEDDING_MODEL", "environment-model")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DocumentProcessing.WorkerToken != "shared-worker-secret" {
		t.Fatalf("explicit worker token binding was ignored: %q", cfg.DocumentProcessing.WorkerToken)
	}
	if cfg.DocumentProcessing.TokenizerID != "environment-tokenizer" {
		t.Fatalf("explicit tokenizer ID binding was ignored: %q", cfg.DocumentProcessing.TokenizerID)
	}
	if cfg.DocumentProcessing.TokenizerRevision != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("explicit tokenizer revision binding was ignored: %q", cfg.DocumentProcessing.TokenizerRevision)
	}
	if cfg.DocumentProcessing.EmbeddingModel != "environment-model" {
		t.Fatalf("explicit embedding model binding was ignored: %q", cfg.DocumentProcessing.EmbeddingModel)
	}
}

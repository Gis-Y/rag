package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DocumentProcessingConfig configures the required structured ingestion worker.
type DocumentProcessingConfig struct {
	WorkerURL           string `mapstructure:"worker_url"`
	WorkerToken         string `mapstructure:"worker_token"`
	TokenizerID         string `mapstructure:"tokenizer_id"`
	TokenizerRevision   string `mapstructure:"tokenizer_revision"`
	EmbeddingModel      string `mapstructure:"embedding_model"`
	TimeoutSeconds      int    `mapstructure:"timeout_seconds"`
	MaxFileBytes        int64  `mapstructure:"max_file_bytes"`
	ParentContextTokens int    `mapstructure:"parent_context_tokens"`
}

func (c DocumentProcessingConfig) WithDefaults() DocumentProcessingConfig {
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 300
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 32 << 20
	}
	if c.ParentContextTokens <= 0 {
		c.ParentContextTokens = 3000
	}
	return c
}

func (c DocumentProcessingConfig) Validate(embedding EmbeddingConfig) error {
	u, err := url.Parse(c.WorkerURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("document_processing.worker_url must be an HTTP(S) service URL without credentials or query")
	}
	if strings.TrimSpace(c.WorkerToken) == "" {
		return fmt.Errorf("document_processing.worker_token is required and must match DOCUMENT_WORKER_TOKEN")
	}
	if strings.TrimSpace(c.TokenizerID) == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(c.TokenizerRevision) {
		return fmt.Errorf("document_processing requires tokenizer_id and a pinned 40-character lowercase hexadecimal tokenizer_revision")
	}
	if strings.TrimSpace(c.EmbeddingModel) == "" || c.EmbeddingModel != embedding.Model {
		return fmt.Errorf("document_processing.embedding_model must match embedding.model")
	}
	if c.TokenizerID != c.EmbeddingModel {
		return fmt.Errorf("document_processing.tokenizer_id must match embedding_model")
	}
	u, err = url.Parse(embedding.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("embedding.base_url must be an HTTP(S) service URL without credentials or query")
	}
	if embedding.Dimensions <= 0 {
		return fmt.Errorf("structured ingestion requires explicit embedding.dimensions")
	}
	if c.MaxFileBytes < 0 || c.MaxFileBytes > 1<<30 || c.TimeoutSeconds < 0 || c.TimeoutSeconds > 3600 || c.ParentContextTokens < 0 {
		return fmt.Errorf("invalid document processing resource limits")
	}
	return nil
}

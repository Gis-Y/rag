// Package embedding provides a client for interacting with embedding models.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"pai-smart-go/internal/config"
	"pai-smart-go/pkg/log"
	"strings"
	"time"
)

// Client defines the interface for an embedding client.
type Client interface {
	CreateEmbedding(ctx context.Context, text string) ([]float32, error)
}

type BatchClient interface {
	CreateEmbeddings(ctx context.Context, texts []string) ([][]float32, error)
}

type openAICompatibleClient struct {
	cfg    config.EmbeddingConfig
	client *http.Client
}

// NewClient creates a new embedding client based on the provider in the config.
func NewClient(cfg config.EmbeddingConfig) *openAICompatibleClient {
	return &openAICompatibleClient{
		cfg:    cfg,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

type embeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     *int      `json:"index"`
	} `json:"data"`
}

// CreateEmbedding calls the OpenAI-compatible API to get the vector for a given text.
func (c *openAICompatibleClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.CreateEmbeddings(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// CreateEmbeddings validates and restores response order before any index write.
func (c *openAICompatibleClient) CreateEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 || len(texts) > 64 {
		return nil, fmt.Errorf("embedding batch must contain 1 to 64 texts")
	}
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("embedding input is empty")
		}
	}
	log.Infof("[EmbeddingClient] 调用 Embedding API, model: %s, batch_size: %d", c.cfg.Model, len(texts))
	reqBody := embeddingRequest{
		Model:      c.cfg.Model, // Use model from config
		Input:      texts,
		Dimensions: c.cfg.Dimensions, // Use dimensions from config
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.cfg.BaseURL, "/")+"/embeddings", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create embedding request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Errorf("[EmbeddingClient] 调用 Embedding API 失败, error: %v", err)
		return nil, fmt.Errorf("failed to call embedding api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Errorf("[EmbeddingClient] Embedding API 返回非 200 状态码: %s", resp.Status)
		return nil, fmt.Errorf("embedding api returned non-200 status: %s", resp.Status)
	}

	var embeddingResp embeddingResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&embeddingResp); err != nil {
		log.Errorf("[EmbeddingClient] 解析 Embedding API 响应失败, error: %v", err)
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}

	if len(embeddingResp.Data) != len(texts) {
		return nil, fmt.Errorf("embedding response has missing items")
	}
	vectors := make([][]float32, len(texts))
	for _, item := range embeddingResp.Data {
		index := 0
		if item.Index != nil {
			index = *item.Index
		} else if len(texts) != 1 {
			return nil, fmt.Errorf("batch embedding response lacks item indices")
		}
		if index < 0 || index >= len(texts) || vectors[index] != nil {
			return nil, fmt.Errorf("embedding response has invalid or duplicate indices")
		}
		if len(item.Embedding) == 0 || (c.cfg.Dimensions > 0 && len(item.Embedding) != c.cfg.Dimensions) {
			return nil, fmt.Errorf("embedding response dimension does not match configuration")
		}
		for _, value := range item.Embedding {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, fmt.Errorf("embedding response has non-finite values")
			}
		}
		vectors[index] = item.Embedding
	}
	return vectors, nil
}

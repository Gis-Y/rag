// Package es 提供 Elasticsearch 基础设施适配器。
package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
)

// Client 隐藏具体 Elasticsearch SDK，业务层只依赖窄接口。
type Client struct {
	client     *elasticsearch.Client
	indexName  string
	dimensions int
}

func NewClient(cfg config.ElasticsearchConfig) (*Client, error) {
	return newClient(cfg, 30*time.Second)
}

func newClient(cfg config.ElasticsearchConfig, startupTimeout time.Duration) (*Client, error) {
	if cfg.Dimensions <= 0 {
		return nil, errors.New("elasticsearch.dimensions must be explicitly configured")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Second
	raw, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{cfg.Addresses},
		Username:  cfg.Username,
		Password:  cfg.Password,
		Transport: transport,
	})
	if err != nil {
		return nil, err
	}
	client := &Client{client: raw, indexName: cfg.IndexName, dimensions: cfg.Dimensions}
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	if err := client.createIndexIfNotExists(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *Client) createIndexIfNotExists(ctx context.Context) error {
	res, err := c.client.Indices.Exists([]string{c.indexName}, c.client.Indices.Exists.WithContext(ctx))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if !res.IsError() && res.StatusCode == http.StatusOK {
		return c.checkVectorDimensions(ctx)
	}
	if res.StatusCode != http.StatusNotFound {
		return fmt.Errorf("检查索引是否存在时收到意外的状态码: %d", res.StatusCode)
	}

	mapping := fmt.Sprintf(`{
		"mappings": {
			"properties": {
				"vector_id": { "type": "keyword" },
				"file_md5": { "type": "keyword" },
				"chunk_id": { "type": "integer" },
				"document_id": { "type": "long" },
				"version": { "type": "keyword" },
				"chunk_key": { "type": "keyword" },
				"parent_id": { "type": "keyword" },
				"file_name": { "type": "keyword" },
				"heading_path": { "type": "text" },
				"source_spans": { "type": "object", "enabled": false },
				"token_count": { "type": "integer" },
				"kind": { "type": "keyword" },
				"table_id": { "type": "keyword" },
				"row_range": { "type": "integer", "index": false },
				"column_paths": { "type": "keyword", "index": false },
				"context_prefix": { "type": "text", "index": false },
				"text_content": {
					"type": "text",
					"analyzer": "ik_max_word",
					"search_analyzer": "ik_smart"
				},
				"vector": {
					"type": "dense_vector",
					"dims": %d,
					"index": true,
					"similarity": "cosine"
				},
				"model_version": { "type": "keyword" },
				"user_id": { "type": "long" },
				"org_tag": { "type": "keyword" },
				"is_public": { "type": "boolean" }
			}
		}
	}`, c.dimensions)

	created, err := c.client.Indices.Create(c.indexName, c.client.Indices.Create.WithContext(ctx), c.client.Indices.Create.WithBody(strings.NewReader(mapping)))
	if err != nil {
		return err
	}
	defer created.Body.Close()
	if created.IsError() {
		return errors.New("创建索引时 Elasticsearch 返回错误")
	}
	return nil
}

func (c *Client) Search(ctx context.Context, body io.Reader) ([]model.SearchHit, error) {
	res, err := c.client.Search(
		c.client.Search.WithContext(ctx),
		c.client.Search.WithIndex(c.indexName),
		c.client.Search.WithBody(body),
		c.client.Search.WithTrackTotalHits(true),
	)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch search failed: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		responseBody, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("elasticsearch returned %s: %s", res.Status(), responseBody)
	}

	var response struct {
		Hits struct {
			Hits []struct {
				Source model.EsDocument `json:"_source"`
				Score  float64          `json:"_score"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode es response: %w", err)
	}

	hits := make([]model.SearchHit, 0, len(response.Hits.Hits))
	for _, hit := range response.Hits.Hits {
		hits = append(hits, model.SearchHit{Source: hit.Source, Score: hit.Score})
	}
	return hits, nil
}

func (c *Client) checkVectorDimensions(ctx context.Context) error {
	response, err := c.client.Indices.GetMapping(c.client.Indices.GetMapping.WithContext(ctx), c.client.Indices.GetMapping.WithIndex(c.indexName))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.IsError() {
		return fmt.Errorf("cannot inspect Elasticsearch vector mapping: %s", response.Status())
	}
	var indices map[string]struct {
		Mappings struct {
			Properties map[string]struct {
				Dims int `json:"dims"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.NewDecoder(response.Body).Decode(&indices); err != nil {
		return err
	}
	if len(indices) == 0 {
		return errors.New("Elasticsearch mapping is empty")
	}
	for _, index := range indices {
		if index.Mappings.Properties["vector"].Dims != c.dimensions {
			return fmt.Errorf("Elasticsearch vector dimensions differ from embedding configuration (%d); create a new index and reprocess documents", c.dimensions)
		}
	}
	return nil
}

func (c *Client) validateVector(vector []float32) error {
	if len(vector) != c.dimensions {
		return fmt.Errorf("vector dimensions: got %d, want %d", len(vector), c.dimensions)
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("non-finite embedding vector")
		}
	}
	return nil
}

// BulkIndexDocuments returns success only after every item is visible to search.
// Partial batches remain hidden by the SQL active-version gate until publication.
func (c *Client) BulkIndexDocuments(ctx context.Context, documents []model.EsDocument) error {
	for _, doc := range documents {
		if doc.VectorID == "" {
			return errors.New("missing vector identity")
		}
		if err := c.validateVector(doc.Vector); err != nil {
			return err
		}
	}
	for start := 0; start < len(documents); start += 100 {
		end := min(start+100, len(documents))
		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		for _, doc := range documents[start:end] {
			if err := encoder.Encode(map[string]any{"index": map[string]string{"_id": doc.VectorID}}); err != nil {
				return err
			}
			if err := encoder.Encode(doc); err != nil {
				return err
			}
		}
		res, err := c.client.Bulk(&body, c.client.Bulk.WithContext(ctx), c.client.Bulk.WithIndex(c.indexName), c.client.Bulk.WithRefresh("wait_for"))
		if err != nil {
			return err
		}
		var result struct {
			Errors bool `json:"errors"`
			Items  []struct {
				Index struct {
					ID     string          `json:"_id"`
					Status int             `json:"status"`
					Error  json.RawMessage `json:"error"`
				} `json:"index"`
			} `json:"items"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&result)
		res.Body.Close()
		if res.IsError() {
			return fmt.Errorf("Elasticsearch bulk failed: %s", res.Status())
		}
		if decodeErr != nil {
			return fmt.Errorf("invalid bulk response: %w", decodeErr)
		}
		if result.Errors || len(result.Items) != end-start {
			return errors.New("Elasticsearch bulk has failed or missing items")
		}
		for i, item := range result.Items {
			if item.Index.Status < 200 || item.Index.Status >= 300 || item.Index.ID != documents[start+i].VectorID || (len(item.Index.Error) > 0 && string(item.Index.Error) != "null") {
				return errors.New("Elasticsearch bulk item was not acknowledged")
			}
		}
	}
	return nil
}

func (c *Client) DeleteDocument(ctx context.Context, documentID, userID uint) error {
	return c.DeleteDocumentVersion(ctx, documentID, userID, "")
}

func (c *Client) DeleteDocumentVersion(ctx context.Context, documentID, userID uint, version string) error {
	if documentID == 0 || userID == 0 {
		return errors.New("document deletion requires document and owner identity")
	}
	filters := []any{map[string]any{"term": map[string]any{"document_id": documentID}}, map[string]any{"term": map[string]any{"user_id": userID}}}
	if version != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"version": version}})
	}
	body, err := json.Marshal(map[string]any{"query": map[string]any{"bool": map[string]any{"filter": filters}}})
	if err != nil {
		return err
	}
	res, err := c.client.DeleteByQuery([]string{c.indexName}, bytes.NewReader(body), c.client.DeleteByQuery.WithContext(ctx), c.client.DeleteByQuery.WithRefresh(true))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.IsError() {
		return fmt.Errorf("Elasticsearch document cleanup failed: %s", res.Status())
	}
	var result struct {
		TimedOut         bool              `json:"timed_out"`
		VersionConflicts int               `json:"version_conflicts"`
		Failures         []json.RawMessage `json:"failures"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return err
	}
	if result.TimedOut || result.VersionConflicts != 0 || len(result.Failures) != 0 {
		return errors.New("Elasticsearch document cleanup incomplete")
	}
	return nil
}

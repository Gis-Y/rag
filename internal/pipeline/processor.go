// Package pipeline processes documents exclusively through the structured worker.
package pipeline

import (
	"context"
	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/embedding"
	"pai-smart-go/pkg/storage"
	"pai-smart-go/pkg/tasks"
)

type VectorIndexer interface {
	BulkIndexDocuments(context.Context, []model.EsDocument) error
	DeleteDocumentVersion(context.Context, uint, uint, string) error
}

type Processor struct {
	embeddingClient embedding.BatchClient
	objectStore     *storage.Client
	vectorIndexer   VectorIndexer
	modelVersion    string
	structured      StructuredOptions
}

func NewProcessor(embeddingClient embedding.BatchClient, objectStore *storage.Client, vectorIndexer VectorIndexer, options StructuredOptions) *Processor {
	if embeddingClient == nil || objectStore == nil || vectorIndexer == nil || options.Parser == nil || options.Repository == nil || options.Config.EmbeddingModel == "" {
		panic("document processor requires embedding, storage, index, parser, repository and model identity")
	}
	options.Config = options.Config.WithDefaults()
	return &Processor{embeddingClient: embeddingClient, objectStore: objectStore, vectorIndexer: vectorIndexer, modelVersion: options.Config.EmbeddingModel, structured: options}
}

func (p *Processor) Process(ctx context.Context, task tasks.FileProcessingTask) error {
	return p.processStructured(ctx, task)
}

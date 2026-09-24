package pipeline

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/storage"
	"pai-smart-go/pkg/tasks"
	"strings"
	"time"
)

type DocumentParser interface {
	Parse(context.Context, io.Reader, string) (*model.ParsedDocument, error)
}

type ProcessingRepository interface {
	Begin(context.Context, uint, uint, string, string) (*model.FileUpload, error)
	Stage(context.Context, uint, uint, string, []model.DocumentChunk) error
	Publish(context.Context, uint, uint, string) error
	Fail(context.Context, uint, uint, string, string) error
	DiscardVersion(context.Context, uint, uint, string, func() error) (bool, error)
}

type StructuredOptions struct {
	Config     config.DocumentProcessingConfig
	Parser     DocumentParser
	Repository ProcessingRepository
}

func (p *Processor) processStructured(ctx context.Context, task tasks.FileProcessingTask) (resultErr error) {
	opts := p.structured
	if task.DocumentID == 0 || task.UserID == 0 || opts.Parser == nil || opts.Repository == nil {
		return errors.New("structured processing requires document_id, owner and parser/repository")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(opts.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	version := hex.EncodeToString(random[:])
	file, err := opts.Repository.Begin(ctx, task.DocumentID, task.UserID, task.FileMD5, version)
	if err != nil {
		if errors.Is(err, repository.ErrDocumentChanged) {
			return tasks.MarkProcessingFailurePersisted(err)
		}
		return err
	}
	defer func() {
		if resultErr != nil {
			failureCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			failureErr := opts.Repository.Fail(failureCtx, file.ID, file.UserID, version, resultErr.Error())
			stop()
			persisted := failureErr == nil || errors.Is(failureErr, repository.ErrDocumentChanged)
			if failureErr != nil && !errors.Is(failureErr, repository.ErrDocumentChanged) {
				resultErr = errors.Join(resultErr, fmt.Errorf("persist processing failure: %w", failureErr))
			}

			// ponytail: prolonged external-store outages may leave inactive artifacts;
			// add a durable cleanup queue when recovery across outages is required.
			cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			_, cleanupErr := opts.Repository.DiscardVersion(cleanupCtx, file.ID, file.UserID, version, func() error {
				return errors.Join(
					p.vectorIndexer.DeleteDocumentVersion(cleanupCtx, file.ID, file.UserID, version),
					p.objectStore.Remove(cleanupCtx, storage.DocumentIRObjectKey(file.UserID, file.ID, version)),
				)
			})
			stop()
			if cleanupErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("discard failed document version: %w", cleanupErr))
			}
			if persisted {
				resultErr = tasks.MarkProcessingFailurePersisted(resultErr)
			}
		}
	}()
	// Metadata/ACL comes from SQL, never from a possibly stale Kafka payload.
	key := storage.DocumentObjectKey(file.UserID, file.ID, file.MergeToken)
	object, err := p.objectStore.Get(ctx, key)
	if err != nil {
		return err
	}
	defer object.Close()
	content, err := readStructuredFile(object, file, opts.Config.MaxFileBytes)
	if err != nil {
		return err
	}
	parsed, err := opts.Parser.Parse(ctx, bytes.NewReader(content), file.FileName)
	if err != nil {
		return fmt.Errorf("parse document: %w", err)
	}
	if parsed == nil || len(parsed.Parents) == 0 || len(parsed.Children) == 0 {
		return errors.New("parser returned no parent/child chunks")
	}
	// The immutable artifact includes exact parser output and server-verified hash.
	digest := sha256.Sum256(content)
	artifact, err := json.Marshal(struct {
		DocumentID uint                  `json:"document_id"`
		Version    string                `json:"version"`
		SHA256     string                `json:"sha256"`
		Document   *model.ParsedDocument `json:"document"`
	}{file.ID, version, hex.EncodeToString(digest[:]), parsed})
	if err != nil {
		return err
	}
	if err := p.objectStore.Put(ctx, storage.DocumentIRObjectKey(file.UserID, file.ID, version), bytes.NewReader(artifact), int64(len(artifact))); err != nil {
		return fmt.Errorf("store document IR: %w", err)
	}
	return p.publishStructuredChunks(ctx, file, version, parsed, p.vectorIndexer)
}

func readStructuredFile(reader io.Reader, file *model.FileUpload, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes >= 1<<62 || file.TotalSize <= 0 || file.TotalSize > maxBytes {
		return nil, errors.New("document size is outside processing limit")
	}
	content, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != file.TotalSize || int64(len(content)) > maxBytes {
		return nil, errors.New("document size does not match upload metadata or exceeds limit")
	}
	digest := md5.Sum(content)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), file.FileMD5) {
		return nil, errors.New("document MD5 does not match upload metadata")
	}
	return content, nil
}

func (p *Processor) publishStructuredChunks(ctx context.Context, file *model.FileUpload, version string, parsed *model.ParsedDocument, bulk VectorIndexer) error {
	chunks := make([]model.DocumentChunk, 0, len(parsed.Parents)+len(parsed.Children))
	for _, parent := range parsed.Parents {
		chunks = append(chunks, model.DocumentChunk{DocumentID: file.ID, Version: version, ChunkID: parent.ChunkID, ParentID: parent.ParentID, UserID: file.UserID, IsParent: true, Data: parent})
	}
	for _, child := range parsed.Children {
		chunks = append(chunks, model.DocumentChunk{DocumentID: file.ID, Version: version, ChunkID: child.ChunkID, ParentID: child.ParentID, UserID: file.UserID, Data: child})
	}
	if err := p.structured.Repository.Stage(ctx, file.ID, file.UserID, version, chunks); err != nil {
		return fmt.Errorf("stage document chunks: %w", err)
	}
	// Bound both the provider request and ES bulk size.
	const batchSize = 32
	for offset := 0; offset < len(parsed.Children); offset += batchSize {
		end := min(offset+batchSize, len(parsed.Children))
		texts := make([]string, end-offset)
		for i := offset; i < end; i++ {
			texts[i-offset] = parsed.Children[i].EmbeddingText
		}
		vectors, err := p.embeddingClient.CreateEmbeddings(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed children %d-%d: %w", offset, end-1, err)
		}
		if len(vectors) != len(texts) {
			return errors.New("embedding batch returned missing vectors")
		}
		documents := make([]model.EsDocument, 0, end-offset)
		for i := offset; i < end; i++ {
			child := parsed.Children[i]
			vector := vectors[i-offset]
			if len(vector) == 0 {
				return fmt.Errorf("embedding batch returned an empty vector for child %s", child.ChunkID)
			}
			documents = append(documents, model.EsDocument{
				VectorID: fmt.Sprintf("%d_%s_%s", file.ID, version, child.ChunkID), DocumentID: file.ID,
				Version: version, ParentID: child.ParentID, ChunkKey: child.ChunkID, FileMD5: file.FileMD5,
				FileName: file.FileName, ChunkID: i, TextContent: child.BodyText, ContextPrefix: child.ContextPrefix, Vector: vector,
				ModelVersion: p.modelVersion, UserID: file.UserID, OrgTag: file.OrgTag, IsPublic: file.IsPublic,
				HeadingPath: child.HeadingPath, SourceSpans: child.SourceSpans, TokenCount: child.TokenCount,
				Kind: child.Kind, TableID: child.TableID, RowRange: child.RowRange, ColumnPaths: child.ColumnPaths,
			})
		}
		if err := bulk.BulkIndexDocuments(ctx, documents); err != nil {
			return fmt.Errorf("index document children: %w", err)
		}
	}
	// A refresh-confirmed successful index is not visible to retrieval until CAS.
	return p.structured.Repository.Publish(ctx, file.ID, file.UserID, version)
}

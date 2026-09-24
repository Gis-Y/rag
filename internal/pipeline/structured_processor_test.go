package pipeline

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"pai-smart-go/internal/model"
	"reflect"
	"strings"
	"testing"
)

type structuredTestRepo struct {
	events                 *[]string
	failStage, failPublish bool
}

func (r structuredTestRepo) Begin(context.Context, uint, uint, string, string) (*model.FileUpload, error) {
	return nil, errors.New("unused")
}
func (r structuredTestRepo) Stage(_ context.Context, docID, userID uint, version string, chunks []model.DocumentChunk) error {
	*r.events = append(*r.events, "stage")
	if len(chunks) != 3 || !chunks[0].IsParent || chunks[1].IsParent || chunks[1].ParentID != "p" {
		return errors.New("incorrect staged parent/child mapping")
	}
	if r.failStage {
		return errors.New("stage failed")
	}
	return nil
}
func (r structuredTestRepo) Publish(context.Context, uint, uint, string) error {
	*r.events = append(*r.events, "publish")
	if r.failPublish {
		return errors.New("stale")
	}
	return nil
}
func (r structuredTestRepo) Fail(context.Context, uint, uint, string, string) error { return nil }
func (r structuredTestRepo) DiscardVersion(context.Context, uint, uint, string, func() error) (bool, error) {
	return true, nil
}

type structuredTestEmbedding struct {
	events *[]string
	fail   bool
}

func (e structuredTestEmbedding) CreateEmbeddings(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		*e.events = append(*e.events, "embed:"+text)
		if e.fail {
			return nil, errors.New("embedding failed")
		}
		vectors[i] = []float32{1}
	}
	return vectors, nil
}

type structuredTestIndex struct {
	events *[]string
	fail   bool
}

func (i structuredTestIndex) BulkIndexDocuments(_ context.Context, docs []model.EsDocument) error {
	*i.events = append(*i.events, "bulk")
	if len(docs) != 2 || docs[0].ParentID != "p" || docs[0].Version != "v" || docs[0].UserID != 7 || docs[0].ContextPrefix != "heading\n" || docs[0].TextContent != "a" {
		return errors.New("incorrect indexed metadata")
	}
	if i.fail {
		return errors.New("partial bulk failure")
	}
	return nil
}
func (i structuredTestIndex) DeleteDocumentVersion(context.Context, uint, uint, string) error {
	*i.events = append(*i.events, "delete-version")
	return nil
}

func TestStructuredPipelinePublishesOnlyAfterAllChildrenIndexed(t *testing.T) {
	file := &model.FileUpload{ID: 12, UserID: 7, FileMD5: "md5", FileName: "doc.md"}
	parsed := &model.ParsedDocument{Parents: []model.ParsedChunk{{ChunkID: "p", BodyText: "parent"}}, Children: []model.ParsedChunk{{ChunkID: "a", ParentID: "p", ContextPrefix: "heading\n", BodyText: "a", EmbeddingText: "heading\na"}, {ChunkID: "b", ParentID: "p", ContextPrefix: "heading\n", BodyText: "b", EmbeddingText: "heading\nb"}}}
	for _, mode := range []string{"success", "stage", "embedding", "bulk", "publish"} {
		t.Run(mode, func(t *testing.T) {
			events := []string{}
			repo := structuredTestRepo{&events, mode == "stage", mode == "publish"}
			p := &Processor{modelVersion: "embedding", embeddingClient: structuredTestEmbedding{&events, mode == "embedding"}, structured: StructuredOptions{Repository: repo}}
			err := p.publishStructuredChunks(context.Background(), file, "v", parsed, structuredTestIndex{&events, mode == "bulk"})
			if (err == nil) != (mode == "success") {
				t.Fatalf("%s err=%v", mode, err)
			}
			expected := []string{"stage"}
			if mode != "stage" {
				expected = append(expected, "embed:heading\na")
			}
			if mode != "stage" && mode != "embedding" {
				expected = append(expected, "embed:heading\nb", "bulk")
			}
			if mode == "success" || mode == "publish" {
				expected = append(expected, "publish")
			}
			if !reflect.DeepEqual(events, expected) {
				t.Fatalf("got %v want %v", events, expected)
			}
		})
	}
}

func TestStructuredInputIsBoundedAndVerified(t *testing.T) {
	body := "中文 payload"
	digest := md5.Sum([]byte(body))
	file := &model.FileUpload{TotalSize: int64(len(body)), FileMD5: hex.EncodeToString(digest[:])}
	if got, err := readStructuredFile(strings.NewReader(body), file, 100); err != nil || string(got) != body {
		t.Fatalf("%s %v", got, err)
	}
	for _, body := range []string{"", "same length?", strings.Repeat("x", 101)} {
		if _, err := readStructuredFile(strings.NewReader(body), file, 100); err == nil {
			t.Fatal("unverified input accepted")
		}
	}
	if _, err := readStructuredFile(strings.NewReader(body), file, 1); err == nil {
		t.Fatal("oversize metadata accepted")
	}
}

func TestProcessorRequiresStructuredDependencies(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("missing structured dependencies accepted")
		}
	}()
	NewProcessor(nil, nil, nil, StructuredOptions{})
}

type structuredTestBatchEmbedding struct {
	structuredTestEmbedding
	result [][]float32
	err    error
}

func (e structuredTestBatchEmbedding) CreateEmbeddings(_ context.Context, texts []string) ([][]float32, error) {
	*e.events = append(*e.events, "batch:"+strings.Join(texts, "|"))
	return e.result, e.err
}

func TestStructuredBatchEmbeddingRequiresEveryVectorBeforeIndexAndPublish(t *testing.T) {
	file := &model.FileUpload{ID: 12, UserID: 7, FileMD5: "md5", FileName: "doc.md"}
	parsed := &model.ParsedDocument{Parents: []model.ParsedChunk{{ChunkID: "p"}}, Children: []model.ParsedChunk{{ChunkID: "a", ParentID: "p", ContextPrefix: "heading\n", BodyText: "a", EmbeddingText: "heading\na"}, {ChunkID: "b", ParentID: "p", ContextPrefix: "heading\n", BodyText: "b", EmbeddingText: "heading\nb"}}}
	for _, test := range []struct {
		name    string
		vectors [][]float32
		err     error
		success bool
	}{
		{"complete", [][]float32{{1}, {2}}, nil, true},
		{"missing", [][]float32{{1}}, nil, false},
		{"empty", [][]float32{{1}, nil}, nil, false},
		{"provider error", nil, errors.New("partial response"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			client := structuredTestBatchEmbedding{structuredTestEmbedding: structuredTestEmbedding{events: &events}, result: test.vectors, err: test.err}
			p := &Processor{embeddingClient: client, structured: StructuredOptions{Repository: structuredTestRepo{events: &events}}}
			err := p.publishStructuredChunks(context.Background(), file, "v", parsed, structuredTestIndex{events: &events})
			if (err == nil) != test.success {
				t.Fatalf("err=%v", err)
			}
			want := []string{"stage", "batch:heading\na|heading\nb"}
			if test.success {
				want = append(want, "bulk", "publish")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events=%v want=%v", events, want)
			}
		})
	}
}

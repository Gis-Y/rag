package document

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
)

func validDocument() (*model.ParsedDocument, config.DocumentProcessingConfig) {
	span := model.SourceSpan{BlockID: "b0", Start: 0, End: 5}
	parent := model.ParsedChunk{ChunkID: "p0", Kind: "text", BodyText: "hello", SourceSpans: []model.SourceSpan{span}, BlockIDs: []string{"b0"}, TokenCount: 2}
	child := parent
	child.ChunkID, child.ParentID, child.EmbeddingText = "c0", "p0", "source\nhello"
	child.ContextPrefix = "source\n"
	cfg := config.DocumentProcessingConfig{TokenizerID: "fixture", TokenizerRevision: strings.Repeat("a", 40), EmbeddingModel: "fixture"}
	return &model.ParsedDocument{SchemaVersion: "document-v1", Parser: "text", ParserVersion: "1", ChunkerVersion: "1", TokenizerID: cfg.TokenizerID, TokenizerRevision: cfg.TokenizerRevision, EmbeddingModel: cfg.EmbeddingModel, IR: json.RawMessage(`{"blocks":[{"block_id":"b0","type":"paragraph","text":"hello","heading_path":[],"reading_order":0,"source_spans":[{"block_id":"b0","page_no":null,"bbox":null,"start":0,"end":5}]}],"tables":[]}`), Parents: []model.ParsedChunk{parent}, Children: []model.ParsedChunk{child}}, cfg
}

func TestWorkerContract(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*model.ParsedDocument)
	}{
		{"different tokenizer", func(d *model.ParsedDocument) { d.TokenizerRevision = "main" }},
		{"oversized child", func(d *model.ParsedDocument) { d.Children[0].TokenCount = 601 }},
		{"oversized parent", func(d *model.ParsedDocument) { d.Parents[0].TokenCount = 2001 }},
		{"lost body", func(d *model.ParsedDocument) { d.Children[0].EmbeddingText = "hel" }},
		{"lost prefix", func(d *model.ParsedDocument) { d.Children[0].ContextPrefix = "" }},
		{"changed prefix", func(d *model.ParsedDocument) { d.Children[0].ContextPrefix = "other\n" }},
		{"orphan", func(d *model.ParsedDocument) { d.Children[0].ParentID = "missing" }},
		{"outside source", func(d *model.ParsedDocument) {
			d.Children[0].SourceSpans = []model.SourceSpan{{BlockID: "b0", Start: 0, End: 7}}
		}},
		{"structural overlap", func(d *model.ParsedDocument) { d.Children[0].OverlapSpans = d.Children[0].SourceSpans }},
		{"invalid page", func(d *model.ParsedDocument) {
			page := 0
			d.Children[0].SourceSpans = []model.SourceSpan{{BlockID: "b0", Start: 0, End: 5, PageNo: &page}}
		}},
		{"invalid IR shape", func(d *model.ParsedDocument) { d.IR = json.RawMessage(`[]`) }},
		{"IR source gap", func(d *model.ParsedDocument) {
			d.IR = json.RawMessage(`{"blocks":[{"block_id":"b0","type":"paragraph","text":"hello","heading_path":[],"reading_order":0,"source_spans":[{"block_id":"b0","start":0,"end":4}]}],"tables":[]}`)
		}},
		{"uncovered IR block", func(d *model.ParsedDocument) {
			d.IR = json.RawMessage(`{"blocks":[{"block_id":"b0","type":"paragraph","text":"hello","heading_path":[],"reading_order":0,"source_spans":[{"block_id":"b0","start":0,"end":5}]},{"block_id":"b1","type":"paragraph","text":"lost","heading_path":[],"reading_order":1,"source_spans":[{"block_id":"b1","start":0,"end":4}]}],"tables":[]}`)
		}},
		{"invalid table cell", func(d *model.ParsedDocument) {
			d.IR = json.RawMessage(`{"blocks":[{"block_id":"b0","type":"paragraph","text":"hello","heading_path":[],"reading_order":0,"source_spans":[{"block_id":"b0","start":0,"end":5}]}],"tables":[{"table_id":"t0","block_ids":[],"num_rows":1,"num_cols":1,"cells":[{"cell_id":"cell0","row":1,"column":0,"row_span":1,"col_span":1,"raw_text":"x","source_spans":[]}],"column_paths":[[]]}]}`)
		}},
		{"unknown IR block", func(d *model.ParsedDocument) {
			span := model.SourceSpan{BlockID: "missing", Start: 0, End: 5}
			d.Parents[0].BlockIDs, d.Parents[0].SourceSpans = []string{"missing"}, []model.SourceSpan{span}
			d.Children[0].BlockIDs, d.Children[0].SourceSpans = []string{"missing"}, []model.SourceSpan{span}
		}},
		{"source exceeds IR", func(d *model.ParsedDocument) {
			span := model.SourceSpan{BlockID: "b0", Start: 0, End: 6}
			d.Parents[0].SourceSpans, d.Children[0].SourceSpans = []model.SourceSpan{span}, []model.SourceSpan{span}
		}},
		{"child omits parent tail", func(d *model.ParsedDocument) {
			d.Children[0].BodyText = "hell"
			d.Children[0].EmbeddingText = d.Children[0].ContextPrefix + "hell"
			d.Children[0].SourceSpans = []model.SourceSpan{{BlockID: "b0", Start: 0, End: 4}}
		}},
		{"body differs from IR", func(d *model.ParsedDocument) {
			d.Children[0].BodyText = "other"
			d.Children[0].EmbeddingText = d.Children[0].ContextPrefix + "other"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, cfg := validDocument()
			test.mutate(doc)
			if Validate(doc, cfg) == nil {
				t.Fatal("accepted invalid worker result")
			}
		})
	}
	doc, cfg := validDocument()
	doc.Children[0].TokenCount = 600
	if err := Validate(doc, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerHTTPAndLimits(t *testing.T) {
	doc, cfg := validDocument()
	called := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		name, _ := url.PathUnescape(r.Header.Get("X-File-Name"))
		if name != "样本.md" || r.Method != "POST" || r.URL.Path != "/parse" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("unexpected worker request")
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer server.Close()
	cfg.WorkerURL, cfg.WorkerToken, cfg.MaxFileBytes = server.URL, "fixture-secret", 5
	client := NewClient(cfg)
	if _, err := client.Parse(context.Background(), strings.NewReader("hello"), "样本.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Parse(context.Background(), strings.NewReader("123456"), "样本.md"); err == nil {
		t.Fatal("accepted oversized file")
	}
	if called != 1 {
		t.Fatal("oversized input reached worker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Parse(ctx, strings.NewReader("hello"), "样本.md"); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestWorkerDoesNotFollowRedirect(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("file or secret sent to redirected destination") }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, cfg := validDocument()
	cfg.WorkerURL = server.URL
	if _, err := NewClient(cfg).Parse(context.Background(), strings.NewReader("hello"), "a.txt"); err == nil {
		t.Fatal("accepted redirected parser")
	}
}

// Opt-in cross-language contract check; no OCR weights, live services or source files.
func TestPythonWorkerContract(t *testing.T) {
	python := os.Getenv("DOCUMENT_TEST_PYTHON")
	if python == "" {
		t.Skip("set DOCUMENT_TEST_PYTHON to run the Python/Go contract check")
	}
	workerDir, err := filepath.Abs("../../workers/document")
	if err != nil {
		t.Fatal(err)
	}
	script := `import json, sys
sys.path.insert(0, sys.argv[1])
from worker import parse_text
from chunking import Chunker
from test_worker import CharacterTokenizer
text = '# Chapter\n\n' + '中文 text. ' * 900 + '\n\nNext paragraph.\n\n| Key | Value |\n| --- | --- |\n| item | ' + 'long value ' * 400 + ' |'
ir = parse_text(text.encode(), True)
parents, children = Chunker(CharacterTokenizer(), 'sample.md').chunks(ir)
print(json.dumps(dict(schema_version='document-v1', parser='text', parser_version='1', chunker_version='1', tokenizer_id='fixture', tokenizer_revision='a'*40, embedding_model='fixture', ir=ir, parents=parents, children=children)))`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, python, "-c", script, workerDir).Output()
	if err != nil {
		t.Fatal(err)
	}
	var result model.ParsedDocument
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	_, cfg := validDocument()
	if err := Validate(&result, cfg); err != nil {
		t.Fatal(err)
	}
	if len(result.Parents) < 2 || len(result.Children) < 3 {
		t.Fatal("cross-language sample did not exercise splits")
	}
}

package service

import (
	"strings"
	"testing"
)

func TestDocumentPreviewUsesCompletePublishedIRWithoutChunkOverlap(t *testing.T) {
	ir := `{"document_id":12,"version":"v1","document":{"schema_version":"document-v1","ir":{"blocks":[{"type":"heading","text":"第一章"},{"type":"paragraph","text":"完整段落，不重复子块重叠。"},{"type":"table_row","text":"名称 | 数量"}]},"children":[{"body_text":"不应读取重复子块"}]}}`
	text, err := readDocumentPreview(strings.NewReader(ir), 12, "v1")
	if err != nil || text != "第一章\n\n完整段落，不重复子块重叠。\n\n名称 | 数量" {
		t.Fatalf("preview=%q err=%v", text, err)
	}
	for _, test := range []struct {
		name, data string
		id         uint
		version    string
	}{
		{"wrong owner document", ir, 13, "v1"},
		{"stale version", ir, 12, "v2"},
		{"malformed", "{", 12, "v1"},
		{"missing IR", `{"document_id":12,"version":"v1","document":{"schema_version":"document-v1"}}`, 12, "v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readDocumentPreview(strings.NewReader(test.data), test.id, test.version); err == nil {
				t.Fatal("invalid or unpublished artifact accepted")
			}
		})
	}
}

func TestDocumentServiceRequiresProcessingDependencies(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("missing required dependencies accepted")
		}
	}()
	NewDocumentService(nil, nil, nil, nil, DocumentProcessingOptions{})
}

func TestDocumentPreviewPreservesTableCaptionOnce(t *testing.T) {
	ir := `{"document_id":12,"version":"v1","document":{"schema_version":"document-v1","ir":{"blocks":[{"text":"收入 | 100","table_id":"t1","table_caption":"单位：万元"},{"text":"支出 | 80","table_id":"t1","table_caption":"单位：万元"},{"text":"人数 | 20","table_id":"t2","table_caption":"单位：人"}]}}}`
	text, err := readDocumentPreview(strings.NewReader(ir), 12, "v1")
	if err != nil || text != "单位：万元\n收入 | 100\n\n支出 | 80\n\n单位：人\n人数 | 20" {
		t.Fatalf("table caption lost or duplicated: %q %v", text, err)
	}
}

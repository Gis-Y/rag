package model

import "encoding/json"

// SourceSpan uses zero-based, half-open Unicode character offsets in an IR block.
// Pages are one-based; missing page/bounding-box information stays nil.
type SourceSpan struct {
	BlockID string      `json:"block_id"`
	PageNo  *int        `json:"page_no,omitempty"`
	BBox    *[4]float64 `json:"bbox,omitempty"`
	Start   int         `json:"start"`
	End     int         `json:"end"`
}

// ParsedChunk is independent of storage identity. Go supplies owner/document/version.
type ParsedChunk struct {
	ChunkID       string       `json:"chunk_id"`
	ParentID      string       `json:"parent_id,omitempty"`
	Kind          string       `json:"kind"`
	BodyText      string       `json:"body_text"`
	ContextPrefix string       `json:"context_prefix,omitempty"`
	EmbeddingText string       `json:"embedding_text,omitempty"`
	HeadingPath   []string     `json:"heading_path"`
	SourceSpans   []SourceSpan `json:"source_spans"`
	OverlapSpans  []SourceSpan `json:"overlap_spans"`
	TokenCount    int          `json:"token_count"`
	BlockIDs      []string     `json:"block_ids"`
	TableID       string       `json:"table_id,omitempty"`
	RowRange      []int        `json:"row_range,omitempty"`
	ColumnPaths   [][]string   `json:"column_paths,omitempty"`
	PrevID        string       `json:"prev_id,omitempty"`
	NextID        string       `json:"next_id,omitempty"`
}

// ParsedDocument retains the complete IR alongside bounded retrieval chunks.
type ParsedDocument struct {
	SchemaVersion     string          `json:"schema_version"`
	Parser            string          `json:"parser"`
	ParserVersion     string          `json:"parser_version"`
	TokenizerID       string          `json:"tokenizer_id"`
	TokenizerRevision string          `json:"tokenizer_revision"`
	EmbeddingModel    string          `json:"embedding_model"`
	ChunkerVersion    string          `json:"chunker_version"`
	IR                json.RawMessage `json:"ir"`
	Parents           []ParsedChunk   `json:"parents"`
	Children          []ParsedChunk   `json:"children"`
}

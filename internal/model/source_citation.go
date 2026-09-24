package model

// SourceCitation binds a displayed reference number to server-selected evidence.
// FileName is display-only; document access always uses DocumentID and Version.
type SourceCitation struct {
	Number      int          `json:"number"`
	DocumentID  uint         `json:"documentId"`
	Version     string       `json:"version"`
	FileName    string       `json:"fileName"`
	ChunkKey    string       `json:"chunkKey,omitempty"`
	Kind        string       `json:"kind,omitempty"`
	HeadingPath []string     `json:"headingPath,omitempty"`
	SourceSpans []SourceSpan `json:"sourceSpans,omitempty"`
	TableID     string       `json:"tableId,omitempty"`
	RowRange    []int        `json:"rowRange,omitempty"`
	ColumnPaths [][]string   `json:"columnPaths,omitempty"`
}

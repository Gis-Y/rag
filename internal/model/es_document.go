// Package model 定义了与数据库表对应的 Go 结构体。
package model

// SearchResponseDTO 定义了返回给前端的搜索结果结构。
type SearchResponseDTO struct {
	FileMD5           string       `json:"fileMd5"`
	FileName          string       `json:"fileName"` // 新增：原始文件名
	ChunkID           int          `json:"chunkId"`
	TextContent       string       `json:"textContent"`
	ContextPrefix     string       `json:"contextPrefix,omitempty"`
	Score             float64      `json:"score"` // 新增：搜索得分
	UserID            string       `json:"userId"`
	OrgTag            string       `json:"orgTag"`
	IsPublic          bool         `json:"isPublic"`
	DocumentID        uint         `json:"documentId,omitempty"`
	Version           string       `json:"version,omitempty"`
	ChunkKey          string       `json:"chunkKey,omitempty"`
	ParentID          string       `json:"parentId,omitempty"`
	HeadingPath       []string     `json:"headingPath,omitempty"`
	SourceSpans       []SourceSpan `json:"sourceSpans,omitempty"`
	TokenCount        int          `json:"tokenCount,omitempty"`
	ParentText        string       `json:"-"`
	ParentSourceSpans []SourceSpan `json:"-"`
	Kind              string       `json:"kind,omitempty"`
	TableID           string       `json:"tableId,omitempty"`
	RowRange          []int        `json:"rowRange,omitempty"`
	ColumnPaths       [][]string   `json:"columnPaths,omitempty"`
	ParentRowRange    []int        `json:"-"`
	ParentColumnPaths [][]string   `json:"-"`
}

// EsDocument 代表存储在 Elasticsearch 中的文档结构。
// EsDocument 定义了存储在 Elasticsearch 中的文档结构。
type EsDocument struct {
	VectorID      string       `json:"vector_id"` // 唯一标识，例如 fileMd5 + chunkId
	FileMD5       string       `json:"file_md5"`
	ChunkID       int          `json:"chunk_id"`
	TextContent   string       `json:"text_content"`
	ContextPrefix string       `json:"context_prefix,omitempty"`
	Vector        []float32    `json:"vector"` // 文本内容的向量表示
	ModelVersion  string       `json:"model_version"`
	UserID        uint         `json:"user_id"`
	OrgTag        string       `json:"org_tag"`
	IsPublic      bool         `json:"is_public"`
	DocumentID    uint         `json:"document_id,omitempty"`
	Version       string       `json:"version,omitempty"`
	ChunkKey      string       `json:"chunk_key,omitempty"`
	ParentID      string       `json:"parent_id,omitempty"`
	FileName      string       `json:"file_name,omitempty"`
	HeadingPath   []string     `json:"heading_path,omitempty"`
	SourceSpans   []SourceSpan `json:"source_spans,omitempty"`
	TokenCount    int          `json:"token_count,omitempty"`
	Kind          string       `json:"kind,omitempty"`
	TableID       string       `json:"table_id,omitempty"`
	RowRange      []int        `json:"row_range,omitempty"`
	ColumnPaths   [][]string   `json:"column_paths,omitempty"`
}

// SearchHit 是搜索端口返回的基础命中，避免业务层依赖具体搜索引擎 SDK。
type SearchHit struct {
	Source EsDocument
	Score  float64
}

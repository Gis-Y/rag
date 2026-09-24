// Package document adapts the isolated structured parsing worker.
package document

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
)

const maxResponseBytes = 64 << 20

type Client struct {
	cfg        config.DocumentProcessingConfig
	httpClient *http.Client
}

func NewClient(cfg config.DocumentProcessingConfig) *Client {
	cfg = cfg.WithDefaults()
	return &Client{cfg: cfg, httpClient: &http.Client{
		Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		// File bytes and worker credentials must never follow a redirect.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) Parse(ctx context.Context, file io.Reader, fileName string) (*model.ParsedDocument, error) {
	if file == nil || strings.TrimSpace(fileName) == "" {
		return nil, errors.New("missing document content or name")
	}
	body, err := io.ReadAll(io.LimitReader(file, c.cfg.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read document: %w", err)
	}
	if len(body) == 0 || int64(len(body)) > c.cfg.MaxFileBytes {
		return nil, errors.New("document is empty or exceeds worker upload limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.WorkerURL, "/")+"/parse", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-File-Name", url.PathEscape(filepath.Base(fileName)))
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if c.cfg.WorkerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.WorkerToken)
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("document worker request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("document worker failed with HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("document worker response is too large")
	}
	var result model.ParsedDocument
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid document worker JSON: %w", err)
	}
	if err := Validate(&result, c.cfg); err != nil {
		return nil, err
	}
	return &result, nil
}

var chunkKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)

type documentIR struct {
	Blocks []documentIRBlock `json:"blocks"`
	Tables []documentIRTable `json:"tables"`
}

type documentIRBlock struct {
	BlockID      string             `json:"block_id"`
	Type         string             `json:"type"`
	Text         string             `json:"text"`
	HeadingPath  []string           `json:"heading_path"`
	ReadingOrder *int               `json:"reading_order"`
	SourceSpans  []model.SourceSpan `json:"source_spans"`
	TableID      string             `json:"table_id,omitempty"`
	Runes        []rune             `json:"-"`
}

type documentIRTable struct {
	TableID     string             `json:"table_id"`
	BlockIDs    []string           `json:"block_ids"`
	NumRows     *int               `json:"num_rows"`
	NumCols     *int               `json:"num_cols"`
	Cells       []documentIRCell   `json:"cells"`
	ColumnPaths [][]string         `json:"column_paths"`
	SourceSpans []model.SourceSpan `json:"source_spans"`
}

type documentIRCell struct {
	CellID      string             `json:"cell_id"`
	Row         *int               `json:"row"`
	Column      *int               `json:"column"`
	RowSpan     *int               `json:"row_span"`
	ColumnSpan  *int               `json:"col_span"`
	RawText     *string            `json:"raw_text"`
	SourceSpans []model.SourceSpan `json:"source_spans"`
}

// Validate rejects incompatible workers and broken source/parent relationships.
// Exact tokenization is performed inside the pinned-tokenizer worker, not guessed in Go.
func Validate(doc *model.ParsedDocument, cfg config.DocumentProcessingConfig) error {
	if doc == nil || doc.SchemaVersion != "document-v1" || doc.Parser == "" || doc.ParserVersion == "" || doc.ChunkerVersion == "" {
		return errors.New("document worker schema or parser version is missing")
	}
	if doc.TokenizerID != cfg.TokenizerID || doc.TokenizerRevision != cfg.TokenizerRevision || doc.EmbeddingModel != cfg.EmbeddingModel {
		return errors.New("document worker tokenizer/model does not match backend configuration")
	}
	blocks, err := validateIR(doc.IR)
	if err != nil {
		return err
	}
	if len(doc.Parents) == 0 || len(doc.Children) == 0 || len(doc.Parents)+len(doc.Children) > 20000 {
		return errors.New("invalid document chunk count")
	}
	parents := make(map[string]model.ParsedChunk, len(doc.Parents))
	seen := make(map[string]bool)
	for _, parent := range doc.Parents {
		if seen[parent.ChunkID] || parent.ParentID != "" || len(parent.OverlapSpans) != 0 {
			return errors.New("invalid parent identity or overlap")
		}
		if err := validateChunk(parent, 2000, blocks); err != nil {
			return err
		}
		seen[parent.ChunkID] = true
		parents[parent.ChunkID] = parent
	}
	usedParents := make(map[string]bool)
	childSources := make(map[string][]model.SourceSpan, len(doc.Parents))
	var previous *model.ParsedChunk
	for i := range doc.Children {
		child := &doc.Children[i]
		if seen[child.ChunkID] {
			return errors.New("duplicate chunk identity")
		}
		seen[child.ChunkID] = true
		if err := validateChunk(*child, 600, blocks); err != nil {
			return err
		}
		if child.EmbeddingText != child.ContextPrefix+child.BodyText {
			return errors.New("retrieval text must equal the context prefix and entire child body")
		}
		parent, ok := parents[child.ParentID]
		if !ok {
			return errors.New("child references unknown parent")
		}
		usedParents[child.ParentID] = true
		childSources[child.ParentID] = append(childSources[child.ParentID], child.SourceSpans...)
		for _, span := range child.SourceSpans {
			if !containsSpan(parent.SourceSpans, span) {
				return errors.New("child source falls outside parent")
			}
		}
		if len(child.OverlapSpans) > 0 {
			if previous == nil || previous.ParentID != child.ParentID || len(child.BlockIDs) != 1 || len(previous.BlockIDs) != 1 || child.BlockIDs[0] != previous.BlockIDs[0] {
				return errors.New("overlap crosses parent or structure boundaries")
			}
			for _, span := range child.OverlapSpans {
				if !containsSpan(previous.SourceSpans, span) || !containsSpan(child.SourceSpans, span) {
					return errors.New("invalid overlap source interval")
				}
			}
		}
		previous = child
	}
	if len(usedParents) != len(parents) {
		return errors.New("parent has no retrieval children")
	}
	for id, parent := range parents {
		if !sourceSpansCovered(parent.SourceSpans, childSources[id]) {
			return fmt.Errorf("children do not completely cover parent: %s", id)
		}
	}
	parentSourcesByBlock := make(map[string][]model.SourceSpan)
	for _, parent := range doc.Parents {
		for _, span := range parent.SourceSpans {
			parentSourcesByBlock[span.BlockID] = append(parentSourcesByBlock[span.BlockID], span)
		}
	}
	for id, block := range blocks {
		if block.Type != "heading" && !rangeCovered(parentSourcesByBlock[id], id, 0, len(block.Runes)) {
			return fmt.Errorf("parents do not completely cover IR block: %s", id)
		}
	}
	return nil
}

func validateChunk(chunk model.ParsedChunk, limit int, irBlocks map[string]documentIRBlock) error {
	if !chunkKeyPattern.MatchString(chunk.ChunkID) || chunk.Kind == "" || strings.TrimSpace(chunk.BodyText) == "" || !utf8.ValidString(chunk.BodyText) || !utf8.ValidString(chunk.ContextPrefix) || chunk.TokenCount <= 0 || chunk.TokenCount > limit {
		return fmt.Errorf("invalid chunk identity, text or token limit: %s", chunk.ChunkID)
	}
	if len(chunk.SourceSpans) == 0 || len(chunk.BlockIDs) == 0 {
		return errors.New("chunk source metadata is missing")
	}
	blocks := make(map[string]bool, len(chunk.BlockIDs))
	for _, id := range chunk.BlockIDs {
		if blocks[id] || irBlocks[id].BlockID == "" {
			return errors.New("chunk references duplicate or unknown IR block")
		}
		blocks[id] = true
	}
	spanBlocks := make(map[string]bool, len(blocks))
	for _, span := range chunk.SourceSpans {
		if !blocks[span.BlockID] || validateSourceSpan(span, irBlocks[span.BlockID]) != nil {
			return errors.New("invalid source interval")
		}
		spanBlocks[span.BlockID] = true
	}
	if len(spanBlocks) != len(blocks) {
		return errors.New("chunk block identity has no source interval")
	}
	for _, span := range chunk.OverlapSpans {
		if !blocks[span.BlockID] || validateSourceSpan(span, irBlocks[span.BlockID]) != nil {
			return errors.New("invalid overlap source interval")
		}
	}
	body, err := bodyFromSources(chunk.BlockIDs, chunk.SourceSpans, irBlocks)
	if err != nil || body != chunk.BodyText {
		return errors.New("chunk body does not match its IR source intervals")
	}
	return nil
}

func validateIR(raw json.RawMessage) (map[string]documentIRBlock, error) {
	var ir documentIR
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &ir) != nil || ir.Blocks == nil || ir.Tables == nil || len(ir.Blocks) == 0 {
		return nil, errors.New("document worker omitted complete structured IR")
	}
	blocks := make(map[string]documentIRBlock, len(ir.Blocks))
	for index, block := range ir.Blocks {
		block.Runes = []rune(block.Text)
		if !chunkKeyPattern.MatchString(block.BlockID) || block.Type == "" || len(block.Runes) == 0 || block.HeadingPath == nil || block.ReadingOrder == nil || *block.ReadingOrder != index || len(block.SourceSpans) == 0 {
			return nil, errors.New("invalid IR block structure")
		}
		if _, exists := blocks[block.BlockID]; exists {
			return nil, errors.New("duplicate IR block identity")
		}
		for _, span := range block.SourceSpans {
			if span.BlockID != block.BlockID || validateSourceSpan(span, block) != nil {
				return nil, errors.New("invalid IR block source interval")
			}
		}
		if !rangeCovered(block.SourceSpans, block.BlockID, 0, len(block.Runes)) {
			return nil, errors.New("IR block source intervals do not cover its text")
		}
		blocks[block.BlockID] = block
	}
	tables := make(map[string]map[string]bool, len(ir.Tables))
	for _, table := range ir.Tables {
		if !chunkKeyPattern.MatchString(table.TableID) || table.BlockIDs == nil || table.NumRows == nil || table.NumCols == nil || *table.NumRows < 0 || *table.NumCols < 0 || table.Cells == nil || table.ColumnPaths == nil || table.SourceSpans == nil || len(table.ColumnPaths) != *table.NumCols {
			return nil, errors.New("invalid IR table structure")
		}
		for _, span := range table.SourceSpans {
			if span.Start < 0 || span.End < span.Start || validateSourceCoordinates(span) != nil {
				return nil, errors.New("invalid IR table source interval")
			}
		}
		if _, exists := tables[table.TableID]; exists {
			return nil, errors.New("duplicate IR table identity")
		}
		tables[table.TableID] = make(map[string]bool, len(table.BlockIDs))
		for _, id := range table.BlockIDs {
			block, exists := blocks[id]
			if !exists || block.TableID != table.TableID || block.Type != "table_row" || tables[table.TableID][id] {
				return nil, errors.New("invalid IR table block relationship")
			}
			tables[table.TableID][id] = true
		}
		cells := make(map[string]bool, len(table.Cells))
		for _, cell := range table.Cells {
			if !chunkKeyPattern.MatchString(cell.CellID) || cells[cell.CellID] || cell.Row == nil || cell.Column == nil || cell.RowSpan == nil || cell.ColumnSpan == nil || cell.RawText == nil || cell.SourceSpans == nil || *cell.Row < 0 || *cell.Column < 0 || *cell.RowSpan < 1 || *cell.ColumnSpan < 1 || *cell.RowSpan > *table.NumRows || *cell.ColumnSpan > *table.NumCols || *cell.Row > *table.NumRows-*cell.RowSpan || *cell.Column > *table.NumCols-*cell.ColumnSpan {
				return nil, errors.New("invalid IR table cell structure")
			}
			cells[cell.CellID] = true
			matchedText := *cell.RawText == ""
			for _, span := range cell.SourceSpans {
				block, exists := blocks[span.BlockID]
				if !exists || block.TableID != table.TableID || span.Start < 0 || span.End < span.Start || span.End > len(block.Runes) || (*cell.RawText == "" && span.Start != span.End) || validateSourceCoordinates(span) != nil || (span.Start < span.End && validateSourceSpan(span, block) != nil) {
					return nil, errors.New("invalid IR table cell source interval")
				}
				if span.Start < span.End && string(block.Runes[span.Start:span.End]) == *cell.RawText {
					matchedText = true
				}
			}
			if !matchedText {
				return nil, errors.New("IR table cell text does not match its source")
			}
		}
	}
	for id, block := range blocks {
		if block.TableID != "" && !tables[block.TableID][id] {
			return nil, errors.New("IR table row is absent from its table")
		}
	}
	return blocks, nil
}

func validateSourceSpan(span model.SourceSpan, block documentIRBlock) error {
	if span.BlockID == "" || span.Start < 0 || span.End <= span.Start || span.End > len(block.Runes) || validateSourceCoordinates(span) != nil {
		return errors.New("invalid source interval")
	}
	return nil
}

func validateSourceCoordinates(span model.SourceSpan) error {
	if span.PageNo != nil && *span.PageNo < 1 {
		return errors.New("invalid source page")
	}
	if span.BBox != nil {
		if span.PageNo == nil {
			return errors.New("source coordinates require a page")
		}
		for _, value := range span.BBox {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				return errors.New("invalid source coordinates")
			}
		}
		if span.BBox[0] > span.BBox[2] || span.BBox[1] > span.BBox[3] {
			return errors.New("inverted source coordinates")
		}
	}
	return nil
}

func bodyFromSources(ids []string, spans []model.SourceSpan, blocks map[string]documentIRBlock) (string, error) {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		ranges := make([]model.SourceSpan, 0)
		for _, span := range spans {
			if span.BlockID == id {
				ranges = append(ranges, span)
			}
		}
		sort.Slice(ranges, func(i, j int) bool {
			if ranges[i].Start == ranges[j].Start {
				return ranges[i].End < ranges[j].End
			}
			return ranges[i].Start < ranges[j].Start
		})
		if len(ranges) == 0 {
			return "", errors.New("missing source interval")
		}
		start, end := ranges[0].Start, ranges[0].End
		for _, current := range ranges[1:] {
			if current.Start > end {
				return "", errors.New("non-contiguous source intervals")
			}
			end = max(end, current.End)
		}
		parts = append(parts, string(blocks[id].Runes[start:end]))
	}
	return strings.Join(parts, "\n\n"), nil
}

func rangeCovered(spans []model.SourceSpan, blockID string, start, end int) bool {
	ranges := make([]model.SourceSpan, 0)
	for _, span := range spans {
		if span.BlockID == blockID && span.End > start && span.Start < end {
			ranges = append(ranges, span)
		}
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	cursor := start
	for _, span := range ranges {
		if span.Start > cursor {
			return false
		}
		cursor = max(cursor, span.End)
		if cursor >= end {
			return true
		}
	}
	return cursor >= end
}

func sourceSpansCovered(targets, covers []model.SourceSpan) bool {
	type spanKey struct {
		block   string
		page    int
		hasPage bool
	}
	key := func(span model.SourceSpan) spanKey {
		value := spanKey{block: span.BlockID}
		if span.PageNo != nil {
			value.page, value.hasPage = *span.PageNo, true
		}
		return value
	}
	grouped := make(map[spanKey][]model.SourceSpan)
	for _, span := range covers {
		grouped[key(span)] = append(grouped[key(span)], span)
	}
	for group := range grouped {
		sort.Slice(grouped[group], func(i, j int) bool { return grouped[group][i].Start < grouped[group][j].Start })
	}
	for _, target := range targets {
		cursor := target.Start
		for _, span := range grouped[key(target)] {
			if span.End <= target.Start || span.Start >= target.End {
				continue
			}
			if span.Start > cursor {
				break
			}
			cursor = max(cursor, span.End)
		}
		if cursor < target.End {
			return false
		}
	}
	return true
}

func containsSpan(spans []model.SourceSpan, target model.SourceSpan) bool {
	for _, span := range spans {
		if span.BlockID == target.BlockID && span.Start <= target.Start && span.End >= target.End && samePage(span.PageNo, target.PageNo) {
			return true
		}
	}
	return false
}

func samePage(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

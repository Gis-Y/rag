package service

import (
	"strings"
	"testing"

	"pai-smart-go/internal/model"
)

func TestTableSourcesRetainColumnsAndParentRowScope(t *testing.T) {
	hit := model.SearchResponseDTO{
		DocumentID: 1, Version: "v1", ChunkKey: "c0", ParentID: "p0", UserID: "7", FileName: "table.pdf", Kind: "row_group", TableID: "t0",
		TextContent: "10", SourceSpans: []model.SourceSpan{{BlockID: "b1", Start: 0, End: 2}},
		RowRange: []int{1, 2}, ColumnPaths: [][]string{{"Income", "USD"}},
		ParentText: "10\n20", ParentSourceSpans: []model.SourceSpan{{BlockID: "b1", Start: 0, End: 2}, {BlockID: "b2", Start: 0, End: 2}},
		ParentRowRange: []int{1, 3}, ParentColumnPaths: [][]string{{"Income", "USD"}},
	}
	child := sourceMetadataText(hit, false)
	parent := sourceMetadataText(hit, true)
	for _, text := range []string{child, parent} {
		if !strings.Contains(text, `"table_id":"t0"`) || !strings.Contains(text, `"column_paths":[["Income","USD"]]`) {
			t.Fatal("lost table identity or unit", text)
		}
	}
	if !strings.Contains(child, `"row_range":[1,2]`) || !strings.Contains(parent, `"row_range":[1,3]`) {
		t.Fatal("parent incorrectly cited only the child row range")
	}
}

package service

import (
	"strings"

	"pai-smart-go/internal/model"
)

const maxParentContextTokens = 3000

type parentContextKey struct {
	Owner, Version, ParentID string
	DocumentID               uint
}

func parentKey(hit model.SearchResponseDTO) parentContextKey {
	return parentContextKey{Owner: hit.UserID, Version: hit.Version, ParentID: hit.ParentID, DocumentID: hit.DocumentID}
}

// Bounded across all subqueries, not once per independently retrieved branch.
func selectedParentContexts(results []model.SearchResponseDTO) ([]model.SearchResponseDTO, map[parentContextKey]int) {
	var parents []model.SearchResponseDTO
	numbers := make(map[parentContextKey]int)
	remaining := maxParentContextTokens
	for _, hit := range results {
		key := parentKey(hit)
		if key.DocumentID == 0 || key.ParentID == "" || key.Version == "" || hit.ParentText == "" || len(hit.ParentSourceSpans) == 0 || numbers[key] != 0 {
			continue
		}
		cost := estimateTextTokens(hit.ParentText)
		if cost > remaining {
			continue
		}
		remaining -= cost
		parents = append(parents, hit)
		numbers[key] = len(results) + len(parents)
	}
	return parents, numbers
}

func sourceCitations(results []model.SearchResponseDTO) []model.SourceCitation {
	parents, _ := selectedParentContexts(results)
	sources := make([]model.SourceCitation, 0, len(results)+len(parents))
	add := func(number int, hit model.SearchResponseDTO, parent bool) {
		if hit.DocumentID == 0 || hit.Version == "" {
			return // Missing identity is never repaired using a display name.
		}
		source := model.SourceCitation{Number: number, DocumentID: hit.DocumentID, Version: hit.Version, FileName: hit.FileName,
			ChunkKey: hit.ChunkKey, Kind: hit.Kind, HeadingPath: hit.HeadingPath, SourceSpans: hit.SourceSpans,
			TableID: hit.TableID, RowRange: hit.RowRange, ColumnPaths: hit.ColumnPaths}
		if parent {
			source.ChunkKey, source.Kind, source.SourceSpans = hit.ParentID, "parent_context", hit.ParentSourceSpans
			source.RowRange, source.ColumnPaths = hit.ParentRowRange, hit.ParentColumnPaths
		}
		sources = append(sources, source)
	}
	for i, hit := range results {
		add(i+1, hit, false)
	}
	for i, hit := range parents {
		add(len(results)+i+1, hit, true)
	}
	return sources
}

func parentCoversChild(parent, child model.SearchResponseDTO) bool {
	if len(child.SourceSpans) == 0 || !strings.Contains(parent.ParentText, child.TextContent) {
		return false
	}
	for _, childSpan := range child.SourceSpans {
		covered := false
		for _, parentSpan := range parent.ParentSourceSpans {
			if childSpan.BlockID != "" && parentSpan.BlockID == childSpan.BlockID && parentSpan.Start <= childSpan.Start && parentSpan.End >= childSpan.End &&
				((parentSpan.PageNo == nil && childSpan.PageNo == nil) || (parentSpan.PageNo != nil && childSpan.PageNo != nil && *parentSpan.PageNo == *childSpan.PageNo)) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// fitAnswerEvidence drops parent expansions before low-ranked optional sources.
// The callback assembles the complete answer prompt and checks the shared budget.
// No source text is shortened here: a binding quote must remain visible.
func (s *chatService) fitAnswerEvidence(execution queryExecution, fits func(queryExecution) bool) (queryExecution, error) {
	if fits(execution) {
		return execution, nil
	}
	fitted := execution
	fitted.Results = append([]model.SearchResponseDTO(nil), execution.Results...)
	fitted.Steps = append([]queryStepResult(nil), execution.Steps...)
	var parentOrder []parentContextKey
	seenParents := make(map[parentContextKey]bool)
	for _, hit := range fitted.Results {
		key := parentKey(hit)
		if hit.ParentText != "" && !seenParents[key] {
			parentOrder = append(parentOrder, key)
			seenParents[key] = true
		}
	}
	// Parent expansion is optional even when its child is mandatory. Remove a
	// whole parent's expansion before touching any original child or bridge quote.
	for i := len(parentOrder) - 1; i >= 0; i-- {
		key := parentOrder[i]
		for j := range fitted.Results {
			if parentKey(fitted.Results[j]) == key {
				fitted.Results[j].ParentText = ""
				fitted.Results[j].ParentSourceSpans = nil
			}
		}
		if fits(fitted) {
			return fitted, nil
		}
	}
	for i := len(fitted.Results) - 1; i >= 0; i-- {
		if fitted.mandatorySources[resultKey(fitted.Results[i])] {
			continue
		}
		fitted.Results = append(fitted.Results[:i], fitted.Results[i+1:]...)
		fitted.rebuildSourceNumbers()
		if fits(fitted) {
			return fitted, nil
		}
	}
	return fitted, ErrContextBudget
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/llm"
)

type documentSearchRepo struct {
	states  func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error)
	parents func(context.Context, []model.ParentRef, uint, []string) ([]model.DocumentChunk, error)
}

func (r documentSearchRepo) FindStates(ctx context.Context, ids []uint) (map[uint]model.DocumentProcessingState, error) {
	return r.states(ctx, ids)
}

func (r documentSearchRepo) FindParents(ctx context.Context, refs []model.ParentRef, user uint, tags []string) ([]model.DocumentChunk, error) {
	return r.parents(ctx, refs, user, tags)
}

func testDocumentSearchOptions(states map[uint]model.DocumentProcessingState) DocumentSearchOptions {
	return DocumentSearchOptions{ModelVersion: "embedding-v1", Repository: documentSearchRepo{
		states: func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error) { return states, nil },
		parents: func(context.Context, []model.ParentRef, uint, []string) ([]model.DocumentChunk, error) {
			return nil, nil
		},
	}}
}

func TestDocumentSearchRequiresRepositoryAndModelVersion(t *testing.T) {
	for _, options := range []DocumentSearchOptions{{}, {ModelVersion: "embedding-v1"}, {Repository: documentSearchRepo{}}, {Repository: documentSearchRepo{}, ModelVersion: " \t"}} {
		if search, err := NewSearchService(nil, nil, nil, nil, options); search != nil || err == nil {
			t.Fatalf("invalid search configuration accepted: %+v %v", options, err)
		}
	}
}

func TestDocumentSearchRejectsOldAndUnpublishedCandidates(t *testing.T) {
	for _, retry := range []bool{false, true} {
		calls := 0
		index := authorizedSearchIndex(func(ctx context.Context, body io.Reader) ([]model.SearchHit, error) {
			calls++
			var query map[string]json.RawMessage
			if err := json.NewDecoder(body).Decode(&query); err != nil {
				return nil, err
			}
			for _, section := range []string{"knn", "query"} {
				encoded := string(query[section])
				for _, want := range []string{`"document_id":11`, `"version":"v1"`, `"model_version":"embedding-v1"`, `"user_id":7`, `"file_md5":"same"`} {
					if !strings.Contains(encoded, want) {
						t.Errorf("missing publication/ACL guard in attempt %d %s: %s", calls, section, encoded)
					}
				}
				for _, forbidden := range []string{`"exists"`, `"document_id":12`, `"document_id":13`} {
					if strings.Contains(encoded, forbidden) {
						t.Errorf("old or unpublished document entered filter: %s", encoded)
					}
				}
			}
			if retry && calls == 1 {
				return nil, nil
			}
			return []model.SearchHit{
				{Source: model.EsDocument{UserID: 7, FileMD5: "same", DocumentID: 11, Version: "v1", ModelVersion: "embedding-v1", TextContent: "published"}},
				{Source: model.EsDocument{UserID: 7, FileMD5: "same", ModelVersion: "embedding-v1", TextContent: "legacy"}},
				{Source: model.EsDocument{UserID: 7, FileMD5: "same", DocumentID: 11, ModelVersion: "embedding-v1", TextContent: "identity-only candidate"}},
				{Source: model.EsDocument{UserID: 7, FileMD5: "same", Version: "failed", ModelVersion: "embedding-v1", TextContent: "version-only candidate"}},
				{Source: model.EsDocument{UserID: 7, FileMD5: "same", DocumentID: 11, Version: "pending", ModelVersion: "embedding-v1", TextContent: "unpublished structured candidate"}},
				{Source: model.EsDocument{UserID: 8, FileMD5: "same", DocumentID: 11, Version: "v1", ModelVersion: "embedding-v1", TextContent: "other owner"}},
				{Source: model.EsDocument{UserID: 7, FileMD5: "missing-state", DocumentID: 12, Version: "v1", ModelVersion: "embedding-v1", TextContent: "missing state"}},
				{Source: model.EsDocument{UserID: 7, FileMD5: "no-active", DocumentID: 13, Version: "v1", ModelVersion: "embedding-v1", TextContent: "no active version"}},
			}, nil
		})
		uploads := contextUploadRepo{find: func(context.Context) ([]model.FileUpload, error) {
			return []model.FileUpload{{ID: 11, UserID: 7, FileMD5: "same", FileName: "same.pdf"}, {ID: 12, UserID: 7, FileMD5: "missing-state"}, {ID: 13, UserID: 7, FileMD5: "no-active"}}, nil
		}}
		s, err := NewSearchService(contextEmbedding{}, index, testCurrentUserService(7, "alice", ""), uploads, testDocumentSearchOptions(map[uint]model.DocumentProcessingState{11: {ActiveVersion: "v1"}, 13: {PendingVersion: "v1"}}))
		if err != nil {
			t.Fatal(err)
		}
		hits, err := s.HybridSearch(context.Background(), "请问这个项目是什么？", 5, &model.User{ID: 7, Username: "alice"})
		if err != nil || len(hits) != 1 || hits[0].TextContent != "published" || (!retry && calls != 1) || (retry && calls != 2) {
			t.Fatalf("search exposed old/unpublished candidate or lost published result: %+v %v retry=%v calls=%d", hits, err, retry, calls)
		}
	}
}

func TestVersionedSearchAccessWithoutActiveStateMatchesNothing(t *testing.T) {
	file := model.FileUpload{ID: 11, UserID: 7, FileMD5: "doc"}
	for _, states := range []map[uint]model.DocumentProcessingState{nil, {11: {PendingVersion: "v1", Status: "failed"}}} {
		encoded, err := json.Marshal(buildVersionedSearchAccess([]model.FileUpload{file}, states))
		if err != nil || string(encoded) != `{"match_none":{}}` {
			t.Fatalf("documents without published state must have an empty search filter: %s %v", encoded, err)
		}
		for _, hit := range []model.EsDocument{{}, {DocumentID: 11, Version: "v1"}} {
			if activeSearchHit(hit, file, states) {
				t.Fatalf("hit without active publication was accepted: %+v", hit)
			}
		}
	}
}

func TestDocumentSearchFiltersPublicationModelAndParentAccess(t *testing.T) {
	files := []model.FileUpload{{ID: 11, UserID: 7, FileMD5: "ready", FileName: "真实名称.pdf"}, {ID: 12, UserID: 7, FileMD5: "pending"}, {ID: 13, UserID: 7, FileMD5: "legacy"}}
	states := map[uint]model.DocumentProcessingState{11: {ActiveVersion: "v1", PendingVersion: "v2", Status: "failed"}, 12: {PendingVersion: "unpublished"}}
	page := 3
	span := model.SourceSpan{BlockID: "b1", PageNo: &page, End: 2}
	active := model.EsDocument{UserID: 7, DocumentID: 11, FileMD5: "ready", FileName: "伪造名", Version: "v1", ModelVersion: "embedding-v1", ParentID: "p1", ChunkKey: "c1", TextContent: "命中", ContextPrefix: "单位：万元\n", HeadingPath: []string{"第一章"}, SourceSpans: []model.SourceSpan{span}, TokenCount: 2}
	stateCalls, parentCalls := 0, 0
	repo := documentSearchRepo{states: func(ctx context.Context, ids []uint) (map[uint]model.DocumentProcessingState, error) {
		stateCalls++
		if !reflect.DeepEqual(ids, []uint{11, 12, 13}) {
			t.Fatalf("wrong document scope: %v", ids)
		}
		return states, nil
	}, parents: func(ctx context.Context, refs []model.ParentRef, user uint, tags []string) ([]model.DocumentChunk, error) {
		parentCalls++
		if user != 7 || !reflect.DeepEqual(refs, []model.ParentRef{{DocumentID: 11, Version: "v1", ParentID: "p1"}}) {
			t.Fatalf("unscoped parent request: %v %d", refs, user)
		}
		return []model.DocumentChunk{{DocumentID: 11, UserID: 7, Version: "v1", ChunkID: "p1", IsParent: true, Data: model.ParsedChunk{BodyText: "命中和补充", ContextPrefix: "单位：万元\n", SourceSpans: []model.SourceSpan{{BlockID: "b1", PageNo: &page, End: 5}}}}}, nil
	}}
	index := authorizedSearchIndex(func(ctx context.Context, body io.Reader) ([]model.SearchHit, error) {
		var query map[string]json.RawMessage
		if err := json.NewDecoder(body).Decode(&query); err != nil {
			return nil, err
		}
		for _, section := range []string{"knn", "query"} {
			encoded := string(query[section])
			for _, want := range []string{`"version":"v1"`, `"document_id":11`, `"user_id":7`, `"model_version":"embedding-v1"`} {
				if !strings.Contains(encoded, want) {
					t.Errorf("missing %s in %s", want, encoded)
				}
			}
			if strings.Contains(encoded, "unpublished") || strings.Contains(encoded, `"version":"v2"`) || strings.Contains(encoded, `"document_id":12`) || strings.Contains(encoded, `"file_md5":"legacy"`) {
				t.Fatalf("pending version leaked into filter: %s", encoded)
			}
		}
		hits := []model.SearchHit{{Source: active}, {Source: model.EsDocument{UserID: 7, FileMD5: "legacy", ModelVersion: "embedding-v1", TextContent: "旧版"}}}
		for _, mutate := range []func(*model.EsDocument){
			func(d *model.EsDocument) { d.Version = "v2" },
			func(d *model.EsDocument) { d.DocumentID = 99 },
			func(d *model.EsDocument) { d.ModelVersion = "wrong-model" },
			func(d *model.EsDocument) { d.UserID = 8 },
			func(d *model.EsDocument) { d.FileMD5 = "legacy" },
			func(d *model.EsDocument) { d.DocumentID, d.FileMD5, d.Version = 12, "pending", "unpublished" },
		} {
			bad := active
			mutate(&bad)
			hits = append(hits, model.SearchHit{Source: bad})
		}
		return hits, nil
	})
	uploads := contextUploadRepo{find: func(context.Context) ([]model.FileUpload, error) { return files, nil }}
	s, err := NewSearchService(contextEmbedding{}, index, testCurrentUserService(7, "alice", ""), uploads, DocumentSearchOptions{Repository: repo, ModelVersion: "embedding-v1"})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.HybridSearch(context.Background(), "检索", 10, &model.User{ID: 7, Username: "alice"})
	if err != nil || len(hits) != 1 || stateCalls != 2 || parentCalls != 1 {
		t.Fatalf("publication/ACL filter failed: %+v %v state=%d parents=%d", hits, err, stateCalls, parentCalls)
	}
	if hits[0].FileName != files[0].FileName || hits[0].ParentText != "单位：万元\n命中和补充" || hits[0].ContextPrefix != active.ContextPrefix || hits[0].TextContent != "命中" || !reflect.DeepEqual(hits[0].SourceSpans, []model.SourceSpan{span}) || !reflect.DeepEqual(hits[0].HeadingPath, active.HeadingPath) {
		t.Fatalf("metadata or original child changed: %+v", hits[0])
	}
}

func TestDocumentSearchRechecksActiveVersionAndParentFailure(t *testing.T) {
	for _, mode := range []string{"changed", "failed", "canceled", "foreign owner", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repo := documentSearchRepo{states: func(context.Context, []uint) (map[uint]model.DocumentProcessingState, error) {
				calls++
				version := "v1"
				if mode == "changed" && calls > 1 {
					version = "v2"
				}
				return map[uint]model.DocumentProcessingState{11: {ActiveVersion: version}}, nil
			}, parents: func(context.Context, []model.ParentRef, uint, []string) ([]model.DocumentChunk, error) {
				if mode == "canceled" {
					cancel()
				}
				if mode == "foreign owner" || mode == "oversized" {
					parent := model.DocumentChunk{DocumentID: 11, UserID: 8, Version: "v1", ChunkID: "p1", IsParent: true, Data: model.ParsedChunk{BodyText: "foreign", SourceSpans: []model.SourceSpan{{BlockID: "b", End: 7}}}}
					if mode == "oversized" {
						parent.UserID, parent.Data.BodyText = 7, strings.Repeat("大", 1001)
					}
					return []model.DocumentChunk{parent}, nil
				}
				return nil, errors.New("parent unavailable")
			}}
			index := authorizedSearchIndex(func(context.Context, io.Reader) ([]model.SearchHit, error) {
				return []model.SearchHit{{Source: model.EsDocument{DocumentID: 11, UserID: 7, FileMD5: "a", Version: "v1", ModelVersion: "embedding-v1", ParentID: "p1", TextContent: "child"}}}, nil
			})
			uploads := contextUploadRepo{find: func(context.Context) ([]model.FileUpload, error) {
				return []model.FileUpload{{ID: 11, UserID: 7, FileMD5: "a"}}, nil
			}}
			s, err := NewSearchService(contextEmbedding{}, index, testCurrentUserService(7, "alice", ""), uploads, DocumentSearchOptions{Repository: repo, ModelVersion: "embedding-v1"})
			if err != nil {
				t.Fatal(err)
			}
			hits, err := s.HybridSearch(ctx, "query", 5, &model.User{ID: 7, Username: "alice"})
			if mode == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				return
			}
			if err != nil || (mode == "changed" && len(hits) != 0) || (mode != "changed" && (len(hits) != 1 || hits[0].ParentText != "" || hits[0].TextContent != "child")) {
				t.Fatalf("unsafe parent or lost child fallback: %+v %v", hits, err)
			}
		})
	}
}

func TestParentContextDedupCitationsAndBudgetPriority(t *testing.T) {
	page := 2
	one := testHit("7", "doc", "第一段", 0)
	one.DocumentID, one.Version, one.ParentID, one.ChunkKey = 11, "v1", "p1", "c1"
	one.HeadingPath = []string{"章", "节"}
	one.SourceSpans = []model.SourceSpan{{BlockID: "b1", PageNo: &page, End: 3}}
	one.ParentText = "第一段\n第二段\n新增父块事实"
	one.ParentSourceSpans = []model.SourceSpan{{BlockID: "b1", PageNo: &page, End: 3}, {BlockID: "b2", PageNo: &page, End: 3}, {BlockID: "b3", PageNo: &page, End: 6}}
	two := one
	two.TextContent, two.ChunkID, two.ChunkKey = "第二段", 1, "c2"
	two.SourceSpans = []model.SourceSpan{{BlockID: "b2", PageNo: &page, End: 3}}
	execution := queryExecution{Results: []model.SearchResponseDTO{one, two}, Steps: []queryStepResult{{QueryStep: QueryStep{ID: 1}, Hits: []model.SearchResponseDTO{one, two}}}}
	execution.protectSources([]model.SearchResponseDTO{two})
	execution.rebuildSourceNumbers()
	var s chatService
	text := s.buildContextText(execution.Results)
	for _, want := range []string{`(来源#3: doc.txt)`, `"kind":"parent_context"`, `"heading_path":["章","节"]`, `"page_no":2`, `"block_id":"b3"`} {
		if !strings.Contains(text, want) {
			t.Errorf("citation metadata missing %s: %s", want, text)
		}
	}
	if strings.Count(text, "第一段") != 1 || strings.Count(text, "第二段") != 1 || strings.Count(text, "新增父块事实") != 1 {
		t.Fatalf("parent/child body duplicated: %s", text)
	}
	fitted, err := s.fitAnswerEvidence(execution, func(candidate queryExecution) bool {
		if len(candidate.Results) != 2 {
			t.Fatal("child removed before parent expansion")
		}
		return !strings.Contains(s.buildContextText(candidate.Results), "新增父块事实")
	})
	if err != nil || fitted.Results[0].TextContent != one.TextContent || fitted.Results[1].TextContent != two.TextContent || execution.Results[0].ParentText == "" {
		t.Fatalf("child evidence changed or caller mutated: %+v %v", fitted, err)
	}
	if strings.Contains(s.buildContextText(fitted.Results), "来源#3") {
		t.Fatal("removed parent retained a citation")
	}
	long := testHit("7", "long", strings.Repeat("a", 1200)+"完整尾部", 1)
	if !strings.Contains(s.buildContextText([]model.SearchResponseDTO{long}), "完整尾部") {
		t.Fatal("new chunk still has the obsolete 1000-rune cutoff")
	}
	newVersion := one
	newVersion.Version = "v2"
	if resultKey(one) == resultKey(newVersion) {
		t.Fatal("versions conflated in query fusion")
	}
	newDocument := one
	newDocument.DocumentID = 12
	if resultKey(one) == resultKey(newDocument) {
		t.Fatal("document incarnations conflated in query fusion")
	}
	missingSpans := one
	missingSpans.ParentSourceSpans = nil
	if parentCoversChild(missingSpans, one) {
		t.Fatal("child text hidden without parent provenance")
	}
	oversized := one
	oversized.ParentText = strings.Repeat("中", 1001)
	if parents, _ := selectedParentContexts([]model.SearchResponseDTO{oversized}); len(parents) != 0 {
		t.Fatal("parent expansion exceeded conservative total budget")
	}
	first, second := one, two
	first.ParentText = strings.Repeat("a", 2000)
	second.ParentText, second.ParentID = strings.Repeat("b", 2000), "p2"
	if parents, _ := selectedParentContexts([]model.SearchResponseDTO{first, second}); len(parents) != 1 {
		t.Fatal("parent budget applied per branch instead of per final context")
	}
}

func TestDependencyBindingKeepsLongChildTail(t *testing.T) {
	text := strings.Repeat("a", 1100) + "负责人是张明。"
	s := testChat(&queryTestRepo{}, func(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
		return []model.SearchResponseDTO{testHit("7", "doc", text, 1)}, nil
	}, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
		if !strings.Contains(messages[1].Content, "负责人是张明。") {
			t.Fatal("dependency binding silently truncated child tail")
		}
		return emitJSON(emit, map[string]any{"steps": []queryBinding{{ID: 2, Evidence: []bindingQuote{{SourceID: "s1:1", Quote: "负责人是张明。", Value: "张明"}}}}})
	})
	execution, err := s.executeQueryPlan(context.Background(), QueryPlan{StandaloneQuestion: "负责人履历", Steps: []QueryStep{{ID: 1, Question: "负责人"}, {ID: 2, Question: "{{step:1}}履历", DependsOn: []int{1}}}}, &model.User{ID: 7})
	if err != nil || execution.Steps[1].Status != "retrieved" || execution.Steps[1].Question != "张明履历" {
		t.Fatalf("complete child no longer binds: %+v %v", execution, err)
	}
}

func TestDependencyBindingSeesContextPrefixButQuotesOnlyBody(t *testing.T) {
	for _, prefixQuote := range []bool{false, true} {
		s := testChat(&queryTestRepo{}, func(context.Context, string, int, *model.User) ([]model.SearchResponseDTO, error) {
			return []model.SearchResponseDTO{{UserID: "7", DocumentID: 11, Version: "v1", TextContent: "负责人是张明。", ContextPrefix: "项目：宽表续块单位万元\n"}}, nil
		}, func(_ context.Context, messages []llm.Message, _ *llm.GenerationParams, emit llm.ChunkHandler) error {
			if !strings.Contains(messages[1].Content, `"context_prefix":"项目：宽表续块单位万元\n"`) || !strings.Contains(messages[1].Content, `"text":"负责人是张明。"`) {
				t.Fatalf("binder lost separate metadata/body: %s", messages[1].Content)
			}
			quote, value := "负责人是张明。", "张明"
			if prefixQuote {
				quote, value = "项目：宽表续块单位万元", "宽表续块"
			}
			return emitJSON(emit, map[string]any{"steps": []queryBinding{{ID: 2, Evidence: []bindingQuote{{SourceID: "s1:1", Quote: quote, Value: value}}}}})
		})
		execution, err := s.executeQueryPlan(context.Background(), QueryPlan{StandaloneQuestion: "负责人履历", Steps: []QueryStep{{ID: 1, Question: "负责人"}, {ID: 2, Question: "{{step:1}}履历", DependsOn: []int{1}}}}, &model.User{ID: 7})
		want := "retrieved"
		if prefixQuote {
			want = "failed_binding"
		}
		if err != nil || execution.Steps[1].Status != want {
			t.Fatalf("prefix-only binding accepted or body binding failed: %+v %v", execution, err)
		}
	}
}

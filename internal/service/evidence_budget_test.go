package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/llm"
)

func TestFitAnswerEvidenceRetainsBridgeAndBranches(t *testing.T) {
	parent := testHit("7", "parent", "星海项目", 0)
	bridge := testHit("7", "bridge", "星海项目负责人是张明。", 1)
	child := testHit("7", "child", "张明的履历", 2)
	low := testHit("7", "low", "补充信息", 3)
	lowest := testHit("7", "lowest", "更多信息", 4)
	execution := queryExecution{
		Steps: []queryStepResult{
			{QueryStep: QueryStep{ID: 1}, Status: "retrieved", Hits: []model.SearchResponseDTO{parent, bridge, low, lowest}},
			{QueryStep: QueryStep{ID: 2, DependsOn: []int{1}}, Status: "retrieved", Hits: []model.SearchResponseDTO{child}},
		},
		// Put mandatory sources after optional ones to exercise skipped pins and renumbering.
		Results: []model.SearchResponseDTO{parent, low, bridge, child, lowest},
	}
	execution.protectSources([]model.SearchResponseDTO{bridge})
	execution.rebuildSourceNumbers()
	originalResults := append([]model.SearchResponseDTO(nil), execution.Results...)
	originalSources := append([]int(nil), execution.Steps[0].Sources...)
	var counts []int
	var service chatService
	fitted, err := service.fitAnswerEvidence(execution, func(candidate queryExecution) bool {
		counts = append(counts, len(candidate.Results))
		for _, step := range candidate.Steps {
			for _, index := range step.Sources {
				if index < 1 || index > len(candidate.Results) {
					t.Fatalf("citation points outside filtered result set: %+v", candidate)
				}
				found := false
				for _, hit := range step.Hits {
					found = found || resultKey(hit) == resultKey(candidate.Results[index-1])
				}
				if !found {
					t.Fatalf("citation points at another branch's source: %+v", step)
				}
			}
		}
		return len(candidate.Results) <= 3
	})
	if err != nil || !reflect.DeepEqual(counts, []int{5, 4, 3}) || !reflect.DeepEqual(fitted.Results, []model.SearchResponseDTO{parent, bridge, child}) {
		t.Fatalf("optional tail sources not dropped first: counts=%v results=%+v err=%v", counts, fitted.Results, err)
	}
	if !reflect.DeepEqual(fitted.Steps[0].Sources, []int{1, 2}) || !reflect.DeepEqual(fitted.Steps[1].Sources, []int{3}) {
		t.Fatalf("citation numbers were not rebuilt: %+v", fitted.Steps)
	}
	if fitted.Results[1].TextContent != bridge.TextContent || !reflect.DeepEqual(execution.Results, originalResults) || !reflect.DeepEqual(execution.Steps[0].Sources, originalSources) {
		t.Fatal("packing truncated bridge evidence or modified caller's input")
	}
	contextText := service.buildContextText(fitted.Results)
	if !strings.Contains(contextText, "(来源#2: bridge.txt)") || strings.Contains(contextText, "low.txt") {
		t.Fatalf("rendered references disagree with filtered sources: %s", contextText)
	}
}

func TestFitAnswerEvidenceAlreadyFitsAndCannotFit(t *testing.T) {
	mandatory := testHit("7", "required", "必须保留", 0)
	optional := testHit("8", "required", "同MD5但不同租户的补充来源", 0)
	execution := queryExecution{
		Steps:   []queryStepResult{{QueryStep: QueryStep{ID: 1}, Status: "retrieved", Hits: []model.SearchResponseDTO{mandatory, optional}}},
		Results: []model.SearchResponseDTO{mandatory, optional},
	}
	execution.protectSources(nil)
	execution.rebuildSourceNumbers()
	var service chatService
	calls := 0
	fitted, err := service.fitAnswerEvidence(execution, func(queryExecution) bool { calls++; return true })
	if err != nil || calls != 1 || !reflect.DeepEqual(fitted, execution) {
		t.Fatalf("already-fitting execution should be returned unchanged with one callback: %+v %d %v", fitted, calls, err)
	}
	calls = 0
	fitted, err = service.fitAnswerEvidence(execution, func(queryExecution) bool { calls++; return false })
	if !errors.Is(err, ErrContextBudget) || calls != 2 || len(fitted.Results) != 1 || resultKey(fitted.Results[0]) != resultKey(mandatory) {
		t.Fatalf("mandatory singleton was removed or owner identity ignored: %+v %d %v", fitted, calls, err)
	}
	calls = 0
	_, err = service.fitAnswerEvidence(queryExecution{}, func(queryExecution) bool { calls++; return false })
	if !errors.Is(err, ErrContextBudget) || calls != 1 {
		t.Fatalf("empty evidence must not bypass the total context budget: %d %v", calls, err)
	}
}

func TestQueryExecutionProtectsSingleAndMultipleBranches(t *testing.T) {
	for _, questions := range [][]string{{"A"}, {"A", "B", "C"}} {
		plan := QueryPlan{StandaloneQuestion: "比较"}
		for i, question := range questions {
			plan.Steps = append(plan.Steps, QueryStep{ID: i + 1, Question: question})
		}
		service := testChat(&queryTestRepo{}, func(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
			return []model.SearchResponseDTO{testHit("7", query, "必要来源", 0), testHit("7", query, "可选来源", 1)}, nil
		}, nil)
		execution, err := service.executeQueryPlan(context.Background(), plan, &model.User{ID: 7})
		if err != nil {
			t.Fatal(err)
		}
		if len(execution.mandatorySources) != len(questions) {
			t.Fatalf("expected one protected top source per branch: %+v", execution.mandatorySources)
		}
		fitted, err := service.fitAnswerEvidence(execution, func(candidate queryExecution) bool { return len(candidate.Results) <= len(questions) })
		if err != nil || len(fitted.Results) != len(questions) {
			t.Fatalf("failed to retain exactly mandatory branches: %+v %v", fitted, err)
		}
		for _, step := range fitted.Steps {
			if len(step.Sources) != 1 || fitted.Results[step.Sources[0]-1].ChunkID != 0 {
				t.Fatalf("branch lost its top source: %+v", step)
			}
		}
	}
}

func TestDependencyBindingBudgetCutsChineseSources(t *testing.T) {
	for _, mode := range []string{"retained bridge", "removed source rejected"} {
		t.Run(mode, func(t *testing.T) {
			plan := QueryPlan{StandaloneQuestion: "比较两位项目负责人的履历", Steps: []QueryStep{
				{ID: 1, Question: "项目A负责人"},
				{ID: 2, Question: "项目B负责人"},
				{ID: 3, Question: "{{step:1}}与{{step:2}}的履历", DependsOn: []int{1, 2}},
			}}
			hits := map[string][]model.SearchResponseDTO{}
			sourceByID := map[string]bindingSource{}
			var allSources []bindingSource
			for parent, name := range []string{"张明", "李华"} {
				query := plan.Steps[parent].Question
				prefix := query + "是" + name + "。"
				for rank := 1; rank <= 5; rank++ {
					text := prefix + strings.Repeat("中", 1000-utf8.RuneCountInString(prefix))
					hit := testHit("7", query, text, rank)
					hits[query] = append(hits[query], hit)
					source := bindingSource{ID: fmt.Sprintf("s%d:%d", parent+1, rank), StepID: parent + 1, Text: text, Hit: hit}
					sourceByID[source.ID] = source
					allSources = append(allSources, source)
				}
			}
			var service *chatService
			var modelCalls, childCalls int
			var boundSources []bindingSource
			service = testChat(&queryTestRepo{}, func(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
				if result, ok := hits[query]; ok {
					return result, nil
				}
				childCalls++
				if query != "张明与李华的履历" {
					t.Errorf("wrong bound query after packing: %q", query)
				}
				return []model.SearchResponseDTO{testHit("7", "biographies", "两位负责人的履历资料", 0)}, nil
			}, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
				modelCalls++
				if len(messages) != 2 || messages[0].Content != dependencyBindingPrompt || service.checkContextBudget(messages, 1024) != nil {
					t.Fatal("binding model received an oversized or incorrectly framed request")
				}
				var input struct {
					Question string          `json:"question"`
					Steps    []QueryStep     `json:"steps"`
					Sources  []bindingSource `json:"sources"`
				}
				if err := json.Unmarshal([]byte(messages[1].Content), &input); err != nil {
					t.Fatal(err)
				}
				if len(input.Sources) >= 10 || len(input.Sources) < 2 {
					t.Fatalf("expected a reduced set of the ten 1000-Chinese-character sources, got %d", len(input.Sources))
				}
				originalInput := input
				originalInput.Sources = allSources
				encoded, _ := json.Marshal(originalInput)
				if !errors.Is(service.checkContextBudget([]llm.Message{{Role: "system", Content: dependencyBindingPrompt}, {Role: "user", Content: string(encoded)}}, 1024), ErrContextBudget) {
					t.Fatal("fixture must exceed the default 20000-byte estimate budget before packing")
				}
				lastByParent := map[int]bindingSource{}
				for _, source := range input.Sources {
					original, ok := sourceByID[source.ID]
					if !ok || source.StepID != original.StepID || source.Text != original.Text {
						t.Fatalf("packing changed source ID, parent or quote range: %+v", source)
					}
					lastByParent[source.StepID] = source
				}
				if len(lastByParent) != 2 {
					t.Fatal("packing removed all sources from a required parent")
				}
				var evidence []bindingQuote
				for parent, name := range []string{"张明", "李华"} {
					source := lastByParent[parent+1]
					boundSources = append(boundSources, sourceByID[source.ID])
					evidence = append(evidence, bindingQuote{SourceID: source.ID, Quote: plan.Steps[parent].Question + "是" + name + "。", Value: name})
				}
				if mode == "removed source rejected" {
					evidence[0].SourceID = "s1:5"
					for _, source := range input.Sources {
						if source.ID == evidence[0].SourceID {
							t.Fatal("fixture's forged source must have been removed")
						}
					}
				}
				return emitJSON(emit, struct {
					Steps []queryBinding `json:"steps"`
				}{Steps: []queryBinding{{ID: 3, Evidence: evidence}}})
			})
			execution, err := service.executeQueryPlan(context.Background(), plan, &model.User{ID: 7})
			if err != nil || modelCalls != 1 {
				t.Fatalf("binding call failed before validation: calls=%d err=%v", modelCalls, err)
			}
			if mode == "removed source rejected" {
				if childCalls != 0 || execution.Steps[2].Status != "failed_binding" {
					t.Fatal("binding accepted evidence the model was not shown")
				}
				return
			}
			if childCalls != 1 || execution.Steps[2].Status != "retrieved" {
				t.Fatalf("valid stable source IDs no longer bind: childCalls=%d steps=%+v", childCalls, execution.Steps)
			}
			for _, source := range boundSources {
				if !execution.mandatorySources[resultKey(source.Hit)] {
					t.Fatalf("verified bridge was not protected for final answering: %s", source.ID)
				}
			}
		})
	}
}

func TestDependencyBindingBudgetSkipsUnrelatedRootsAndFailsClosed(t *testing.T) {
	for _, tinyBudget := range []bool{false, true} {
		plan := QueryPlan{StandaloneQuestion: "A负责人履历和B信息", Steps: []QueryStep{
			{ID: 1, Question: "A负责人"}, {ID: 2, Question: "B信息"}, {ID: 3, Question: "{{step:1}}履历", DependsOn: []int{1}},
		}}
		var calls, childCalls int
		service := testChat(&queryTestRepo{}, func(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
			if query == "张明履历" {
				childCalls++
			}
			return []model.SearchResponseDTO{testHit("7", query, "负责人是张明。", 0)}, nil
		}, func(ctx context.Context, messages []llm.Message, params *llm.GenerationParams, emit llm.ChunkHandler) error {
			calls++
			var input struct {
				Sources []bindingSource `json:"sources"`
			}
			if err := json.Unmarshal([]byte(messages[1].Content), &input); err != nil {
				t.Fatal(err)
			}
			if len(input.Sources) != 1 || input.Sources[0].StepID != 1 || input.Sources[0].ID != "s1:1" {
				t.Fatalf("unrelated root leaked into dependency binding: %+v", input.Sources)
			}
			return emitJSON(emit, struct {
				Steps []queryBinding `json:"steps"`
			}{Steps: []queryBinding{{ID: 3, Evidence: []bindingQuote{{SourceID: "s1:1", Quote: "负责人是张明。", Value: "张明"}}}}})
		})
		if tinyBudget {
			service.memoryCfg.MaxInputTokens = 1
		}
		execution, err := service.executeQueryPlan(context.Background(), plan, &model.User{ID: 7})
		if err != nil {
			t.Fatal(err)
		}
		if tinyBudget {
			if calls != 0 || childCalls != 0 || execution.Steps[2].Status != "failed_binding" || len(execution.Results) == 0 {
				t.Fatalf("minimum evidence budget failure must remain a partial binding failure: %+v calls=%d childCalls=%d", execution, calls, childCalls)
			}
		} else if calls != 1 || childCalls != 1 || execution.Steps[2].Status != "retrieved" {
			t.Fatalf("relevant-only binding failed: %+v calls=%d childCalls=%d", execution, calls, childCalls)
		}
	}
}

// Package service 提供了搜索相关的业务逻辑。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/embedding"
	"pai-smart-go/pkg/log"
	"regexp"
	"strconv"
	"strings"
)

// SearchService 接口定义了搜索操作。
type SearchService interface {
	HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error)
}

type SearchIndex interface {
	Search(ctx context.Context, body io.Reader) ([]model.SearchHit, error)
}

type DocumentSearchRepository interface {
	FindStates(ctx context.Context, documentIDs []uint) (map[uint]model.DocumentProcessingState, error)
	FindParents(ctx context.Context, refs []model.ParentRef, userID uint, orgTags []string) ([]model.DocumentChunk, error)
}

type DocumentSearchOptions struct {
	Repository          DocumentSearchRepository
	ModelVersion        string
	ParentContextTokens int
}

type searchService struct {
	embeddingClient embedding.Client
	searchIndex     SearchIndex
	userService     UserService
	uploadRepo      repository.UploadRepository
	documents       DocumentSearchOptions
}

// NewSearchService 创建一个新的 SearchService 实例。
func NewSearchService(embeddingClient embedding.Client, searchIndex SearchIndex, userService UserService, uploadRepo repository.UploadRepository, documents DocumentSearchOptions) (SearchService, error) {
	if documents.Repository == nil || strings.TrimSpace(documents.ModelVersion) == "" {
		return nil, fmt.Errorf("document processing repository and embedding model version are required")
	}
	if documents.ParentContextTokens <= 0 || documents.ParentContextTokens > maxParentContextTokens {
		documents.ParentContextTokens = maxParentContextTokens
	}
	return &searchService{
		embeddingClient: embeddingClient,
		searchIndex:     searchIndex,
		userService:     userService,
		uploadRepo:      uploadRepo,
		documents:       documents,
	}, nil
}

// HybridSearch 执行与 Java 项目逻辑一致的两阶段混合搜索。
func (s *searchService) HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
	if user == nil || user.ID == 0 || topK < 1 || topK > 100 {
		return nil, fmt.Errorf("invalid search user or result limit")
	}
	log.Infof("[SearchService] 开始执行混合搜索, query_bytes=%d topK=%d userID=%d", len(query), topK, user.ID)

	// 1. 获取用户有效的组织标签（包含层级关系）
	log.Info("[SearchService] 步骤1: 获取用户有效组织标签")
	userEffectiveTags, err := s.userService.GetUserEffectiveOrgTags(ctx, user)
	if err != nil {
		log.Errorf("[SearchService] 获取用户有效组织标签失败: %v", err)
		return nil, fmt.Errorf("获取用户访问范围失败: %w", err)
	}
	log.Infof("[SearchService] 获取到 %d 个有效组织标签", len(userEffectiveTags))

	// SQL 是权限规则的唯一来源；ES 只接收允许访问的文件集合。
	accessibleFiles, err := s.uploadRepo.FindAccessibleFiles(ctx, user.ID, userEffectiveTags)
	if err != nil {
		return nil, fmt.Errorf("获取用户可访问文件失败: %w", err)
	}
	if len(accessibleFiles) == 0 {
		return []model.SearchResponseDTO{}, nil
	}
	states, err := s.documents.Repository.FindStates(ctx, searchDocumentIDs(accessibleFiles))
	if err != nil {
		return nil, fmt.Errorf("获取文档活动版本失败: %w", err)
	}
	accessFilter := map[string]interface{}{"bool": map[string]interface{}{"filter": []interface{}{
		buildVersionedSearchAccess(accessibleFiles, states), map[string]interface{}{"term": map[string]interface{}{"model_version": s.documents.ModelVersion}},
	}}}

	// 2. 轻量归一化（去噪）以获取核心短语
	normalized, phrase := normalizeQuery(query)
	if normalized != query {
		log.Info("[SearchService] 已规范化查询")
	}

	// 3. 向量化查询（用原始用户问句，保持语义检索能力）
	log.Info("[SearchService] 步骤2: 开始向量化查询")
	queryVector, err := s.embeddingClient.CreateEmbedding(ctx, query)
	if err != nil {
		log.Errorf("[SearchService] 向量化查询失败: %v", err)
		return nil, fmt.Errorf("failed to create query embedding: %w", err)
	}
	log.Infof("[SearchService] 步骤2: 向量化查询成功, 向量维度: %d", len(queryVector))

	// 4. 构建 Elasticsearch 的复杂混合搜索查询 (与Java对齐)，并加入短语兜底 should
	log.Info("[SearchService] 步骤3: 开始构建 Elasticsearch 两阶段混合搜索查询")
	var buf bytes.Buffer
	esQuery := map[string]interface{}{
		"knn": map[string]interface{}{
			"field":          "vector",
			"query_vector":   queryVector,
			"k":              topK * 30,
			"num_candidates": topK * 30,
			"filter":         accessFilter,
		},
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"must": map[string]interface{}{
					"match": map[string]interface{}{
						"text_content": normalized,
					},
				},
				"filter": accessFilter,
				// 额外的 should：对核心短语做 match_phrase 以兜底召回
				"should": buildPhraseShould(phrase),
			},
		},
		"rescore": map[string]interface{}{
			"window_size": topK * 30, // 与 Java 的 recallK 对齐
			"query": map[string]interface{}{
				"rescore_query": map[string]interface{}{
					"match": map[string]interface{}{
						"text_content": map[string]interface{}{
							"query":    normalized,
							"operator": "and",
						},
					},
				},
				"query_weight":         0.2, // 保留部分 k-NN 分数
				"rescore_query_weight": 1.0, // BM25 分数权重
			},
		},
		"size": topK,
	}

	if err := json.NewEncoder(&buf).Encode(esQuery); err != nil {
		log.Errorf("[SearchService] 序列化 Elasticsearch 查询失败: %v", err)
		return nil, fmt.Errorf("failed to encode es query: %w", err)
	}
	log.Infof("[SearchService] 已构建 Elasticsearch 查询, bytes=%d", buf.Len())

	// 5. 执行搜索
	log.Info("[SearchService] 步骤4: 开始向 Elasticsearch 发送搜索请求")
	// 与 Java 索引名保持一致（假设为 knowledge_base）
	hits, err := s.searchIndex.Search(ctx, &buf)
	if err != nil {
		log.Errorf("[SearchService] 向 Elasticsearch 发送搜索请求失败: %v", err)
		return nil, err
	}
	log.Info("[SearchService] 成功从 Elasticsearch 获取响应")

	if len(hits) == 0 {
		log.Infof("[SearchService] Elasticsearch 返回 0 条命中结果")
		// 兜底：若规范化后核心短语存在且与原问句不同，则用核心短语重试一次（更强关键词信号）
		if phrase != "" && phrase != query {
			log.Info("[SearchService] 使用核心短语重试查询")
			// rebuild with phrase in must+rescore
			var retryBuf bytes.Buffer
			retryQuery := esQuery
			// update must.match.text_content
			((retryQuery["query"].(map[string]interface{}))["bool"].(map[string]interface{}))["must"] = map[string]interface{}{
				"match": map[string]interface{}{
					"text_content": phrase,
				},
			}
			// update rescore query
			((retryQuery["rescore"].(map[string]interface{}))["query"].(map[string]interface{}))["rescore_query"] = map[string]interface{}{
				"match": map[string]interface{}{
					"text_content": map[string]interface{}{
						"query":    phrase,
						"operator": "and",
					},
				},
			}
			if err := json.NewEncoder(&retryBuf).Encode(retryQuery); err != nil {
				return nil, fmt.Errorf("failed to encode retry query: %w", err)
			}
			retryHits, retryErr := s.searchIndex.Search(ctx, &retryBuf)
			if retryErr != nil {
				return nil, fmt.Errorf("retry search failed: %w", retryErr)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			hits = retryHits
			log.Infof("[SearchService] 重试后命中 %d 条", len(hits))
		}
		if len(hits) == 0 {
			return []model.SearchResponseDTO{}, nil
		}
	}

	// Re-read the user after the external search. The user snapshot attached to
	// the request may have lost an organization while ES was running.
	currentUser, err := s.userService.GetProfile(ctx, user.Username)
	if err != nil {
		return nil, fmt.Errorf("复核当前用户失败: %w", err)
	}
	if currentUser == nil || currentUser.ID != user.ID {
		return nil, errors.New("当前用户身份已变更")
	}
	userEffectiveTags, err = s.userService.GetUserEffectiveOrgTags(ctx, currentUser)
	if err != nil {
		return nil, fmt.Errorf("复核用户访问范围失败: %w", err)
	}
	// Recheck publication and permissions after the external search, not only before it.
	accessibleFiles, err = s.uploadRepo.FindAccessibleFiles(ctx, currentUser.ID, userEffectiveTags)
	if err != nil {
		return nil, fmt.Errorf("复核文档访问权限失败: %w", err)
	}
	states, err = s.documents.Repository.FindStates(ctx, searchDocumentIDs(accessibleFiles))
	if err != nil {
		return nil, fmt.Errorf("复核文档活动版本失败: %w", err)
	}
	filesByKey := make(map[string]model.FileUpload, len(accessibleFiles))
	for _, file := range accessibleFiles {
		filesByKey[searchFileKey(file.UserID, file.FileMD5)] = file
	}
	// 8. 组装最终结果
	log.Info("[SearchService] 步骤7: 开始组装最终响应 DTO")
	results := make([]model.SearchResponseDTO, 0, len(hits))
	for _, hit := range hits {
		file, allowed := filesByKey[searchFileKey(hit.Source.UserID, hit.Source.FileMD5)]
		if !activeSearchHit(hit.Source, file, states) {
			allowed = false
		}
		if hit.Source.ModelVersion != s.documents.ModelVersion {
			allowed = false
		}
		if !allowed {
			log.Warnf("[SearchService] 丢弃不在 SQL 授权结果中的 ES 命中, userID=%d, fileMD5=%s", hit.Source.UserID, hit.Source.FileMD5)
			continue
		}
		dto := model.SearchResponseDTO{
			FileMD5:       hit.Source.FileMD5,
			FileName:      file.FileName,
			ChunkID:       hit.Source.ChunkID,
			TextContent:   hit.Source.TextContent,
			ContextPrefix: hit.Source.ContextPrefix,
			Score:         hit.Score,
			UserID:        strconv.FormatUint(uint64(hit.Source.UserID), 10),
			OrgTag:        hit.Source.OrgTag,
			IsPublic:      hit.Source.IsPublic,
			DocumentID:    hit.Source.DocumentID,
			Version:       hit.Source.Version,
			ParentID:      hit.Source.ParentID,
			ChunkKey:      hit.Source.ChunkKey,
			HeadingPath:   hit.Source.HeadingPath,
			SourceSpans:   hit.Source.SourceSpans,
			TokenCount:    hit.Source.TokenCount,
			Kind:          hit.Source.Kind, TableID: hit.Source.TableID, RowRange: hit.Source.RowRange, ColumnPaths: hit.Source.ColumnPaths,
		}
		results = append(results, dto)
	}
	if err := s.attachParentContexts(ctx, results, currentUser, userEffectiveTags); err != nil {
		return nil, err
	}

	log.Infof("[SearchService] 组装最终响应成功, 返回 %d 条结果", len(results))
	log.Info("[SearchService] 混合搜索执行完毕")
	return results, nil
}

func searchDocumentIDs(files []model.FileUpload) []uint {
	ids := make([]uint, 0, len(files))
	for _, file := range files {
		ids = append(ids, file.ID)
	}
	return ids
}

func activeSearchHit(hit model.EsDocument, file model.FileUpload, states map[uint]model.DocumentProcessingState) bool {
	state, ok := states[file.ID]
	return ok && file.ID != 0 && state.ActiveVersion != "" && hit.DocumentID == file.ID && hit.Version == state.ActiveVersion
}

func buildVersionedSearchAccess(files []model.FileUpload, states map[uint]model.DocumentProcessingState) map[string]interface{} {
	var clauses []interface{}
	for _, file := range files {
		state, ok := states[file.ID]
		if !ok || file.ID == 0 || state.ActiveVersion == "" {
			continue
		}
		filters := []interface{}{
			map[string]interface{}{"term": map[string]interface{}{"user_id": file.UserID}},
			map[string]interface{}{"term": map[string]interface{}{"file_md5": file.FileMD5}},
			map[string]interface{}{"term": map[string]interface{}{"document_id": file.ID}},
			map[string]interface{}{"term": map[string]interface{}{"version": state.ActiveVersion}},
		}
		clauses = append(clauses, map[string]interface{}{"bool": map[string]interface{}{"filter": filters}})
	}
	if len(clauses) == 0 {
		return map[string]interface{}{"match_none": map[string]interface{}{}}
	}
	return map[string]interface{}{"bool": map[string]interface{}{"should": clauses, "minimum_should_match": 1}}
}

func (s *searchService) attachParentContexts(ctx context.Context, results []model.SearchResponseDTO, user *model.User, tags []string) error {
	if len(results) == 0 {
		return ctx.Err()
	}
	refs := make([]model.ParentRef, 0, len(results))
	seen := make(map[model.ParentRef]bool)
	for _, result := range results {
		ref := model.ParentRef{DocumentID: result.DocumentID, Version: result.Version, ParentID: result.ParentID}
		if ref.DocumentID > 0 && ref.Version != "" && ref.ParentID != "" && !seen[ref] {
			refs = append(refs, ref)
			seen[ref] = true
		}
	}
	if len(refs) == 0 {
		return ctx.Err()
	}
	parents, err := s.documents.Repository.FindParents(ctx, refs, user.ID, tags)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		log.Warnf("Optional parent context unavailable; retaining retrieved child evidence")
		return nil
	}
	byRef := make(map[model.ParentRef]model.DocumentChunk, len(parents))
	for _, parent := range parents {
		ref := model.ParentRef{DocumentID: parent.DocumentID, Version: parent.Version, ParentID: parent.ChunkID}
		if seen[ref] && parent.IsParent && strings.TrimSpace(parent.Data.BodyText) != "" && len(parent.Data.SourceSpans) > 0 {
			byRef[ref] = parent
		}
	}
	remaining := s.documents.ParentContextTokens
	selected := make(map[model.ParentRef]bool)
	for i := range results {
		hit := &results[i]
		ref := model.ParentRef{DocumentID: hit.DocumentID, Version: hit.Version, ParentID: hit.ParentID}
		parent, ok := byRef[ref]
		if !ok || strconv.FormatUint(uint64(parent.UserID), 10) != hit.UserID {
			continue
		}
		text := documentContextText(parent.Data.ContextPrefix, parent.Data.BodyText)
		cost := estimateTextTokens(text)
		if !selected[ref] {
			if cost > remaining {
				continue
			}
			remaining -= cost
			selected[ref] = true
		}
		// Keep child text and exact source spans unchanged for dependency evidence.
		hit.ParentText, hit.ParentSourceSpans = text, parent.Data.SourceSpans
		hit.ParentRowRange, hit.ParentColumnPaths = parent.Data.RowRange, parent.Data.ColumnPaths
	}
	return nil
}

func searchFileKey(userID uint, fileMD5 string) string {
	return fmt.Sprintf("%d:%s", userID, fileMD5)
}

// normalizeQuery 对用户查询进行轻量去噪与短语提取。
// 返回值：规范化后的查询（用于 BM25/rescore）与核心短语（用于 match_phrase 兜底）。
func normalizeQuery(q string) (string, string) {
	if q == "" {
		return q, ""
	}
	lower := strings.ToLower(q)
	// 去除常见口语/功能词
	stopPhrases := []string{"是谁", "是什么", "是啥", "请问", "怎么", "如何", "告诉我", "严格", "按照", "不要补充", "的区别", "区别", "吗", "呢", "？", "?"}
	for _, sp := range stopPhrases {
		lower = strings.ReplaceAll(lower, sp, " ")
	}
	// 仅保留中文、英文、数字与空白
	reKeep := regexp.MustCompile(`[^\p{Han}a-z0-9\s]+`)
	kept := reKeep.ReplaceAllString(lower, " ")
	// 归一空白
	reSpace := regexp.MustCompile(`\s+`)
	kept = strings.TrimSpace(reSpace.ReplaceAllString(kept, " "))
	if kept == "" {
		return q, ""
	}
	return kept, kept
}

// buildPhraseShould 构建 match_phrase should 子句（带 boost），为空则返回 nil
func buildPhraseShould(phrase string) interface{} {
	if phrase == "" {
		return nil
	}
	return []map[string]interface{}{
		{
			"match_phrase": map[string]interface{}{
				"text_content": map[string]interface{}{
					"query": phrase,
					"boost": 3.0,
				},
			},
		},
	}
}

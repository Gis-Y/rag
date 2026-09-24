package repository

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"pai-smart-go/internal/model"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Opt-in only: apply the conversation-memory and document-sources migrations to
// a dedicated database ending in _test first. Never migrates or flushes databases.
func TestConversationPersistenceIntegration(t *testing.T) {
	dsn, addr := os.Getenv("PAISMART_TEST_MYSQL_DSN"), os.Getenv("PAISMART_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("set PAISMART_TEST_MYSQL_DSN and PAISMART_TEST_REDIS_ADDR for real transaction/migration checks")
	}
	parsed, err := mysqlDriver.ParseDSN(dsn)
	if err != nil || !strings.HasSuffix(parsed.DBName, "_test") {
		t.Fatal("integration test requires a dedicated MySQL database name ending in _test")
	}
	parsed.ParseTime = true
	db, err := gorm.Open(mysql.Open(parsed.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if !db.Migrator().HasTable(&model.ConversationState{}) || !db.Migrator().HasTable(&model.Conversation{}) {
		t.Fatal("apply the conversation-memory migration to the dedicated test database first")
	}
	if !db.Migrator().HasColumn(&model.Conversation{}, "Sources") {
		t.Fatal("apply docs/migrations/20260903_document_sources.sql to the dedicated test database first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	options := &redis.Options{Addr: addr, Password: os.Getenv("PAISMART_TEST_REDIS_PASSWORD")}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	userID := uint(binary.BigEndian.Uint64(random[:])>>1) | 1
	conversationID := fmt.Sprintf("test-%x", random)
	userKey := fmt.Sprintf("user:%d:current_conversation", userID)
	historyKey := "conversation:" + conversationID
	var count int64
	if err := db.WithContext(ctx).Model(&model.ConversationState{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("isolated user collision: %d %v", count, err)
	}
	if count, err := client.Exists(ctx, userKey, historyKey).Result(); err != nil || count != 0 {
		t.Fatalf("isolated Redis collision: %d %v", count, err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := db.WithContext(cleanup).Where("user_id = ? AND conversation_id = ?", userID, conversationID).Delete(&model.Conversation{}).Error; err != nil {
			t.Error(err)
		}
		if err := db.WithContext(cleanup).Where("user_id = ? AND id = ?", userID, conversationID).Delete(&model.ConversationState{}).Error; err != nil {
			t.Error(err)
		}
		cleanRedis := redis.NewClient(options)
		defer cleanRedis.Close()
		if err := cleanRedis.Del(cleanup, userKey, historyKey, historyKey+":version").Err(); err != nil {
			t.Error(err)
		}
	})
	legacy := []model.ChatMessage{{Role: "user", Content: "不使用外部服务"}, {Role: "assistant", Content: "记录为用户约束"}}
	data, _ := json.Marshal(legacy)
	if err := client.Set(ctx, userKey, conversationID, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, historyKey, data, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	repo := NewConversationRepository(client, db)
	var wg sync.WaitGroup
	ids, errs := make([]string, 8), make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) { defer wg.Done(); ids[i], errs[i] = repo.GetOrCreateConversationID(ctx, userID) }(i)
	}
	wg.Wait()
	for i, id := range ids {
		if errs[i] != nil || id != conversationID {
			t.Fatalf("concurrent migration: %v %v", ids, errs)
		}
	}
	state, err := repo.GetConversationState(ctx, userID, conversationID)
	if err != nil || state.LastTurn != 1 || state.Version != 0 {
		t.Fatalf("migrated state: %+v %v", state, err)
	}
	for i := 2; i <= 12; i++ {
		turn := model.Conversation{TurnID: fmt.Sprintf("request-%d", i), Question: fmt.Sprintf("问题%d", i), Answer: fmt.Sprintf("回答%d", i)}
		if err := repo.AppendConversationTurn(ctx, userID, conversationID, state.Version, turn); err != nil {
			t.Fatal(err)
		}
		if err := repo.AppendConversationTurn(ctx, userID, conversationID, state.Version, turn); err != nil {
			t.Fatalf("idempotent retry: %v", err)
		}
		state.Version++
		state.LastTurn++
	}
	if err := repo.UpdateConversationSummary(ctx, userID, conversationID, state.Version, 6, `{"constraint":"不使用外部服务"}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateConversationSummary(ctx, userID, conversationID, state.Version, 7, `{}`); !errors.Is(err, ErrConversationChanged) {
		t.Fatalf("stale summary accepted: %v", err)
	}
	turns, err := repo.GetConversationTurns(ctx, userID, conversationID, 0, 100)
	if err != nil || len(turns) != 12 || turns[0].Question != legacy[0].Content {
		t.Fatalf("raw archive lost: %+v %v", turns, err)
	}
	history, err := repo.GetConversationHistory(ctx, conversationID)
	if err != nil || len(history) != 20 || history[0].Content != "问题3" {
		t.Fatalf("recent window: %+v %v", history, err)
	}
	if foreign, err := repo.GetConversationTurns(ctx, userID+1, conversationID, 0, 100); err != nil || len(foreign) != 0 {
		t.Fatalf("archive owner filter: %+v %v", foreign, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if id, err := repo.GetOrCreateConversationID(ctx, userID); err != nil || id != conversationID {
		t.Fatalf("Redis outage blocked SQL: %q %v", id, err)
	}
	if err := repo.AppendConversationTurn(ctx, userID, conversationID, state.Version+1, model.Conversation{TurnID: "offline", Question: "离线缓存", Answer: "原文照常归档"}); err != nil {
		t.Fatalf("Redis outage blocked append: %v", err)
	}
	t.Run("confirmed memory changes cannot resurrect old context", func(t *testing.T) {
		if !db.Migrator().HasTable(&model.UserMemory{}) {
			t.Skip("apply 20260903_user_memories.sql to test durable user memories")
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := db.WithContext(cleanup).Where("user_id = ?", userID).Delete(&model.UserMemory{}).Error; err != nil {
				t.Error(err)
			}
		})
		memories := NewUserMemoryRepository(db)
		item, err := memories.Create(ctx, userID, model.UserMemory{Scope: "global", Kind: "preference", Key: "language", Content: "中文", Keywords: []string{"语言"}})
		if err != nil {
			t.Fatal(err)
		}
		items, version, err := memories.Snapshot(ctx, userID)
		if err != nil || len(items) != 1 || items[0].ID != item.ID {
			t.Fatalf("snapshot: %+v %v", items, err)
		}
		current, err := repo.GetConversationState(ctx, userID, conversationID)
		if err != nil || current.Version != version || current.ContextAfter != current.LastTurn || current.Summary != "{}" {
			t.Fatalf("context reset: %+v %v", current, err)
		}
		item.Content = "简体中文"
		updated, err := memories.Update(ctx, userID, item.ID, item.Version, item)
		if err != nil || updated.Version != item.Version+1 {
			t.Fatalf("update: %+v %v", updated, err)
		}
		if _, err := memories.Update(ctx, userID, item.ID, item.Version, item); !errors.Is(err, ErrMemoryConflict) {
			t.Fatalf("stale update: %v", err)
		}
		if err := repo.AppendConversationTurn(ctx, userID, conversationID, version, model.Conversation{TurnID: "stale-memory", Question: "旧记忆", Answer: "不得归档"}); !errors.Is(err, ErrConversationChanged) {
			t.Fatalf("stale snapshot saved: %v", err)
		}
		if err := memories.Delete(ctx, userID+1, item.ID, updated.Version); err == nil {
			t.Fatal("foreign user deleted memory")
		}
		if err := db.WithContext(ctx).Model(&model.UserMemory{}).Where("id = ? AND user_id = ?", item.ID, userID).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
		items, afterExpiry, err := memories.Snapshot(ctx, userID)
		if err != nil || len(items) != 0 || afterExpiry <= version {
			t.Fatalf("expiry: %+v %d %v", items, afterExpiry, err)
		}
		turns, err := repo.GetConversationTurns(ctx, userID, conversationID, 0, 100)
		if err != nil || len(turns) != 13 {
			t.Fatalf("memory operations modified archive: %d %v", len(turns), err)
		}
	})
}

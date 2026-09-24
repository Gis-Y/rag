package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"pai-smart-go/internal/model"
	"strings"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func memoryStateSQLStep() conversationSQLStep {
	return conversationSQLStep{contains: []string{"SELECT * FROM `conversation_states`", "user_id = ?", "FOR UPDATE"},
		args: map[int]interface{}{0: int64(7)}, columns: []string{"id", "user_id", "version", "last_turn", "summary_until"},
		rows: [][]driver.Value{{"conv", int64(7), int64(3), int64(10), int64(4)}}}
}

func memoryExpirySQLStep(affected int64) conversationSQLStep {
	return conversationSQLStep{contains: []string{"DELETE FROM `user_memories`", "user_id = ? AND expires_at IS NOT NULL AND expires_at <= ?"},
		args: map[int]interface{}{0: int64(7)}, affected: affected}
}

func memoryResetSQLStep(affected int64) conversationSQLStep {
	return conversationSQLStep{contains: []string{"UPDATE `conversation_states`", "`context_after`=?", "`summary`=?", "`summary_until`=?", "id = ? AND user_id = ? AND version = ?"},
		args: map[int]interface{}{0: int64(10), 1: "{}", 2: int64(10), 3: int64(4), 5: "conv", 6: int64(7), 7: int64(3)}, affected: affected}
}

func memoryFindSQLStep(found bool, version int64) conversationSQLStep {
	step := conversationSQLStep{contains: []string{"SELECT * FROM `user_memories`", "id = ? AND user_id = ?"},
		args: map[int]interface{}{0: int64(2), 1: int64(7)}, columns: []string{"id", "user_id", "version", "scope", "kind", "key", "content", "keywords"}}
	if found {
		step.rows = [][]driver.Value{{int64(2), int64(7), version, "global", "preference", "language", "中文", "[]"}}
	}
	return step
}

func memoryFixture() model.UserMemory {
	return model.UserMemory{Scope: "global", Kind: "preference", Key: "language", Content: "默认中文", Keywords: []string{"语言"}}
}

func TestMemorySnapshotBindsExpiryAndStateVersion(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "expire and reset"}[expired], func(t *testing.T) {
			steps := []conversationSQLStep{sqlOp("BEGIN"), memoryStateSQLStep()}
			version := uint64(3)
			if expired {
				steps = append(steps, memoryExpirySQLStep(1), memoryResetSQLStep(1))
				version++
			} else {
				steps = append(steps, memoryExpirySQLStep(0))
			}
			steps = append(steps, conversationSQLStep{contains: []string{"SELECT * FROM `user_memories`", "user_id = ?", "ORDER BY updated_at DESC, id DESC"}, args: map[int]interface{}{0: int64(7)}, columns: []string{"id"}}, sqlOp("COMMIT"))
			repo := NewUserMemoryRepository(newConversationSQLTest(t, steps...).db)
			items, gotVersion, err := repo.Snapshot(context.Background(), 7)
			if err != nil || gotVersion != version || items == nil || len(items) != 0 {
				t.Fatalf("items=%+v version=%d error=%v", items, gotVersion, err)
			}
		})
	}
}

func TestMemoryCreateLimitUniquenessAndContextReset(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		count                int64
		insertErr, errorWant error
	}{
		{"create", 0, nil, nil}, {"limit", 50, nil, ErrMemoryLimit}, {"duplicate key", 1, &mysqlDriver.MySQLError{Number: 1062}, ErrMemoryConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []conversationSQLStep{sqlOp("BEGIN"), memoryStateSQLStep(), memoryExpirySQLStep(0),
				{contains: []string{"SELECT count(*) FROM `user_memories`", "user_id = ?"}, args: map[int]interface{}{0: int64(7)}, columns: []string{"count(*)"}, rows: [][]driver.Value{{tc.count}}}}
			if tc.count < 50 {
				steps = append(steps, conversationSQLStep{contains: []string{"INSERT INTO `user_memories`"}, args: map[int]interface{}{0: int64(7)}, affected: 1, err: tc.insertErr})
			}
			if tc.errorWant == nil {
				steps = append(steps, memoryResetSQLStep(1), sqlOp("COMMIT"))
			} else {
				steps = append(steps, sqlOp("ROLLBACK"))
			}
			repo := NewUserMemoryRepository(newConversationSQLTest(t, steps...).db)
			item, err := repo.Create(context.Background(), 7, memoryFixture())
			if !errors.Is(err, tc.errorWant) {
				t.Fatalf("got %v, want %v", err, tc.errorWant)
			}
			if err == nil && (item.UserID != 7 || item.Version != 1 || item.ID == 0) {
				t.Fatalf("wrong created identity: %+v", item)
			}
		})
	}
}

func TestMemoryUpdateOwnerVersionAndExpiryClear(t *testing.T) {
	t.Run("owner and version scoped update then context reset", func(t *testing.T) {
		repo := NewUserMemoryRepository(newConversationSQLTest(t, sqlOp("BEGIN"), memoryStateSQLStep(), memoryExpirySQLStep(0), memoryFindSQLStep(true, 1),
			conversationSQLStep{contains: []string{"UPDATE `user_memories`", "`keywords`=?", "`expires_at`=?", "id = ? AND user_id = ? AND version = ?"},
				args: map[int]interface{}{4: `["语言"]`, 5: int64(2), 6: nil}, affected: 1}, memoryResetSQLStep(1), sqlOp("COMMIT")).db)
		item, err := repo.Update(context.Background(), 7, 2, 1, memoryFixture())
		if err != nil || item.ID != 2 || item.UserID != 7 || item.Version != 2 || item.ExpiresAt != nil {
			t.Fatalf("%+v %v", item, err)
		}
	})
	for _, tc := range []struct {
		name    string
		found   bool
		version int64
		want    error
	}{{"foreign owner", false, 0, ErrMemoryNotFound}, {"stale version", true, 2, ErrMemoryConflict}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := NewUserMemoryRepository(newConversationSQLTest(t, sqlOp("BEGIN"), memoryStateSQLStep(), memoryExpirySQLStep(0), memoryFindSQLStep(tc.found, tc.version), sqlOp("ROLLBACK")).db)
			if _, err := repo.Update(context.Background(), 7, 2, 1, memoryFixture()); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestMemoryDeleteRollbackCannotEraseArchive(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "reset failure rolls back delete", true: "delete resets automatic context"}[success], func(t *testing.T) {
			steps := []conversationSQLStep{sqlOp("BEGIN"), memoryStateSQLStep(), memoryExpirySQLStep(0), memoryFindSQLStep(true, 1),
				{contains: []string{"DELETE FROM `user_memories`", "id = ? AND user_id = ? AND version = ?"}, args: map[int]interface{}{0: int64(2), 1: int64(7), 2: int64(1)}, affected: 1}}
			if success {
				steps = append(steps, memoryResetSQLStep(1), sqlOp("COMMIT"))
			} else {
				steps = append(steps, memoryResetSQLStep(0), sqlOp("ROLLBACK"))
			}
			repo := NewUserMemoryRepository(newConversationSQLTest(t, steps...).db)
			err := repo.Delete(context.Background(), 7, 2, 1)
			if success && err != nil {
				t.Fatal(err)
			}
			if !success && !errors.Is(err, ErrConversationChanged) {
				t.Fatalf("CAS failure must roll back memory deletion: %v", err)
			}
		})
	}
}

func TestMemoryRepositoryInputValidation(t *testing.T) {
	base := memoryFixture()
	if err := validateUserMemory(7, base); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	for _, mutate := range []func(*model.UserMemory){
		func(m *model.UserMemory) { m.UserID = 8 }, func(m *model.UserMemory) { m.Scope = "" }, func(m *model.UserMemory) { m.Key = strings.Repeat("字", 65) },
		func(m *model.UserMemory) { m.Kind = "unconfirmed" }, func(m *model.UserMemory) { m.Content = strings.Repeat("字", 501) }, func(m *model.UserMemory) { m.ExpiresAt = &past },
		func(m *model.UserMemory) { m.Keywords = []string{""} },
	} {
		item := base
		mutate(&item)
		if err := validateUserMemory(7, item); err == nil {
			t.Fatalf("invalid memory accepted: %+v", item)
		}
	}
}

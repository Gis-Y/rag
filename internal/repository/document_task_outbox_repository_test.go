package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"pai-smart-go/internal/model"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func mergeRow(status int64, token string, expiry *time.Time) conversationSQLStep {
	var deadline driver.Value
	if expiry != nil {
		deadline = *expiry
	}
	return conversationSQLStep{contains: []string{"SELECT * FROM `file_upload`", "id = ? AND user_id = ?", "FOR UPDATE"},
		columns: []string{"id", "user_id", "status", "merge_token", "merge_expires_at"},
		rows:    [][]driver.Value{{int64(12), int64(7), status, token, deadline}}}
}

func TestMergeCompletionAtomicallyQueuesTaskAndRejectsStaleAttempts(t *testing.T) {
	ctx := context.Background()
	token := strings.Repeat("a", 32)
	later, earlier := time.Now().Add(time.Minute), time.Now().Add(-time.Minute)
	for _, test := range []struct {
		name   string
		status int64
		token  string
		expiry *time.Time
	}{{"deleted", 3, token, &later}, {"other attempt", 4, "newer", &later}, {"expired", 4, token, &earlier}} {
		t.Run(test.name, func(t *testing.T) {
			db := newConversationSQLTest(t, sqlOp("BEGIN"), mergeRow(test.status, test.token, test.expiry), sqlOp("ROLLBACK")).db
			if err := NewUploadRepository(db, nil).CompleteMerge(ctx, 12, 7, token); !errors.Is(err, ErrDocumentChanged) {
				t.Fatalf("stale attempt accepted: %v", err)
			}
		})
	}
	for _, failOutbox := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit upload and outbox", true: "outbox failure rolls back upload"}[failOutbox], func(t *testing.T) {
			insert := conversationSQLStep{contains: []string{"INSERT INTO `document_task_outbox`", "ON DUPLICATE KEY UPDATE"}, affected: 1}
			ending := "COMMIT"
			if failOutbox {
				insert.err, ending = errors.New("outbox unavailable"), "ROLLBACK"
			}
			db := newConversationSQLTest(t, sqlOp("BEGIN"), mergeRow(4, token, &later),
				conversationSQLStep{contains: []string{"UPDATE `file_upload`", "id = ? AND user_id = ? AND status = ? AND merge_token = ?"}, affected: 1},
				insert, sqlOp(ending)).db
			if err := NewUploadRepository(db, nil).CompleteMerge(ctx, 12, 7, token); (err != nil) != failOutbox {
				t.Fatal(err)
			}
		})
	}
	t.Run("uncertain commit can be retried without new task", func(t *testing.T) {
		db := newConversationSQLTest(t, sqlOp("BEGIN"), mergeRow(1, token, nil), sqlOp("COMMIT")).db
		if err := NewUploadRepository(db, nil).CompleteMerge(ctx, 12, 7, token); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMergeLeaseOnlyReclaimsExpiredAttemptsAndReleaseIsCAS(t *testing.T) {
	ctx, token := context.Background(), strings.Repeat("b", 32)
	later, earlier := time.Now().Add(time.Minute), time.Now().Add(-time.Minute)
	for _, active := range []bool{false, true} {
		expiry := &earlier
		if active {
			expiry = &later
		}
		steps := []conversationSQLStep{sqlOp("BEGIN"), mergeRow(4, "old", expiry)}
		if active {
			steps = append(steps, sqlOp("ROLLBACK"))
		} else {
			steps = append(steps, conversationSQLStep{contains: []string{"UPDATE `file_upload`", "`merge_token`=?", "`status`=?"}, affected: 1}, sqlOp("COMMIT"))
		}
		repo := NewUploadRepository(newConversationSQLTest(t, steps...).db, nil)
		file, err := repo.BeginMerge(ctx, 12, 7, token, later)
		if active && !errors.Is(err, ErrMergeInProgress) {
			t.Fatalf("active merge replaced: %v", err)
		}
		if !active && (err != nil || file.MergeToken != token || file.Status != 4) {
			t.Fatalf("expired merge not reclaimed: %+v %v", file, err)
		}
	}
	for _, affected := range []int64{0, 1} {
		repo := NewUploadRepository(newConversationSQLTest(t, conversationSQLStep{
			contains: []string{"UPDATE `file_upload`", "id = ? AND user_id = ? AND status = ? AND merge_token = ?"}, affected: affected,
		}).db, nil)
		released, err := repo.ReleaseMerge(ctx, 12, 7, token)
		if err != nil || released != (affected == 1) {
			t.Fatalf("release CAS: %t %v", released, err)
		}
	}
}

func TestOutboxUsesCompletedUploadAndLeaseScopedAcknowledgement(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	token := strings.Repeat("c", 32)
	t.Run("explicit reprocess durable enqueue", func(t *testing.T) {
		db := newConversationSQLTest(t, sqlOp("BEGIN"), documentUploadStep(true),
			conversationSQLStep{contains: []string{"INSERT INTO `document_task_outbox`", "ON DUPLICATE KEY UPDATE"}, affected: 1}, sqlOp("COMMIT")).db
		if err := NewDocumentTaskOutboxRepository(db).Enqueue(ctx, 12, 7); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("claim with upload lock and expired lease CAS", func(t *testing.T) {
		db := newConversationSQLTest(t,
			conversationSQLStep{contains: []string{"SELECT * FROM `document_task_outbox`", "available_at <= ? AND (lease_until IS NULL OR lease_until <= ?)"},
				columns: []string{"document_id", "user_id", "attempts"}, rows: [][]driver.Value{{int64(12), int64(7), int64(2)}}},
			sqlOp("BEGIN"), documentUploadStep(true),
			conversationSQLStep{contains: []string{"UPDATE `document_task_outbox`", "attempts + 1", "lease_until <= ?", "document_id = ? AND user_id = ?"}, affected: 1}, sqlOp("COMMIT")).db
		entry, file, err := NewDocumentTaskOutboxRepository(db).Claim(ctx, token, now)
		if err != nil || entry.DocumentID != 12 || entry.Attempts != 3 || entry.LeaseToken != token || file.UserID != 7 {
			t.Fatalf("claim: %+v %+v %v", entry, file, err)
		}
	})
	t.Run("acknowledge and retry cannot mutate newer lease", func(t *testing.T) {
		db := newConversationSQLTest(t,
			conversationSQLStep{contains: []string{"DELETE FROM `document_task_outbox` WHERE document_id = ? AND user_id = ? AND lease_token = ?"}, args: map[int]interface{}{0: int64(12), 1: int64(7), 2: token}},
			conversationSQLStep{contains: []string{"UPDATE `document_task_outbox`", "document_id = ? AND user_id = ? AND lease_token = ?"}},
		).db
		repo := NewDocumentTaskOutboxRepository(db)
		if err := repo.Acknowledge(ctx, 12, 7, token); err != nil {
			t.Fatal(err)
		}
		if err := repo.Retry(ctx, 12, 7, token, "broker unavailable", now); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUploadKeysAndFinalDeletionAreDocumentScoped(t *testing.T) {
	r := &uploadRepository{}
	if r.getRedisUploadKey(12, 7) == r.getRedisUploadKey(13, 7) {
		t.Fatal("new upload reused old progress key")
	}
	db := newConversationSQLTest(t, sqlOp("BEGIN"),
		conversationSQLStep{contains: []string{"SELECT * FROM `file_upload`", "id = ? AND user_id = ? AND status = ?", "FOR UPDATE"}, columns: []string{"id"}, rows: [][]driver.Value{{int64(12)}}},
		conversationSQLStep{contains: []string{"DELETE FROM `chunk_info` WHERE document_id = ? AND user_id = ?"}},
		conversationSQLStep{contains: []string{"DELETE FROM `document_task_outbox` WHERE document_id = ? AND user_id = ?"}},
		conversationSQLStep{contains: []string{"DELETE FROM `file_upload` WHERE id = ? AND user_id = ? AND status = ?"}}, sqlOp("COMMIT")).db
	if err := NewUploadRepository(db, nil).DeleteFileUploadByID(context.Background(), 12, 7); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeChunkLocksUploadingDocumentAroundPublication(t *testing.T) {
	chunk := &model.ChunkInfo{DocumentID: 12, UserID: 7, FileMD5: strings.Repeat("a", 32), ChunkIndex: 2, ChunkMD5: strings.Repeat("b", 32), StoragePath: "chunks/7/12/2"}
	uploading := conversationSQLStep{
		contains: []string{"SELECT * FROM `file_upload`", "id = ? AND user_id = ? AND status = ?", "FOR UPDATE"},
		args:     map[int]interface{}{0: int64(12), 1: int64(7), 2: int64(0)},
		columns:  []string{"id", "user_id", "file_md5", "status"},
		rows:     [][]driver.Value{{int64(12), int64(7), strings.Repeat("a", 32), int64(0)}},
	}
	persisted := conversationSQLStep{
		contains: []string{"SELECT * FROM `chunk_info`", "document_id = ? AND user_id = ? AND chunk_index = ? AND file_md5 = ? AND chunk_md5 = ? AND storage_path = ?", "FOR UPDATE"},
		columns:  []string{"id", "document_id", "user_id", "file_md5", "chunk_index", "chunk_md5", "storage_path"},
		rows:     [][]driver.Value{{int64(1), int64(12), int64(7), strings.Repeat("a", 32), int64(2), strings.Repeat("b", 32), "chunks/7/12/2"}},
	}
	newProgress := func(t *testing.T, events *[]string) *redis.Client {
		t.Helper()
		client := redis.NewClient(&redis.Options{Addr: "unused:0"})
		t.Cleanup(func() { _ = client.Close() })
		client.AddHook(conversationCommandHook{process: func(cmd redis.Cmder) {
			args := cmd.Args()
			if len(args) != 4 || fmt.Sprint(args[0]) != "setbit" {
				t.Fatalf("unexpected progress command: %v", args)
			}
			*events = append(*events, "bit="+fmt.Sprint(args[3]))
			cmd.(*redis.IntCmd).SetVal(0)
		}})
		return client
	}
	t.Run("publish and completed bit follow SQL commit", func(t *testing.T) {
		db := newConversationSQLTest(t, sqlOp("BEGIN"), uploading,
			conversationSQLStep{contains: []string{"DELETE FROM `chunk_info`", "document_id = ? AND user_id = ? AND chunk_index = ?"}, affected: 1},
			conversationSQLStep{contains: []string{"INSERT INTO `chunk_info`"}, affected: 1}, sqlOp("COMMIT"),
			sqlOp("BEGIN"), uploading, persisted, sqlOp("COMMIT")).db
		events := make([]string, 0, 3)
		repo := &uploadRepository{db: db, redisClient: newProgress(t, &events)}
		err := repo.FinalizeChunk(context.Background(), chunk, func() error {
			events = append(events, "publish")
			return nil
		})
		if err != nil || !reflect.DeepEqual(events, []string{"bit=0", "publish", "bit=1"}) {
			t.Fatalf("unsafe publication order: events=%v err=%v", events, err)
		}
	})
	for _, failure := range []string{"insert", "commit"} {
		t.Run(failure+" failure leaves progress incomplete", func(t *testing.T) {
			failed := errors.New("SQL publication failed")
			insert := conversationSQLStep{contains: []string{"INSERT INTO `chunk_info`"}, affected: 1}
			commit := sqlOp("COMMIT")
			ending := "COMMIT"
			if failure == "insert" {
				insert.err, ending = failed, "ROLLBACK"
			} else {
				commit.err = failed
			}
			db := newConversationSQLTest(t, sqlOp("BEGIN"), uploading,
				conversationSQLStep{contains: []string{"DELETE FROM `chunk_info`"}, affected: 1}, insert,
				map[string]conversationSQLStep{"COMMIT": commit, "ROLLBACK": sqlOp("ROLLBACK")}[ending]).db
			events := make([]string, 0, 1)
			repo := &uploadRepository{db: db, redisClient: newProgress(t, &events)}
			published := false
			err := repo.FinalizeChunk(context.Background(), chunk, func() error { published = true; return nil })
			if !errors.Is(err, failed) || published || !reflect.DeepEqual(events, []string{"bit=0"}) {
				t.Fatalf("failed SQL exposed chunk: published=%t events=%v err=%v", published, events, err)
			}
		})
	}
	t.Run("tombstone rejects before object publication", func(t *testing.T) {
		missing := uploading
		missing.rows = nil
		db := newConversationSQLTest(t, sqlOp("BEGIN"), missing, sqlOp("ROLLBACK")).db
		events := make([]string, 0)
		published := false
		repo := &uploadRepository{db: db, redisClient: newProgress(t, &events)}
		err := repo.FinalizeChunk(context.Background(), chunk, func() error {
			published = true
			return nil
		})
		if !errors.Is(err, ErrDocumentChanged) || published || len(events) != 0 {
			t.Fatalf("stale chunk publication accepted: published=%t events=%v err=%v", published, events, err)
		}
	})
}

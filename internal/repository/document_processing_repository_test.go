package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"pai-smart-go/internal/model"
	"testing"
)

func newDocumentSQLTest(t *testing.T, steps ...conversationSQLStep) *DocumentProcessingRepository {
	return NewDocumentProcessingRepository(newConversationSQLTest(t, steps...).db)
}

func documentUploadStep(exists bool) conversationSQLStep {
	step := conversationSQLStep{contains: []string{"SELECT * FROM `file_upload`", "id = ? AND user_id = ? AND status = ?", "FOR UPDATE"}, args: map[int]interface{}{0: int64(12), 1: int64(7), 2: int64(1)}, columns: []string{"id", "user_id", "file_md5", "file_name", "status"}}
	if exists {
		step.rows = [][]driver.Value{{int64(12), int64(7), "md5", "file.md", int64(1)}}
	}
	return step
}

func documentPendingStep(exists bool) conversationSQLStep {
	step := conversationSQLStep{contains: []string{"SELECT * FROM `document_processing_states`", "document_id = ? AND user_id = ? AND pending_version = ?"}, args: map[int]interface{}{0: int64(12), 1: int64(7), 2: "new"}, columns: []string{"document_id", "user_id", "active_version", "pending_version", "status"}}
	if exists {
		step.rows = [][]driver.Value{{int64(12), int64(7), "old", "new", "embedding"}}
	}
	return step
}

func TestDocumentProcessingStagingAndPublication(t *testing.T) {
	ctx := context.Background()
	t.Run("begin preserves active version", func(t *testing.T) {
		r := newDocumentSQLTest(t, sqlOp("BEGIN"), documentUploadStep(true), conversationSQLStep{contains: []string{"INSERT INTO `document_processing_states`", "ON DUPLICATE KEY UPDATE", "`pending_version`=VALUES(`pending_version`)"}, affected: 1}, sqlOp("COMMIT"))
		file, err := r.Begin(ctx, 12, 7, "md5", "new")
		if err != nil || file.ID != 12 {
			t.Fatalf("%+v %v", file, err)
		}
	})
	t.Run("publish only after staged pending version and existing upload", func(t *testing.T) {
		r := newDocumentSQLTest(t, sqlOp("BEGIN"), documentUploadStep(true), documentPendingStep(true), conversationSQLStep{contains: []string{"UPDATE `document_processing_states`", "`active_version`=?", "pending_version = ? AND status = ?"}, affected: 1}, sqlOp("COMMIT"))
		if err := r.Publish(ctx, 12, 7, "new"); err != nil {
			t.Fatal(err)
		}
	})
	for _, test := range []struct {
		name            string
		upload, pending bool
	}{{"removed upload", false, false}, {"superseded task", true, false}} {
		t.Run(test.name, func(t *testing.T) {
			steps := []conversationSQLStep{sqlOp("BEGIN"), documentUploadStep(test.upload)}
			if test.upload {
				steps = append(steps, documentPendingStep(test.pending))
			}
			steps = append(steps, sqlOp("ROLLBACK"))
			r := newDocumentSQLTest(t, steps...)
			if err := r.Publish(ctx, 12, 7, "new"); !errors.Is(err, ErrDocumentChanged) {
				t.Fatal(err)
			}
		})
	}
	t.Run("CAS miss rolls back", func(t *testing.T) {
		r := newDocumentSQLTest(t, sqlOp("BEGIN"), documentUploadStep(true), documentPendingStep(true), sqlOp("UPDATE `document_processing_states`"), sqlOp("ROLLBACK"))
		if err := r.Publish(ctx, 12, 7, "new"); !errors.Is(err, ErrDocumentChanged) {
			t.Fatal(err)
		}
	})
	t.Run("invalid chunk owner is rejected before SQL", func(t *testing.T) {
		r := newDocumentSQLTest(t)
		if err := r.Stage(ctx, 12, 7, "new", []model.DocumentChunk{{DocumentID: 12, UserID: 8, Version: "new"}}); err == nil {
			t.Fatal("cross-owner chunk accepted")
		}
	})
}

func TestDocumentParentLookupRequiresActiveVersionAndPermission(t *testing.T) {
	r := newDocumentSQLTest(t, conversationSQLStep{contains: []string{"SELECT c.*", "JOIN file_upload AS f", "f.status = 1", "s.active_version = c.version", "c.is_parent = ?", "f.user_id = ? OR f.is_public = ?", "f.org_tag IN", "c.document_id = ? AND c.version = ? AND c.chunk_id = ?"}, columns: []string{"document_id"}})
	if _, err := r.FindParents(context.Background(), []model.ParentRef{{DocumentID: 12, Version: "old", ParentID: "parent"}}, 7, []string{"TEAM"}); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentFailureAndDeletionStayVersionAndOwnerScoped(t *testing.T) {
	t.Run("failure never replaces active version", func(t *testing.T) {
		r := newDocumentSQLTest(t, conversationSQLStep{contains: []string{"UPDATE `document_processing_states` SET `error`=?,`status`=?,`updated_at`=? WHERE document_id = ? AND user_id = ? AND pending_version = ?"}, args: map[int]interface{}{0: "parser failed", 1: "failed", 3: int64(12), 4: int64(7), 5: "new"}, affected: 1})
		if err := r.Fail(context.Background(), 12, 7, "new", "parser failed"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing pending generation is not reported as persisted", func(t *testing.T) {
		r := newDocumentSQLTest(t, conversationSQLStep{contains: []string{"UPDATE `document_processing_states`", "pending_version = ?"}, affected: 0})
		if err := r.Fail(context.Background(), 12, 7, "new", "parser failed"); !errors.Is(err, ErrDocumentChanged) {
			t.Fatalf("missing failure CAS reported success: %v", err)
		}
	})
	t.Run("discard skips an already published version", func(t *testing.T) {
		state := conversationSQLStep{contains: []string{"SELECT * FROM `document_processing_states`", "FOR UPDATE"}, columns: []string{"document_id", "user_id", "active_version"}, rows: [][]driver.Value{{int64(12), int64(7), "new"}}}
		r := newDocumentSQLTest(t, sqlOp("BEGIN"), state, sqlOp("COMMIT"))
		called := false
		discarded, err := r.DiscardVersion(context.Background(), 12, 7, "new", func() error { called = true; return nil })
		if err != nil || discarded || called {
			t.Fatalf("published version was discarded: discarded=%t called=%t err=%v", discarded, called, err)
		}
	})
	t.Run("discard cleans external data before staged SQL", func(t *testing.T) {
		state := conversationSQLStep{contains: []string{"SELECT * FROM `document_processing_states`", "FOR UPDATE"}, columns: []string{"document_id", "user_id", "active_version"}, rows: [][]driver.Value{{int64(12), int64(7), "old"}}}
		r := newDocumentSQLTest(t, sqlOp("BEGIN"), state,
			conversationSQLStep{contains: []string{"DELETE FROM `document_chunks`", "document_id = ? AND user_id = ? AND version = ?"}, affected: 3}, sqlOp("COMMIT"))
		called := false
		discarded, err := r.DiscardVersion(context.Background(), 12, 7, "new", func() error { called = true; return nil })
		if err != nil || !discarded || !called {
			t.Fatalf("failed version was not discarded: discarded=%t called=%t err=%v", discarded, called, err)
		}
	})
	t.Run("cleanup is authorized only after upload tombstone", func(t *testing.T) {
		r := newDocumentSQLTest(t, sqlOp("BEGIN"), conversationSQLStep{contains: []string{"SELECT * FROM `file_upload`", "FOR UPDATE"}, args: map[int]interface{}{0: int64(12), 1: int64(7), 2: int64(3)}, columns: []string{"id"}, rows: [][]driver.Value{{int64(12)}}},
			conversationSQLStep{contains: []string{"DELETE FROM `document_chunks` WHERE document_id = ? AND user_id = ?"}, args: map[int]interface{}{0: int64(12), 1: int64(7)}, affected: 3},
			conversationSQLStep{contains: []string{"DELETE FROM `document_processing_states` WHERE document_id = ? AND user_id = ?"}, args: map[int]interface{}{0: int64(12), 1: int64(7)}, affected: 1}, sqlOp("COMMIT"))
		if err := r.DeleteArtifacts(context.Background(), 12, 7); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("upload completion cannot reverse deletion", func(t *testing.T) {
		db := newConversationSQLTest(t, conversationSQLStep{contains: []string{"UPDATE `file_upload` SET `status`=? WHERE id = ? AND status <> ?"}, args: map[int]interface{}{0: int64(1), 1: int64(12), 2: int64(3)}}, conversationSQLStep{contains: []string{"SELECT count(*) FROM `file_upload` WHERE id = ? AND status = ?"}, columns: []string{"count"}, rows: [][]driver.Value{{int64(0)}}}).db
		if err := NewUploadRepository(db, nil).UpdateFileUploadStatus(12, 1); !errors.Is(err, ErrDocumentChanged) {
			t.Fatalf("tombstone restored: %v", err)
		}
	})
}

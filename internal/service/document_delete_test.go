package service

import (
	"context"
	"errors"
	"testing"

	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
)

type deleteIdentityRepo struct {
	repository.UploadRepository
	files             map[uint]*model.FileUpload
	readID, deletedID uint
	stop              error
}

func (r *deleteIdentityRepo) GetFileUploadByID(ctx context.Context, id uint) (*model.FileUpload, error) {
	r.readID = id
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.files[id], nil
}

func (r *deleteIdentityRepo) UpdateFileUploadStatus(id uint, status int) error {
	if status != 3 {
		return errors.New("expected tombstone before cleanup")
	}
	r.deletedID = id
	return r.stop // Stop before real storage; this check verifies the first mutation's identity.
}

func TestDeleteDocumentUsesIDAndOwnerBeforeAnyMutation(t *testing.T) {
	stop := errors.New("authorized tombstone reached")
	for _, tc := range []struct {
		name, role     string
		id, wantDelete uint
	}{
		{"own document", "USER", 12, 12},
		{"other owner same MD5", "USER", 37, 0},
		{"admin selected other owner", "ADMIN", 37, 37},
		{"missing document", "ADMIN", 99, 0},
		{"zero ID", "USER", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &deleteIdentityRepo{files: map[uint]*model.FileUpload{
				12: {ID: 12, UserID: 7, FileMD5: "same", FileName: "same.pdf"},
				37: {ID: 37, UserID: 8, FileMD5: "same", FileName: "same.pdf", IsPublic: true},
			}, stop: stop}
			s := &documentService{uploadRepo: repo}
			err := s.DeleteDocument(context.Background(), tc.id, &model.User{ID: 7, Role: tc.role})
			if err == nil || repo.deletedID != tc.wantDelete || (tc.wantDelete != 0 && !errors.Is(err, stop)) {
				t.Fatalf("selected=%d mutated=%d err=%v", tc.id, repo.deletedID, err)
			}
			if tc.id != 0 && repo.readID != tc.id {
				t.Fatal("lookup did not use selected document ID")
			}
		})
	}
	if err := (&documentService{}).DeleteDocument(context.Background(), 12, nil); err == nil {
		t.Fatal("missing authenticated user accepted")
	}
}

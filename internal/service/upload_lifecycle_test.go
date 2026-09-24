package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/storage"
)

type lifecycleUploadRepo struct {
	repository.UploadRepository
	mu                            sync.Mutex
	file                          model.FileUpload
	marks                         map[uint][]int
	queued                        int
	completeErr                   error
	commitDespiteError            bool
	finalizeErr                   error
	cleanupStart, cleanupContinue chan struct{}
}

func (r *lifecycleUploadRepo) GetFileUploadRecord(string, uint) (*model.FileUpload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file := r.file
	return &file, nil
}
func (r *lifecycleUploadRepo) BeginMerge(_ context.Context, id, owner uint, token string, expiry time.Time) (*model.FileUpload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file.Status == 4 {
		return nil, repository.ErrMergeInProgress
	}
	if r.file.Status == 3 || r.file.ID != id || r.file.UserID != owner {
		return nil, repository.ErrDocumentChanged
	}
	if r.file.Status != 1 {
		r.file.Status, r.file.MergeToken, r.file.MergeExpiresAt = 4, token, &expiry
	}
	file := r.file
	return &file, nil
}
func (r *lifecycleUploadRepo) CompleteMerge(_ context.Context, id, owner uint, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file.ID != id || r.file.UserID != owner || r.file.MergeToken != token || r.file.Status != 4 {
		return repository.ErrDocumentChanged
	}
	if r.completeErr == nil || r.commitDespiteError {
		r.file.Status = 1
		r.queued++
	}
	return r.completeErr
}
func (r *lifecycleUploadRepo) ReleaseMerge(_ context.Context, id, owner uint, token string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file.ID != id || r.file.UserID != owner || r.file.MergeToken != token || r.file.Status != 4 {
		return false, nil
	}
	r.file.Status = 0
	return true, nil
}
func (r *lifecycleUploadRepo) GetUploadedChunksFromRedis(_ context.Context, id, owner uint, total int) ([]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.marks[id]...), nil
}
func (r *lifecycleUploadRepo) FinalizeChunk(_ context.Context, chunk *model.ChunkInfo, publish func() error) error {
	r.mu.Lock()
	valid := r.file.ID == chunk.DocumentID && r.file.UserID == chunk.UserID && r.file.Status == 0 && r.file.FileMD5 == chunk.FileMD5
	if valid {
		remaining := make([]int, 0, len(r.marks[chunk.DocumentID]))
		for _, existing := range r.marks[chunk.DocumentID] {
			if existing != chunk.ChunkIndex {
				remaining = append(remaining, existing)
			}
		}
		r.marks[chunk.DocumentID] = remaining
	}
	finalizeErr := r.finalizeErr
	r.mu.Unlock()
	if !valid {
		return repository.ErrDocumentChanged
	}
	if finalizeErr != nil {
		return finalizeErr
	}
	if err := publish(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file.Status != 0 {
		return repository.ErrDocumentChanged
	}
	for _, existing := range r.marks[chunk.DocumentID] {
		if existing == chunk.ChunkIndex {
			return nil
		}
	}
	r.marks[chunk.DocumentID] = append(r.marks[chunk.DocumentID], chunk.ChunkIndex)
	return nil
}
func (r *lifecycleUploadRepo) DeleteUploadMark(_ context.Context, id, owner uint) error {
	if r.cleanupStart != nil {
		close(r.cleanupStart)
		<-r.cleanupContinue
	}
	r.mu.Lock()
	delete(r.marks, id)
	r.mu.Unlock()
	return nil
}

type lifecycleStore struct {
	mu                      sync.Mutex
	objects                 map[string]string
	writes, removed         []string
	verifyErr               error
	copyStart, copyContinue chan struct{}
	cleanupDone             chan struct{}
}

func (s *lifecycleStore) Put(_ context.Context, key string, body io.Reader, size int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = string(data)
	s.writes = append(s.writes, key)
	return nil
}
func (s *lifecycleStore) Copy(ctx context.Context, dest, source string) error {
	if s.copyStart != nil {
		close(s.copyStart)
		<-s.copyContinue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[dest] = s.objects[source]
	s.writes = append(s.writes, dest)
	return ctx.Err()
}
func (s *lifecycleStore) Compose(ctx context.Context, dest string, sources []string) error {
	return s.Copy(ctx, dest, sources[0])
}
func (s *lifecycleStore) VerifyMD5(ctx context.Context, key, digest string) error {
	if s.verifyErr != nil {
		return s.verifyErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	got := md5.Sum([]byte(s.objects[key]))
	if hex.EncodeToString(got[:]) != digest {
		return storage.ErrMD5Mismatch
	}
	return nil
}
func (s *lifecycleStore) RemoveDocumentChunks(ctx context.Context, owner, document uint) error {
	prefix := "chunks/" + strconv.FormatUint(uint64(owner), 10) + "/" + strconv.FormatUint(uint64(document), 10) + "/"
	s.mu.Lock()
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			delete(s.objects, key)
			s.removed = append(s.removed, key)
		}
	}
	s.mu.Unlock()
	if s.cleanupDone != nil {
		select {
		case <-s.cleanupDone:
		default:
			close(s.cleanupDone)
		}
	}
	return ctx.Err()
}
func (s *lifecycleStore) Remove(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	s.removed = append(s.removed, key)
	return nil
}
func (s *lifecycleStore) RemoveMany(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if err := s.Remove(ctx, key); err != nil {
			return err
		}
	}
	if s.cleanupDone != nil {
		close(s.cleanupDone)
	}
	return nil
}
func (s *lifecycleStore) PresignedGetURL(ctx context.Context, key string, _ time.Duration) (string, error) {
	return key, ctx.Err()
}

func newLifecycleUpload(t *testing.T) (*lifecycleUploadRepo, *lifecycleStore, UploadService) {
	t.Helper()
	log.Init("error", "json", "")
	digest := md5.Sum([]byte("data"))
	repo := &lifecycleUploadRepo{file: model.FileUpload{ID: 12, UserID: 7, FileMD5: hex.EncodeToString(digest[:]), FileName: "doc.txt", TotalSize: 4}, marks: map[uint][]int{12: {0}}}
	store := &lifecycleStore{objects: map[string]string{storage.ChunkObjectKey(7, 12, 0): "data"}, cleanupDone: make(chan struct{})}
	return repo, store, NewUploadService(repo, nil, store, 32<<20)
}

func waitLifecycle(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lifecycle operation did not finish")
	}
}

func TestCompletedUploadAndMergeNeverRewriteOrRemoveRaw(t *testing.T) {
	repo, store, service := newLifecycleUpload(t)
	repo.file.Status, repo.file.MergeToken = 1, strings.Repeat("a", 32)
	raw := storage.DocumentObjectKey(7, 12, repo.file.MergeToken)
	store.objects[raw] = "data"
	repo.marks = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	indexes, total, err := service.UploadChunk(ctx, repo.file.FileMD5, "doc.txt", 4, 0, strings.NewReader("bad!"), 7, "", false)
	if err != nil || total != 1 || !reflect.DeepEqual(indexes, []int{0}) {
		t.Fatalf("completed chunk was not idempotent: %v %v", indexes, err)
	}
	if _, err := service.MergeChunks(ctx, repo.file.FileMD5, "doc.txt", 7); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(store.writes) != 0 || store.objects[raw] != "data" || repo.queued != 0 {
		t.Fatal("completed upload was mutated")
	}
	for _, removed := range store.removed {
		if removed == raw {
			t.Fatal("completed raw object was removed")
		}
	}
}

func TestMergeFailuresOnlyCleanAnUnpublishedOwnedAttempt(t *testing.T) {
	for _, mode := range []string{"verification", "transaction rollback", "lost commit response"} {
		t.Run(mode, func(t *testing.T) {
			repo, store, service := newLifecycleUpload(t)
			if mode == "verification" {
				store.verifyErr = errors.New("corrupt or cancelled read")
			} else {
				repo.completeErr = errors.New("transaction response lost")
				repo.commitDespiteError = mode == "lost commit response"
			}
			old := storage.DocumentObjectKey(7, 12, "older-attempt")
			store.objects[old] = "untouched"
			if _, err := service.MergeChunks(context.Background(), repo.file.FileMD5, "doc.txt", 7); err == nil {
				t.Fatal("failure was hidden")
			}
			if len(store.writes) != 1 || store.writes[0] == old || store.objects[old] != "untouched" {
				t.Fatal("merge reused another raw key")
			}
			_, exists := store.objects[store.writes[0]]
			if exists != (mode == "lost commit response") {
				t.Fatalf("unsafe uncertain-commit cleanup: exists=%t mode=%s", exists, mode)
			}
			if mode == "lost commit response" && (repo.file.Status != 1 || repo.queued != 1 || len(store.removed) != 0) {
				t.Fatal("committed raw/outbox was changed after lost response")
			}
		})
	}
}

func TestConcurrentMergeIsRejectedBeforeSecondObjectWrite(t *testing.T) {
	repo, store, service := newLifecycleUpload(t)
	store.copyStart, store.copyContinue = make(chan struct{}), make(chan struct{})
	digest := repo.file.FileMD5
	finished := make(chan error, 1)
	go func() { _, err := service.MergeChunks(context.Background(), digest, "doc.txt", 7); finished <- err }()
	waitLifecycle(t, store.copyStart)
	if _, err := service.MergeChunks(context.Background(), digest, "doc.txt", 7); !errors.Is(err, repository.ErrMergeInProgress) {
		t.Fatalf("parallel merge accepted: %v", err)
	}
	close(store.copyContinue)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, store.cleanupDone)
	if len(store.writes) != 1 || repo.queued != 1 || repo.file.Status != 1 {
		t.Fatal("merge was published more than once")
	}
}

func TestOldMergeCleanupCannotDeleteReuploadedDocumentChunks(t *testing.T) {
	repo, store, service := newLifecycleUpload(t)
	repo.cleanupStart, repo.cleanupContinue = make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := service.MergeChunks(context.Background(), repo.file.FileMD5, "doc.txt", 7)
		finished <- err
	}()
	waitLifecycle(t, repo.cleanupStart)
	// Deletion permits a new SQL upload ID for the same owner and MD5.
	repo.mu.Lock()
	repo.file.ID, repo.file.Status = 13, 0
	repo.marks[13] = []int{0}
	repo.mu.Unlock()
	newKey := storage.ChunkObjectKey(7, 13, 0)
	store.mu.Lock()
	store.objects[newKey] = "new generation"
	store.mu.Unlock()
	close(repo.cleanupContinue)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, store.cleanupDone)
	if !reflect.DeepEqual(repo.marks[13], []int{0}) || store.objects[newKey] != "new generation" {
		t.Fatal("old cleanup deleted new upload generation")
	}
	if _, exists := store.objects[storage.ChunkObjectKey(7, 12, 0)]; exists {
		t.Fatal("old chunk cleanup did not run")
	}
}

func TestCorruptChunkProgressIsResetAndExplicitRetransmitReplacesBytes(t *testing.T) {
	repo, store, service := newLifecycleUpload(t)
	canonical := storage.ChunkObjectKey(7, 12, 0)
	store.objects[canonical] = "bad!"
	if _, err := service.MergeChunks(context.Background(), repo.file.FileMD5, "doc.txt", 7); !errors.Is(err, storage.ErrMD5Mismatch) {
		t.Fatalf("corrupt merge was not rejected: %v", err)
	}
	if len(repo.marks[12]) != 0 || repo.file.Status != 0 {
		t.Fatalf("corrupt progress was not reset: marks=%v status=%d", repo.marks[12], repo.file.Status)
	}
	if _, _, err := service.UploadChunk(context.Background(), repo.file.FileMD5, "doc.txt", 4, 0, strings.NewReader("data"), 7, "", false); err != nil {
		t.Fatalf("correct retransmit failed: %v", err)
	}
	if store.objects[canonical] != "data" {
		t.Fatalf("retransmit did not replace canonical bytes: %q", store.objects[canonical])
	}
	if _, err := service.MergeChunks(context.Background(), repo.file.FileMD5, "doc.txt", 7); err != nil {
		t.Fatalf("corrected upload did not merge: %v", err)
	}
	if repo.file.Status != 1 || repo.queued != 1 {
		t.Fatalf("corrected upload was not published: status=%d queued=%d", repo.file.Status, repo.queued)
	}
}

func TestChunkSQLFailureLeavesProgressIncomplete(t *testing.T) {
	repo, store, service := newLifecycleUpload(t)
	repo.finalizeErr = errors.New("chunk_info commit failed")
	canonical := storage.ChunkObjectKey(7, 12, 0)

	if _, _, err := service.UploadChunk(context.Background(), repo.file.FileMD5, "doc.txt", 4, 0, strings.NewReader("data"), 7, "", false); !errors.Is(err, repo.finalizeErr) {
		t.Fatalf("SQL failure was hidden: %v", err)
	}
	if len(repo.marks[12]) != 0 {
		t.Fatalf("failed SQL publication left a completed Redis bit: %v", repo.marks[12])
	}
	if len(store.writes) != 1 || store.writes[0] == canonical || store.objects[canonical] != "data" {
		t.Fatalf("canonical object was published before SQL commit: writes=%v", store.writes)
	}
}

package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/log"
)

type uploadLimitRepo struct {
	repository.UploadRepository
	file  *model.FileUpload
	err   error
	reads int
}

func (r *uploadLimitRepo) GetFileUploadRecord(string, uint) (*model.FileUpload, error) {
	r.reads++
	return r.file, r.err
}

func TestUploadLimitsBeforeStorageAndMetadataWrites(t *testing.T) {
	log.Init("error", "json", "")
	const limit = DefaultChunkSize + 3
	const md5 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reachedRepo := errors.New("validated input reached repository")
	for _, test := range []struct {
		name                   string
		size                   int64
		index                  int
		body, filename, digest string
		want                   error
	}{
		{"above limit", limit + 1, 0, "", "doc.md", md5, ErrUploadTooLarge},
		{"empty", 0, 0, "", "doc.md", md5, ErrInvalidUpload},
		{"negative size", -1, 0, "", "doc.md", md5, ErrInvalidUpload},
		{"negative index", 4, -1, "data", "doc.md", md5, ErrInvalidUpload},
		{"past last chunk", 4, 1, "data", "doc.md", md5, ErrInvalidUpload},
		{"extra byte", 4, 0, "data!", "doc.md", md5, ErrInvalidUpload},
		{"missing byte", 4, 0, "dat", "doc.md", md5, ErrInvalidUpload},
		{"bad md5", 4, 0, "data", "doc.md", "../bad", ErrInvalidUpload},
		{"path filename", 4, 0, "data", "../doc.md", md5, ErrInvalidUpload},
		{"out of order unsupported type", limit, 1, "end", "doc.exe", md5, ErrInvalidUpload},
		{"valid small file", 4, 0, "data", "doc.md", md5, reachedRepo},
		{"valid full chunk", limit, 0, strings.Repeat("x", DefaultChunkSize), "doc.md", md5, reachedRepo},
		{"exact limit final chunk", limit, 1, "end", "doc.md", md5, reachedRepo},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &uploadLimitRepo{err: reachedRepo}
			s := NewUploadService(repo, nil, nil, limit)
			_, _, err := s.UploadChunk(context.Background(), test.digest, test.filename, test.size, test.index, strings.NewReader(test.body), 1, "", false)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if test.want != reachedRepo && repo.reads != 0 {
				t.Fatal("invalid upload reached persistence")
			}
		})
	}
	repo := &uploadLimitRepo{file: &model.FileUpload{FileName: "doc.md", TotalSize: 5}}
	s := NewUploadService(repo, nil, nil, limit)
	if _, _, err := s.UploadChunk(context.Background(), md5, "doc.md", 4, 0, strings.NewReader("data"), 1, "", false); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("accepted inconsistent resumed upload: %v", err)
	}
	repo.file.TotalSize = limit + 1
	if _, err := s.MergeChunks(context.Background(), md5, "doc.md", 1); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("merge bypassed processing limit: %v", err)
	}
	limits, _ := s.GetSupportedFileTypes()
	if limits["maxFileBytes"] != int64(limit) {
		t.Fatal("frontend limit differs from processing limit")
	}
}

func TestUploadReadsOnlyExpectedChunkPlusOneByte(t *testing.T) {
	log.Init("error", "json", "")
	reader := strings.NewReader(strings.Repeat("x", 100))
	s := NewUploadService(nil, nil, nil, 32<<20)
	_, _, err := s.UploadChunk(context.Background(), strings.Repeat("a", 32), "doc.txt", 4, 0, reader, 1, "", false)
	if !errors.Is(err, ErrInvalidUpload) || reader.Len() != 95 {
		t.Fatalf("unbounded chunk read: remaining=%d err=%v", reader.Len(), err)
	}
	var empty io.Reader
	if _, _, err := s.UploadChunk(context.Background(), strings.Repeat("a", 32), "doc.txt", 4, 0, empty, 1, "", false); !errors.Is(err, ErrInvalidUpload) {
		t.Fatal("nil input accepted")
	}
}

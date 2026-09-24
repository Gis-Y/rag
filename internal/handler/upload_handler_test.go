package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"pai-smart-go/internal/middleware"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/log"
)

type countedUploadBody struct {
	io.Reader
	read int
}

func (r *countedUploadBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func TestUploadRejectsOversizedMultipartBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log.Init("error", "json", "")
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "large.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(bytes.Repeat([]byte("x"), 8<<20))
	_ = form.Close()
	router := gin.New()
	router.Use(middleware.RequestLogger())
	router.POST("/upload", NewUploadHandler(nil).UploadChunk)
	countedBody := &countedUploadBody{Reader: &body}
	request := httptest.NewRequest(http.MethodPost, "/upload", countedBody)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, body %s", response.Code, response.Body.String())
	}
	if countedBody.read > service.DefaultChunkSize+(1<<20)+1 {
		t.Fatalf("request was buffered before the upload limit: read %d bytes", countedBody.read)
	}
	for _, test := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("context: %w", service.ErrUploadTooLarge), 413},
		{fmt.Errorf("context: %w", service.ErrInvalidUpload), 400},
		{errors.New("storage failed"), 500},
	} {
		if uploadErrorStatus(test.err) != test.status {
			t.Fatalf("incorrect status for %v", test.err)
		}
	}
}

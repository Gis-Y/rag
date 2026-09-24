package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"pai-smart-go/pkg/log"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type observedRequestBody struct {
	io.Reader
	reads, bytes int
}

func (b *observedRequestBody) Read(data []byte) (int, error) {
	b.reads++
	n, err := b.Reader.Read(data)
	b.bytes += n
	return n, err
}

func (*observedRequestBody) Close() error { return nil }

func TestRequestLoggerDoesNotReadMultipartBeforeHandlerLimit(t *testing.T) {
	log.Init("error", "json", "")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestLogger())
	body := &observedRequestBody{Reader: strings.NewReader("--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"doc.pdf\"\r\n\r\nlarge-document\r\n--boundary--\r\n")}
	const limit = 8
	router.POST("/upload", func(c *gin.Context) {
		if body.reads != 0 || c.Request.Body != body {
			t.Fatal("logger read or replaced the multipart stream before the handler")
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		_, err := io.ReadAll(c.Request.Body)
		var oversized *http.MaxBytesError
		if !errors.As(err, &oversized) {
			t.Fatalf("handler limit was bypassed: %v", err)
		}
		c.Status(http.StatusRequestEntityTooLarge)
	})
	request := httptest.NewRequest(http.MethodPost, "/upload", nil)
	request.Body = body
	request.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || body.bytes != limit+1 {
		t.Fatalf("status=%d read=%d bytes; expected rejection after %d bytes", response.Code, body.bytes, limit+1)
	}
}

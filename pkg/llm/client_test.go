package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"pai-smart-go/internal/config"
	"strings"
	"testing"
	"time"
)

func TestStreamChatMessages(t *testing.T) {
	tests := []struct {
		name              string
		body              string
		want              string
		wantErr           string
		wantUnexpectedEOF bool
	}{
		{
			name: "complete stream",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]",
			want: "Hello world",
		},
		{
			name: "length limit",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"length\"}]}\n\n" +
				"data: [DONE]\n\n",
			want:    "partial",
			wantErr: "finish_reason \"length\"",
		},
		{
			name:              "unexpected eof",
			body:              "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}",
			want:              "partial",
			wantUnexpectedEOF: true,
		},
		{
			name:              "stop without done marker",
			body:              "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}]}\n\n",
			want:              "partial",
			wantUnexpectedEOF: true,
		},
		{
			name:    "malformed chunk",
			body:    "data: {not-json}\n\n",
			wantErr: "failed to decode streamed chunk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			client := NewClient(config.LLMConfig{BaseURL: server.URL, Model: "test"})
			var got strings.Builder
			err := client.StreamChatMessages(context.Background(), []Message{{Role: "user", Content: "test"}}, nil, func(data []byte) error {
				_, _ = got.Write(data)
				return nil
			})

			if got.String() != tt.want {
				t.Fatalf("content = %q, want %q", got.String(), tt.want)
			}
			if tt.wantUnexpectedEOF {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("error = %v, want unexpected EOF", err)
				}
				return
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestStreamChatMessagesHasTotalDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	started := time.Now()
	err := NewClient(config.LLMConfig{BaseURL: server.URL, Model: "test", TimeoutSeconds: 1}).StreamChatMessages(
		context.Background(), []Message{{Role: "user", Content: "test"}}, nil, nil,
	)
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("stream deadline was not enforced: duration=%s err=%v", time.Since(started), err)
	}
}

func TestStreamChatMessagesBoundsUpstreamResponses(t *testing.T) {
	for name, statusAndBody := range map[string]struct {
		status int
		body   string
	}{
		"error body":    {http.StatusBadGateway, strings.Repeat("private-upstream-error", 1000)},
		"oversized SSE": {http.StatusOK, "data: " + strings.Repeat("x", (256<<10)+1) + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(statusAndBody.status)
				_, _ = io.WriteString(w, statusAndBody.body)
			}))
			defer server.Close()
			err := NewClient(config.LLMConfig{BaseURL: server.URL, Model: "test"}).StreamChatMessages(context.Background(), []Message{{Role: "user", Content: "test"}}, nil, nil)
			if err == nil || strings.Contains(err.Error(), "private-upstream-error") {
				t.Fatalf("unbounded or leaked upstream response: %v", err)
			}
		})
	}
}

// Package llm provides a client for interacting with Large Language Models.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"pai-smart-go/internal/config"
	"strings"
	"time"
)

// ChunkHandler 接收一个 LLM 文本分块，不携带任何传输层语义。
type ChunkHandler func(data []byte) error

// Client defines the interface for an LLM client.
type Client interface {
	// StreamChatMessages 以 role-based 消息与可选生成参数调用聊天接口，并将流式分块写入 writer。
	StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, onChunk ChunkHandler) error
}

type deepseekClient struct {
	cfg    config.LLMConfig
	client *http.Client
}

// NewClient creates a new LLM client based on the provider in the config.
func NewClient(cfg config.LLMConfig) Client {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &deepseekClient{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

// Message 表示一条角色消息
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// GenerationParams 控制生成行为
type GenerationParams struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
}

func (c *deepseekClient) StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, onChunk ChunkHandler) error {
	reqBody := chatRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   true,
	}
	// 从配置或传参注入生成参数（传参优先生效）
	if gen != nil {
		reqBody.Temperature = gen.Temperature
		reqBody.TopP = gen.TopP
		reqBody.MaxTokens = gen.MaxTokens
	} else {
		// 从客户端持有的配置注入（若非零值）
		if c.cfg.Generation.Temperature != 0 {
			t := c.cfg.Generation.Temperature
			reqBody.Temperature = &t
		}
		if c.cfg.Generation.TopP != 0 {
			p := c.cfg.Generation.TopP
			reqBody.TopP = &p
		}
		if c.cfg.Generation.MaxTokens != 0 {
			m := c.cfg.Generation.MaxTokens
			reqBody.MaxTokens = &m
		}
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return fmt.Errorf("failed to create chat request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call chat api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chat api returned non-200 status: %s", resp.Status)
	}

	const maxSSELineBytes = 256 << 10
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes)
	done := false
	finishReason := ""
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				done = true
			} else if data != "" {
				var chunk chatResponse
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					return fmt.Errorf("failed to decode streamed chunk: %w", err)
				}

				if len(chunk.Choices) > 0 {
					choice := chunk.Choices[0]
					if choice.FinishReason != "" {
						finishReason = choice.FinishReason
					}
					if onChunk != nil {
						if err := onChunk([]byte(choice.Delta.Content)); err != nil {
							return fmt.Errorf("failed to handle streamed chunk: %w", err)
						}
					}
				}
			}
		}

		if done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read from stream: %w", err)
	}
	if !done {
		return fmt.Errorf("failed to read from stream: %w", io.ErrUnexpectedEOF)
	}

	if finishReason != "" && finishReason != "stop" {
		return fmt.Errorf("chat completion stopped with finish_reason %q", finishReason)
	}
	return nil
}

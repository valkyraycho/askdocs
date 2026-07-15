package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxAttempts     = 3
	baseBackoff     = 500 * time.Millisecond
	maxResponseSize = 10 << 20 // 10MB
)

type Client struct {
	cfg Config
	hc  *http.Client
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, hc: &http.Client{}}
}

func (c *Client) Host() string       { return c.cfg.Host() }
func (c *Client) EmbedModel() string { return c.cfg.EmbedModel }

func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	payload, err := json.Marshal(map[string]any{
		"model": c.cfg.EmbedModel,
		"input": texts,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embeddings request: %w", err)
	}
	resp, err := c.doWithRetry(ctx, "/embeddings", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read embeddings response: %w", err)
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse embeddings response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings response has %d items for %d inputs", len(parsed.Data), len(texts))
	}

	out := make([][]float32, len(texts))
	dims := -1
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("embeddings response index %d out of range", item.Index)
		}
		if out[item.Index] != nil {
			return nil, fmt.Errorf("embeddings response has duplicate index %d", item.Index)
		}
		if dims == -1 {
			dims = len(item.Embedding)
			if dims == 0 {
				return nil, errors.New("embeddings response contains an empty vector")
			}
		} else if len(item.Embedding) != dims {
			return nil, fmt.Errorf("embeddings response mixes dimensions %d and %d", dims, len(item.Embedding))
		}
		v := make([]float32, len(item.Embedding))
		for i, f := range item.Embedding {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, fmt.Errorf("embeddings response contains non-finite value at index %d", item.Index)
			}
			v[i] = float32(f)
		}
		out[item.Index] = v
	}
	return out, nil
}

func (c *Client) ChatStream(ctx context.Context, system, user string, onDelta func(string) error) error {
	payload, err := json.Marshal(chatRequest(c.cfg, system, user, true))
	if err != nil {
		return fmt.Errorf("marshal chat request: %w", err)
	}
	resp, err := c.doWithRetry(ctx, "/chat/completions", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		data, found := strings.CutPrefix(line, "data:")
		if !found {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("parse stream chunk: %w", err)
		}
		if len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Content == "" {
			continue
		}
		if err := onDelta(chunk.Choices[0].Delta.Content); err != nil {
			return fmt.Errorf("stream aborted: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read chat stream: %w", err)
	}
	return nil
}

func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	payload, err := json.Marshal(chatRequest(c.cfg, system, user, false))
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}
	resp, err := c.doWithRetry(ctx, "/chat/completions", payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return "", fmt.Errorf("read chat response: %w", err)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse chat response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("chat response has no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

func chatRequest(cfg Config, system, user string, stream bool) map[string]any {
	return map[string]any{
		"model":      cfg.ChatModel,
		"stream":     stream,
		"max_tokens": cfg.MaxAnswerTokens,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
}

func (c *Client) doWithRetry(ctx context.Context, path string, payload []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimSuffix(c.cfg.BaseURL, "/")+path, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}

		var serverWait time.Duration
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("call %s: %w", path, err)
		} else if resp.StatusCode == http.StatusOK {
			return resp, nil
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("%s returned %d: %.300s", path, resp.StatusCode, string(body))
			if !retryable(resp.StatusCode) {
				return nil, lastErr
			}
			serverWait = retryAfter(resp.Header.Get("Retry-After"))
		}
		if attempt == maxAttempts {
			break // no sleep before giving up
		}
		wait := serverWait
		if wait == 0 {
			backoff := baseBackoff * (1 << (attempt - 1))
			wait = backoff + time.Duration(rand.Int63n(int64(backoff)/2))
		}
		if !sleepCtx(ctx, wait) {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func retryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

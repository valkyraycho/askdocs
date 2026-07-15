package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:         baseURL,
		EmbedModel:      "fake-embed",
		ChatModel:       "fake-chat",
		MaxAnswerTokens: 64,
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"https remote with key", map[string]string{"OPENAI_BASE_URL": "https://api.example.com/v1", "OPENAI_API_KEY": "sk-x"}, false},
		{"https remote without key", map[string]string{"OPENAI_BASE_URL": "https://api.example.com/v1"}, true},
		{"http loopback keyless (ollama)", map[string]string{"OPENAI_BASE_URL": "http://localhost:11434/v1"}, false},
		{"http 127.0.0.1 keyless", map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:8080/v1"}, false},
		{"http remote rejected", map[string]string{"OPENAI_BASE_URL": "http://api.example.com/v1", "OPENAI_API_KEY": "sk-x"}, true},
		{"http remote with insecure override", map[string]string{"OPENAI_BASE_URL": "http://api.example.com/v1", "OPENAI_API_KEY": "sk-x", "ASKDOCS_ALLOW_INSECURE": "1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "ASKDOCS_ALLOW_INSECURE", "ASKDOCS_EMBED_MODEL", "ASKDOCS_CHAT_MODEL", "ASKDOCS_MAX_ANSWER_TOKENS"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := ConfigFromEnv()
			if (err != nil) != tt.wantErr {
				t.Errorf("ConfigFromEnv() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestClientAccessors(t *testing.T) {
	cfg := testConfig("https://api.example.com/v1")
	c := New(cfg)
	if c.Host() != "api.example.com" || c.EmbedModel() != "fake-embed" {
		t.Errorf("accessors: %q %q", c.Host(), c.EmbedModel())
	}
}

func TestConfigHost(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://api.rdsec.example.com/prod/aiendpoint/v1")
	t.Setenv("OPENAI_API_KEY", "sk-x")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Host() != "api.rdsec.example.com" {
		t.Errorf("Host = %q", cfg.Host())
	}
}

func embedResponse(indices []int, vecs [][]float32) string {
	items := make([]string, len(indices))
	for i, idx := range indices {
		b, _ := json.Marshal(vecs[i])
		items[i] = fmt.Sprintf(`{"index":%d,"embedding":%s}`, idx, b)
	}
	return `{"data":[` + strings.Join(items, ",") + `]}`
}

func TestEmbedReordersByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("keyless client sent Authorization %q", got)
		}
		// deliberately out of order
		fmt.Fprint(w, embedResponse([]int{1, 0}, [][]float32{{2, 2}, {1, 1}}))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	got, err := c.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got[0][0] != 1 || got[1][0] != 2 {
		t.Errorf("not reordered by index: %v", got)
	}
}

func TestEmbedValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"wrong count", embedResponse([]int{0}, [][]float32{{1}})},
		{"duplicate index", embedResponse([]int{0, 0}, [][]float32{{1}, {2}})},
		{"out of range index", embedResponse([]int{0, 5}, [][]float32{{1}, {2}})},
		{"dims mismatch", embedResponse([]int{0, 1}, [][]float32{{1, 2}, {1}})},
		{"non-finite", `{"data":[{"index":0,"embedding":[1e999]},{"index":1,"embedding":[1]}]}`},
		{"garbage", `{not json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			c := New(testConfig(srv.URL))
			if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
				t.Errorf("Embed accepted %s, want error", tt.name)
			}
		})
	}
}

func TestEmbedRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, embedResponse([]int{0}, [][]float32{{1, 2}}))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed after retries: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestEmbedRetriesBounded(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	if _, err := c.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatalf("Embed succeeded against permanent 503, want error")
	}
	if calls.Load() != maxAttempts {
		t.Errorf("calls = %d, want %d", calls.Load(), maxAttempts)
	}
}

func TestEmbedNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"bad model"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	if _, err := c.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatalf("want error on 400")
	}
	if calls.Load() != 1 {
		t.Errorf("client retried a 400: %d calls", calls.Load())
	}
}

func chatSSEBody(tokens ...string) string {
	var b strings.Builder
	for _, tok := range tokens {
		j, _ := json.Marshal(tok)
		fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", j)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			t.Errorf("stream = %v", req["stream"])
		}
		if int(req["max_tokens"].(float64)) != 64 {
			t.Errorf("max_tokens = %v", req["max_tokens"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chatSSEBody("Hel", "lo ", "world\nnewline"))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	var got strings.Builder
	err := c.ChatStream(context.Background(), "sys", "user", func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != "Hello world\nnewline" {
		t.Errorf("streamed = %q", got.String())
	}
}

func TestChatStreamOnDeltaErrorAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chatSSEBody("a", "b", "c"))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	wantErr := fmt.Errorf("cap exceeded")
	calls := 0
	err := c.ChatStream(context.Background(), "s", "u", func(string) error {
		calls++
		return wantErr
	})
	if err == nil || !strings.Contains(err.Error(), "cap exceeded") {
		t.Errorf("err = %v, want cap exceeded", err)
	}
	if calls != 1 {
		t.Errorf("onDelta called %d times after abort, want 1", calls)
	}
}

func TestChatStreamContextCancelReachesServer(t *testing.T) {
	serverSawCancel := make(chan struct{})
	firstToken := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(firstToken)
		select {
		case <-r.Context().Done():
			close(serverSawCancel)
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New(testConfig(srv.URL))
	done := make(chan error, 1)
	go func() {
		done <- c.ChatStream(ctx, "s", "u", func(string) error { return nil })
	}()
	<-firstToken
	cancel()
	select {
	case <-serverSawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("server never observed client cancellation")
	}
	<-done
}

func TestChatNonStreamFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] == true {
			t.Errorf("fallback sent stream:true")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"full answer"}}]}`)
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	got, err := c.Chat(context.Background(), "s", "u")
	if err != nil || got != "full answer" {
		t.Errorf("Chat = %q, %v", got, err)
	}
}

func TestAuthorizationHeaderSentWhenKeyed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, embedResponse([]int{0}, [][]float32{{1}}))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.APIKey = "sk-test"
	if _, err := New(cfg).Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
}

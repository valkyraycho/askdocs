package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valkyraycho/askdocs/internal/store"
)

const testPort = 4712

type fakeLLM struct {
	tokens    []string
	embedErr  error
	chatErr   error
	chatCalls atomic.Int32
	blockChat chan struct{} // when set, ChatStream blocks until closed or ctx done
	sawCancel chan struct{}
}

func (f *fakeLLM) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

func (f *fakeLLM) Chat(_ context.Context, _, _ string) (string, error) {
	f.chatCalls.Add(1)
	if f.chatErr != nil {
		return "", f.chatErr
	}
	return strings.Join(f.tokens, ""), nil
}

func (f *fakeLLM) ChatStream(ctx context.Context, _, _ string, onDelta func(string) error) error {
	f.chatCalls.Add(1)
	if f.chatErr != nil {
		return f.chatErr
	}
	if f.blockChat != nil {
		select {
		case <-ctx.Done():
			if f.sawCancel != nil {
				close(f.sawCancel)
			}
			return ctx.Err()
		case <-f.blockChat:
		}
	}
	for _, tok := range f.tokens {
		if err := onDelta(tok); err != nil {
			return err
		}
	}
	return nil
}

func newTestCorpus(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenIngest(filepath.Join(t.TempDir(), "askdocs.db"))
	if err != nil {
		t.Fatalf("OpenIngest: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureSpace(store.EmbeddingSpace{Model: "fake-embed", Dims: 4, Host: "fake.test"}, false); err != nil {
		t.Fatalf("EnsureSpace: %v", err)
	}
	if err := st.ReplaceFile("guide.md", "h1", []store.ChunkInput{
		{Heading: "guide.md › Retry", Content: "## Retry\n\nuse exponential backoff for docker restarts", Vec: []float32{1, 0, 0, 0}},
		{Heading: "guide.md › Limits", Content: "cap attempts at three", Vec: []float32{0, 1, 0, 0}},
	}); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	return st
}

func handlerWith(t *testing.T, llm LLM) http.Handler {
	t.Helper()
	return New(newTestCorpus(t), llm, testPort, "testcorpus")
}

func get(t *testing.T, h http.Handler, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIndex(t *testing.T) {
	rec := get(t, handlerWith(t, &fakeLLM{}), "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"testcorpus", "guide.md", "2 chunks", "fake-embed", "/static/htmx.min.js", "/static/sse.js", `hx-get="/ask/connect"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
}

func TestSearchKeywordTier(t *testing.T) {
	h := handlerWith(t, &fakeLLM{})
	body := get(t, h, "/search?q=backoff", nil).Body.String()
	if !strings.Contains(body, "keyword") || !strings.Contains(body, "guide.md › Retry") {
		t.Errorf("search fragment = %q", body)
	}
	for _, q := range []string{"%22", "AND", "%28%28%28", strings.Repeat("x", 2048)} {
		if code := get(t, h, "/search?q="+q, nil).Code; code != http.StatusOK {
			t.Errorf("hostile q=%s → %d", q, code)
		}
	}
}

func TestHybridTier(t *testing.T) {
	h := handlerWith(t, &fakeLLM{})
	body := get(t, h, "/hybrid?q=backoff", nil).Body.String()
	if !strings.Contains(body, "hybrid") || !strings.Contains(body, "guide.md") {
		t.Errorf("hybrid fragment = %q", body)
	}
}

func TestHybridWithoutEmbeddingsFallsBack(t *testing.T) {
	st, err := store.OpenIngest(filepath.Join(t.TempDir(), "askdocs.db"))
	if err != nil {
		t.Fatalf("OpenIngest: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h := New(st, &fakeLLM{}, testPort, "empty")
	rec := get(t, h, "/hybrid?q=anything", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "no embeddings") {
		t.Errorf("hybrid on empty corpus: %d %q", rec.Code, rec.Body.String())
	}
}

func TestChunkDetailAndNeighbors(t *testing.T) {
	st := newTestCorpus(t)
	h := New(st, &fakeLLM{}, testPort, "c")
	chunks, _ := st.FileChunks("guide.md")

	body := get(t, h, fmt.Sprintf("/chunks/%d", chunks[0].ID), nil).Body.String()
	if !strings.Contains(body, "<h2>Retry</h2>") {
		t.Errorf("markdown not rendered: %q", body)
	}
	if !strings.Contains(body, fmt.Sprintf("/chunks/%d", chunks[1].ID)) {
		t.Errorf("next-neighbor link missing")
	}
	if code := get(t, h, "/chunks/9999", nil).Code; code != http.StatusNotFound {
		t.Errorf("missing chunk → %d", code)
	}
	if code := get(t, h, "/chunks/abc", nil).Code; code != http.StatusBadRequest {
		t.Errorf("bad id → %d", code)
	}
}

func TestAskConnect(t *testing.T) {
	h := handlerWith(t, &fakeLLM{})
	body := get(t, h, "/ask/connect?q=how+to+retry", nil).Body.String()
	if !strings.Contains(body, `sse-connect="/ask/stream?once=`) && !strings.Contains(body, "once=") {
		t.Errorf("connector missing nonce url: %q", body)
	}
	if !strings.Contains(body, `sse-swap="done"`) || !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Errorf("connector must replace itself on done: %q", body)
	}
	if code := get(t, h, "/ask/connect?q=", nil).Code; code != http.StatusBadRequest {
		t.Errorf("empty q → %d", code)
	}
	long := url.QueryEscape(strings.Repeat("x", 600))
	if code := get(t, h, "/ask/connect?q="+long, nil).Code; code != http.StatusBadRequest {
		t.Errorf("oversized q → %d", code)
	}
}

func TestSecurityHeadersAndHostAndSecFetch(t *testing.T) {
	h := handlerWith(t, &fakeLLM{})
	rec := get(t, h, "/", nil)
	for k, v := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q", k, got)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example.com:4712"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign Host → %d", rec.Code)
	}

	if code := get(t, h, "/ask/stream?q=x", map[string]string{"Sec-Fetch-Site": "cross-site"}).Code; code != http.StatusForbidden {
		t.Errorf("cross-site fetch → %d, want 403", code)
	}
	if code := get(t, h, "/search?q=x", map[string]string{"Sec-Fetch-Site": "same-origin"}).Code; code != http.StatusOK {
		t.Errorf("same-origin fetch → %d", code)
	}
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	name string
	data string
}

func readSSE(t *testing.T, resp *http.Response, max int, timeout time.Duration) []sseEvent {
	t.Helper()
	type result struct {
		events []sseEvent
	}
	ch := make(chan result, 1)
	go func() {
		var events []sseEvent
		var cur sseEvent
		var data []string
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			case line == "":
				if cur.name != "" || len(data) > 0 {
					cur.data = strings.Join(data, "\n")
					events = append(events, cur)
					cur = sseEvent{}
					data = nil
					if len(events) >= max {
						ch <- result{events}
						return
					}
				}
			}
		}
		ch <- result{events}
	}()
	select {
	case r := <-ch:
		return r.events
	case <-time.After(timeout):
		t.Fatalf("timed out reading SSE events")
		return nil
	}
}

func startServer(t *testing.T, st *store.Store, llm LLM) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(st, llm, testPort, "c"))
	t.Cleanup(srv.Close)
	return srv
}

// sseGet requests an SSE endpoint with the Host header the handler expects.
func sseGet(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func connectURL(t *testing.T, srv *httptest.Server, q string) string {
	t.Helper()
	resp := sseGet(t, srv, "/ask/connect?q="+url.QueryEscape(q))
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	m := regexp.MustCompile(`sse-connect="([^"]+)"`).FindSubmatch(buf[:n])
	if m == nil {
		t.Fatalf("no sse-connect in connector: %s", buf[:n])
	}
	return strings.ReplaceAll(string(m[1]), "&amp;", "&")
}

func TestAskStreamFullFlow(t *testing.T) {
	st := newTestCorpus(t)
	llm := &fakeLLM{tokens: []string{"Use <script>backoff</script> ", "per [1]."}}
	srv := startServer(t, st, llm)

	resp := sseGet(t, srv, connectURL(t, srv, "how to retry"))
	events := readSSE(t, resp, 4, 5*time.Second)
	if len(events) != 4 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].name != "citations" || !strings.Contains(events[0].data, "guide.md") {
		t.Errorf("first event = %+v, want citations", events[0])
	}
	if events[1].name != "token" || !strings.Contains(events[1].data, "&lt;script&gt;") {
		t.Errorf("token not escaped: %+v", events[1])
	}
	done := events[3]
	if done.name != "done" {
		t.Errorf("last event = %q", done.name)
	}
	if !strings.Contains(done.data, `class="cite"`) || !strings.Contains(done.data, "/chunks/") {
		t.Errorf("done missing linked citation: %q", done.data)
	}
	if strings.Contains(done.data, "<script>backoff") {
		t.Errorf("raw script in rendered answer")
	}
}

func TestAskStreamNonceReplayDoesNotRebill(t *testing.T) {
	st := newTestCorpus(t)
	llm := &fakeLLM{tokens: []string{"answer"}}
	srv := startServer(t, st, llm)

	streamURL := connectURL(t, srv, "question")
	resp := sseGet(t, srv, streamURL)
	readSSE(t, resp, 3, 5*time.Second)
	if llm.chatCalls.Load() != 1 {
		t.Fatalf("first stream chatCalls = %d", llm.chatCalls.Load())
	}

	replay := sseGet(t, srv, streamURL)
	events := readSSE(t, replay, 1, 5*time.Second)
	if len(events) != 1 || events[0].name != "done" || !strings.Contains(events[0].data, "interrupted") {
		t.Errorf("replay events = %+v, want immediate done", events)
	}
	if llm.chatCalls.Load() != 1 {
		t.Errorf("replay re-billed: chatCalls = %d", llm.chatCalls.Load())
	}
}

func TestAskStreamStreamsBeforeCompletion(t *testing.T) {
	st := newTestCorpus(t)
	llm := &fakeLLM{blockChat: make(chan struct{}), tokens: []string{"late answer"}}
	srv := startServer(t, st, llm)

	resp := sseGet(t, srv, connectURL(t, srv, "question"))
	events := readSSE(t, resp, 1, 5*time.Second) // citations must arrive while chat is still blocked
	if len(events) != 1 || events[0].name != "citations" {
		t.Fatalf("first streamed event = %+v", events)
	}
	close(llm.blockChat)
}

func TestAskStreamClientDisconnectCancelsUpstream(t *testing.T) {
	st := newTestCorpus(t)
	llm := &fakeLLM{blockChat: make(chan struct{}), sawCancel: make(chan struct{})}
	srv := startServer(t, st, llm)

	resp := sseGet(t, srv, connectURL(t, srv, "question"))
	readSSE(t, resp, 1, 5*time.Second) // citations arrived, chat now blocked
	resp.Body.Close()

	select {
	case <-llm.sawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw cancellation after client disconnect")
	}
}

func TestAskStreamSemaphoreBusy(t *testing.T) {
	st := newTestCorpus(t)
	llm := &fakeLLM{blockChat: make(chan struct{})}
	srv := startServer(t, st, llm)

	for range maxConcurrentAsks {
		resp := sseGet(t, srv, connectURL(t, srv, "slow question"))
		readSSE(t, resp, 1, 5*time.Second)
	}
	resp := sseGet(t, srv, connectURL(t, srv, "one too many"))
	events := readSSE(t, resp, 2, 5*time.Second)
	found := false
	for _, e := range events {
		if e.name == "done" && strings.Contains(e.data, "too many questions") {
			found = true
		}
	}
	if !found {
		t.Errorf("third ask not rejected as busy: %+v", events)
	}
	close(llm.blockChat)
}

func TestAskStreamChatFailureSendsTerminalError(t *testing.T) {
	st := newTestCorpus(t)
	llm := &fakeLLM{chatErr: errors.New("upstream exploded")}
	srv := startServer(t, st, llm)

	resp := sseGet(t, srv, connectURL(t, srv, "question"))
	events := readSSE(t, resp, 2, 5*time.Second)
	last := events[len(events)-1]
	if last.name != "done" || !strings.Contains(last.data, "ask again") {
		t.Errorf("failure did not produce terminal done event: %+v", events)
	}
}

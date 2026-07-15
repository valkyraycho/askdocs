package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func startFakeOpenAI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		items := make([]string, len(req.Input))
		for i, text := range req.Input {
			h := fnv.New32a()
			h.Write([]byte(text))
			v := h.Sum32()
			items[i] = fmt.Sprintf(`{"index":%d,"embedding":[%d,%d,%d,%d]}`,
				i, v&0xff, v>>8&0xff, v>>16&0xff, v>>24&0xff)
		}
		fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(items, ","))
	})
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fake answer [1]\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"fake plain answer [1]"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func setupCorpusEnv(t *testing.T) string {
	t.Helper()
	srv := startFakeOpenAI(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "guide.md"),
		[]byte("# Guide\n\n## Retries\n\nretry three times with backoff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"OPENAI_BASE_URL":     srv.URL, // loopback → keyless is allowed
		"OPENAI_API_KEY":      "",
		"ASKDOCS_DB":          filepath.Join(root, "askdocs.db"),
		"ASKDOCS_EMBED_MODEL": "",
		"ASKDOCS_CHAT_MODEL":  "",
		"ASKDOCS_NO_STREAM":   "",
	} {
		t.Setenv(k, v)
	}
	return root
}

func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	code := fn()
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out), code
}

func TestCLIEndToEnd(t *testing.T) {
	root := setupCorpusEnv(t)

	out, code := captureStdout(t, func() int { return run([]string{"askdocs", "ingest", root}) })
	if code != 0 {
		t.Fatalf("ingest exit = %d (%s)", code, out)
	}
	if !strings.Contains(out, "embedded 1") {
		t.Errorf("ingest output = %q", out)
	}

	out, code = captureStdout(t, func() int { return run([]string{"askdocs", "search", "retry", "backoff"}) })
	if code != 0 || !strings.Contains(out, "hybrid results") || !strings.Contains(out, "guide.md") {
		t.Errorf("search: exit %d output %q", code, out)
	}

	out, code = captureStdout(t, func() int { return run([]string{"askdocs", "ask", "how many retries?"}) })
	if code != 0 || !strings.Contains(out, "fake answer") || !strings.Contains(out, "sources:") {
		t.Errorf("ask: exit %d output %q", code, out)
	}

	out, code = captureStdout(t, func() int { return run([]string{"askdocs", "status"}) })
	if code != 0 || !strings.Contains(out, "files:   1") || !strings.Contains(out, "text-embedding-3-small") {
		t.Errorf("status: exit %d output %q", code, out)
	}

	// second ingest is incremental
	out, code = captureStdout(t, func() int { return run([]string{"askdocs", "ingest", root}) })
	if code != 0 || !strings.Contains(out, "skipped 1") {
		t.Errorf("re-ingest: exit %d output %q", code, out)
	}
}

func TestCLIAskNoStreamFallback(t *testing.T) {
	root := setupCorpusEnv(t)
	if _, code := captureStdout(t, func() int { return run([]string{"askdocs", "ingest", root}) }); code != 0 {
		t.Fatalf("ingest failed")
	}
	t.Setenv("ASKDOCS_NO_STREAM", "1")
	out, code := captureStdout(t, func() int { return run([]string{"askdocs", "ask", "how many retries?"}) })
	if code != 0 || !strings.Contains(out, "fake plain answer") {
		t.Errorf("no-stream ask: exit %d output %q", code, out)
	}
}

func TestCLISearchFallsBackWithoutProvider(t *testing.T) {
	root := setupCorpusEnv(t)
	if _, code := captureStdout(t, func() int { return run([]string{"askdocs", "ingest", root}) }); code != 0 {
		t.Fatalf("ingest failed")
	}
	t.Setenv("OPENAI_BASE_URL", "https://api.example.com/v1") // remote without key → invalid config
	out, code := captureStdout(t, func() int { return run([]string{"askdocs", "search", "retry"}) })
	if code != 0 || !strings.Contains(out, "keyword results") {
		t.Errorf("keyword fallback: exit %d output %q", code, out)
	}
}

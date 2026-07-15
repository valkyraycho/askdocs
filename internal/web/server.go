package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

//go:embed templates/* static/*
var assets embed.FS

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
	streamLifetime    = 10 * time.Minute
	nonceTTL          = 10 * time.Minute
	maxConcurrentAsks = 2
	maxAskChars       = 512
	searchLimit       = 20
)

type LLM interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	ChatStream(ctx context.Context, system, user string, onDelta func(string) error) error
	Chat(ctx context.Context, system, user string) (string, error)
}

type server struct {
	store  Corpus
	llm    LLM
	tmpl   *template.Template
	nonces *nonceSet
	askSem chan struct{}
	name   string
}

func New(st Corpus, llmClient LLM, port int, corpusName string) http.Handler {
	funcs := template.FuncMap{
		"snippet": snippet,
	}
	s := &server{
		store:  st,
		llm:    llmClient,
		tmpl:   template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")),
		nonces: newNonceSet(nonceTTL),
		askSem: make(chan struct{}, maxConcurrentAsks),
		name:   corpusName,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /hybrid", s.hybrid)
	mux.HandleFunc("GET /chunks/{id}", s.chunk)
	mux.HandleFunc("GET /ask/connect", s.askConnect)
	mux.HandleFunc("GET /ask/stream", s.askStream)
	mux.Handle("GET /static/", http.FileServerFS(assets))
	return securityHeaders(hostCheck(port, secFetchCheck(mux)))
}

func Serve(st Corpus, llmClient LLM, port int, corpusName string) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	srv := &http.Server{
		Handler:           New(st, llmClient, port, corpusName),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		// no WriteTimeout: SSE streams outlive any static deadline; per-write
		// deadlines are set in sseWriter instead
	}
	fmt.Printf("askdocs web UI: http://127.0.0.1:%d  (ctrl-c to stop)\n", port)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func hostCheck(port int, next http.Handler) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secFetchCheck rejects cross-site requests. Unlike an Origin check it also
// covers GETs (an <img src> pointed at /ask/stream would otherwise trigger
// API spend). Non-browser clients don't send the header and pass.
func secFetchCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "cross-site requests are not allowed", http.StatusForbidden)
		}
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type nonceSet struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

func newNonceSet(ttl time.Duration) *nonceSet {
	return &nonceSet{m: make(map[string]time.Time), ttl: ttl}
}

func (n *nonceSet) Mint() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint nonce: %w", err)
	}
	id := hex.EncodeToString(raw)
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	for k, exp := range n.m {
		if now.After(exp) {
			delete(n.m, k)
		}
	}
	n.m[id] = now.Add(n.ttl)
	return id, nil
}

// Consume atomically claims a nonce: the first caller wins, every replay
// (an EventSource auto-reconnect) is rejected and must not re-bill.
func (n *nonceSet) Consume(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	exp, ok := n.m[id]
	if !ok {
		return false
	}
	delete(n.m, id)
	return time.Now().Before(exp)
}

func snippet(content string) string {
	const max = 240
	runes := []rune(content)
	if len(runes) <= max {
		return content
	}
	return string(runes[:max]) + "…"
}

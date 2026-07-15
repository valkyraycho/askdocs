package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/valkyraycho/askdocs/internal/rag"
	"github.com/valkyraycho/askdocs/internal/render"
	"github.com/valkyraycho/askdocs/internal/store"
)

const maxAnswerBytes = 256 << 10 // 256KB accumulation cap for the rendered answer

type Corpus interface {
	SearchFTS(query string, limit int) ([]store.Hit, error)
	SearchHybrid(query string, vec []float32, limit int) ([]store.Hit, error)
	GetChunk(id int64) (store.Chunk, error)
	FileChunks(path string) ([]store.Chunk, error)
	Files() ([]store.FileInfo, error)
	Stats() (store.Stats, error)
}

type indexData struct {
	Name  string
	Stats store.Stats
	Files []store.FileInfo
}

type resultsData struct {
	Label string
	Hits  []store.Hit
	Empty string
}

type chunkData struct {
	Chunk  store.Chunk
	HTML   template.HTML
	PrevID int64
	NextID int64
}

type connectorData struct {
	StreamURL string
}

type answerData struct {
	Hits    []store.Hit
	Answer  template.HTML
	Message string
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats()
	if err != nil {
		s.fail(w, err)
		return
	}
	files, err := s.store.Files()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "index.html", indexData{Name: s.name, Stats: stats, Files: files})
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	hits, err := s.store.SearchFTS(r.FormValue("q"), searchLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "results.html", resultsData{Label: "keyword", Hits: hits, Empty: "no keyword matches"})
}

func (s *server) hybrid(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	if q == "" {
		s.search(w, r)
		return
	}
	vecs, err := s.llm.Embed(r.Context(), []string{q})
	if err != nil {
		s.fail(w, err)
		return
	}
	hits, err := s.store.SearchHybrid(q, vecs[0], searchLimit)
	if errors.Is(err, store.ErrNoEmbeddings) {
		s.render(w, "results.html", resultsData{Label: "keyword", Empty: err.Error()})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "results.html", resultsData{Label: "hybrid", Hits: hits, Empty: "no matches"})
}

func (s *server) chunk(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid chunk id", http.StatusBadRequest)
		return
	}
	c, err := s.store.GetChunk(id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "chunk not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	html, err := render.Markdown(c.Content)
	if err != nil {
		s.fail(w, err)
		return
	}
	data := chunkData{Chunk: c, HTML: html}
	if siblings, err := s.store.FileChunks(c.Path); err == nil {
		for i, sib := range siblings {
			if sib.ID != c.ID {
				continue
			}
			if i > 0 {
				data.PrevID = siblings[i-1].ID
			}
			if i < len(siblings)-1 {
				data.NextID = siblings[i+1].ID
			}
			break
		}
	}
	s.render(w, "chunk.html", data)
}

func (s *server) askConnect(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	if q == "" {
		http.Error(w, "question is empty", http.StatusBadRequest)
		return
	}
	if len([]rune(q)) > maxAskChars {
		http.Error(w, fmt.Sprintf("question is longer than %d characters", maxAskChars), http.StatusBadRequest)
		return
	}
	nonce, err := s.nonces.Mint()
	if err != nil {
		s.fail(w, err)
		return
	}
	v := url.Values{}
	v.Set("q", q)
	v.Set("once", nonce)
	s.render(w, "connector.html", connectorData{StreamURL: "/ask/stream?" + v.Encode()})
}

func (s *server) askStream(w http.ResponseWriter, r *http.Request) {
	sw := newSSEWriter(w)
	q := strings.TrimSpace(r.FormValue("q"))

	if !s.nonces.Consume(r.FormValue("once")) {
		s.sendDone(sw, answerData{Message: "connection interrupted — ask again to retry"})
		return
	}
	if q == "" || len([]rune(q)) > maxAskChars {
		s.sendDone(sw, answerData{Message: "invalid question"})
		return
	}
	select {
	case s.askSem <- struct{}{}:
		defer func() { <-s.askSem }()
	default:
		s.sendDone(sw, answerData{Message: "too many questions in flight — try again in a moment"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), streamLifetime)
	defer cancel()

	vecs, err := s.llm.Embed(ctx, []string{q})
	if err != nil {
		log.Printf("askdocs web: embed question: %v", err)
		s.sendDone(sw, answerData{Message: "could not embed the question — check the provider configuration"})
		return
	}
	hits, err := s.store.SearchHybrid(q, vecs[0], rag.TopChunks)
	if err != nil {
		log.Printf("askdocs web: retrieve: %v", err)
		s.sendDone(sw, answerData{Message: "retrieval failed: " + err.Error()})
		return
	}
	if err := sw.Send("citations", s.fragment("citations.html", hits)); err != nil {
		return // client gone
	}

	system, user := rag.BuildPrompt(q, hits)
	var answer bytes.Buffer
	onDelta := func(delta string) error {
		if answer.Len()+len(delta) > maxAnswerBytes {
			return errors.New("answer exceeded size cap")
		}
		answer.WriteString(delta)
		return sw.Send("token", template.HTMLEscapeString(delta))
	}
	// ponytail: ASKDOCS_NO_STREAM works around gateways whose SSE proxying
	// misbehaves — one whole-answer "token" then the normal done event.
	var streamErr error
	if os.Getenv("ASKDOCS_NO_STREAM") != "" {
		var full string
		if full, streamErr = s.llm.Chat(ctx, system, user); streamErr == nil {
			streamErr = onDelta(full)
		}
	} else {
		streamErr = s.llm.ChatStream(ctx, system, user, onDelta)
	}
	if streamErr != nil {
		log.Printf("askdocs web: chat stream: %v", streamErr)
		s.sendDone(sw, answerData{Hits: hits, Message: "answer stream failed — ask again to retry"})
		return
	}

	html, err := render.Markdown(answer.String())
	if err != nil {
		log.Printf("askdocs web: render answer: %v", err)
		s.sendDone(sw, answerData{Hits: hits, Message: "could not render the answer"})
		return
	}
	linked := rag.LinkCitations(string(html), hits)
	s.sendDone(sw, answerData{Hits: hits, Answer: template.HTML(linked)})
}

// sendDone emits the terminal SSE event. Its payload replaces the element
// carrying sse-connect, removing the EventSource so the htmx SSE extension
// cannot auto-reconnect and re-bill.
func (s *server) sendDone(sw *sseWriter, data answerData) {
	if err := sw.Send("done", s.fragment("answer.html", data)); err != nil {
		log.Printf("askdocs web: send done: %v", err)
	}
}

func (s *server) fragment(name string, data any) string {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("askdocs web: render %s: %v", name, err)
		return "<p>internal error</p>"
	}
	return buf.String()
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := buf.WriteTo(w); err != nil {
		log.Printf("askdocs web: write response: %v", err)
	}
}

func (s *server) fail(w http.ResponseWriter, err error) {
	log.Printf("askdocs web: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

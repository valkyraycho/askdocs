package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const sseWriteDeadline = 10 * time.Second

// sseWriter frames server-sent events per the spec: payload newlines become
// separate `data:` fields (a naive single `data:` line would truncate events
// or let payload lines masquerade as fields). Every write carries its own
// deadline so a stalled client is dropped instead of pinning the handler.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

func (s *sseWriter) Send(event, data string) error {
	if err := s.rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("set write deadline: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")
	if _, err := fmt.Fprint(s.w, b.String()); err != nil {
		return fmt.Errorf("write sse event: %w", err)
	}
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("flush sse event: %w", err)
	}
	return nil
}

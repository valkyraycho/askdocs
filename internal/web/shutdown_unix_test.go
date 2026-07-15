//go:build unix

package web

import (
	"fmt"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestServeGracefulShutdownOnSignal(t *testing.T) {
	st := newTestCorpus(t)
	const port = 47621
	done := make(chan error, 1)
	go func() { done <- Serve(st, &fakeLLM{}, port, "sig") }()

	up := false
	for range 100 {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				up = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatal("server never became reachable")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not shut down within 5s of SIGINT")
	}
}

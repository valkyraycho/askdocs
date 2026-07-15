//go:build unix

package main

import (
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestCmdWebServesAndShutsDown(t *testing.T) {
	root := setupCorpusEnv(t)
	if _, code := captureStdout(t, func() int { return run([]string{"askdocs", "ingest", root}) }); code != 0 {
		t.Fatalf("ingest failed")
	}

	const port = "47622"
	done := make(chan int, 1)
	go func() {
		_, code := captureStdout(t, func() int { return run([]string{"askdocs", "web", "-port", port}) })
		done <- code
	}()

	up := false
	for range 100 {
		resp, err := http.Get("http://127.0.0.1:" + port + "/")
		if err == nil {
			resp.Body.Close()
			up = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatal("cmdWeb never became reachable")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cmdWeb exit = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdWeb did not shut down within 5s of SIGINT")
	}
}

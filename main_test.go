package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunDispatcher(t *testing.T) {
	t.Setenv("ASKDOCS_DB", filepath.Join(t.TempDir(), "missing.db"))
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no args", []string{"askdocs"}, 2},
		{"help", []string{"askdocs", "help"}, 0},
		{"unknown", []string{"askdocs", "bogus"}, 2},
		{"status missing corpus", []string{"askdocs", "status"}, 1},
		{"search missing corpus", []string{"askdocs", "search", "x"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(tt.args); got != tt.want {
				t.Errorf("run(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestDBPathPrecedence(t *testing.T) {
	t.Setenv("ASKDOCS_DB", "/env/place.db")
	if got := dbPath("/flag/place.db"); got != "/flag/place.db" {
		t.Errorf("flag should win: %q", got)
	}
	if got := dbPath(""); got != "/env/place.db" {
		t.Errorf("env fallback: %q", got)
	}
	t.Setenv("ASKDOCS_DB", "")
	if got := dbPath(""); got != defaultDBName {
		t.Errorf("default: %q", got)
	}
}

func TestCmdIngestRejectsNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdIngest([]string{file}); err == nil {
		t.Errorf("cmdIngest on a file succeeded, want error")
	}
}

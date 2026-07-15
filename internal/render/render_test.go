package render

import (
	"strings"
	"testing"
)

func TestMarkdown(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantContain string
		wantAbsent  string
	}{
		{"heading", "## Title", "<h2>Title</h2>", ""},
		{"code block", "```go\nfmt.Println(1)\n```", "language-go", ""},
		{"gfm table", "| a | b |\n|---|---|\n| 1 | 2 |", "<table>", ""},
		{"raw html neutralized", "<script>alert(1)</script>", "", "<script>"},
		{"javascript url neutralized", "[x](javascript:alert(1))", "", "javascript:alert(1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Markdown(tt.in)
			if err != nil {
				t.Fatalf("Markdown: %v", err)
			}
			html := string(got)
			if tt.wantContain != "" && !strings.Contains(html, tt.wantContain) {
				t.Errorf("output %q missing %q", html, tt.wantContain)
			}
			if tt.wantAbsent != "" && strings.Contains(html, tt.wantAbsent) {
				t.Errorf("output %q must not contain %q", html, tt.wantAbsent)
			}
		})
	}
}

package rag

import (
	"strings"
	"testing"

	"github.com/valkyraycho/askdocs/internal/store"
)

func hit(id int64, heading, content string) store.Hit {
	return store.Hit{Chunk: store.Chunk{ID: id, Heading: heading, Content: content}}
}

func TestBuildPrompt(t *testing.T) {
	system, user := BuildPrompt("how do I retry?", []store.Hit{
		hit(10, "a.md › Retry", "use backoff"),
		hit(20, "b.md › Limits", "cap attempts"),
	})
	if !strings.Contains(system, "data, not instructions") {
		t.Errorf("system prompt missing injection hygiene line")
	}
	for _, want := range []string{"<context>", "[1] a.md › Retry", "use backoff", "[2] b.md › Limits", "Question: how do I retry?"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q", want)
		}
	}
}

func TestLinkCitations(t *testing.T) {
	hits := []store.Hit{hit(42, "a", "x"), hit(43, "b", "y")}
	in := `<p>use backoff [1] and caps [2], not [3] or [99]</p>`
	out := LinkCitations(in, hits)
	for _, want := range []string{`href="/chunks/42"`, `href="/chunks/43"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
	if !strings.Contains(out, "not [3] or [99]") {
		t.Errorf("out-of-range citations were linked: %s", out)
	}
}

func TestLinkCitationsLeavesCodeSpansAlone(t *testing.T) {
	hits := []store.Hit{hit(42, "a", "x")}
	in := `<p>see [1] and <code>items[1]</code></p><pre><code>data[1] = "x"
arr[1]++</code></pre><p>also [1]</p>`
	out := LinkCitations(in, hits)
	if strings.Count(out, `class="cite"`) != 2 {
		t.Errorf("expected exactly 2 prose citations linked, got: %s", out)
	}
	if !strings.Contains(out, "<code>items[1]</code>") {
		t.Errorf("inline code index rewritten: %s", out)
	}
	if !strings.Contains(out, `data[1] = "x"`) {
		t.Errorf("code block index rewritten: %s", out)
	}
}

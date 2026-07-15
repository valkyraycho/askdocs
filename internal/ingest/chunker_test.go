package ingest

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkMarkdownHeadingBreadcrumbs(t *testing.T) {
	doc := `intro before any heading

# Guide

top level text

## Setup

setup text

### Docker

docker text

## Usage

usage text
`
	sections := ChunkFile("docs/guide.md", []byte(doc))

	want := []struct{ heading, contains string }{
		{"docs/guide.md", "intro before any heading"},
		{"docs/guide.md › Guide", "top level text"},
		{"docs/guide.md › Guide › Setup", "setup text"},
		{"docs/guide.md › Guide › Setup › Docker", "docker text"},
		{"docs/guide.md › Guide › Usage", "usage text"},
	}
	if len(sections) != len(want) {
		t.Fatalf("got %d sections, want %d: %+v", len(sections), len(want), headings(sections))
	}
	for i, w := range want {
		if sections[i].Heading != w.heading {
			t.Errorf("section %d heading = %q, want %q", i, sections[i].Heading, w.heading)
		}
		if !strings.Contains(sections[i].Content, w.contains) {
			t.Errorf("section %d content %q missing %q", i, sections[i].Content, w.contains)
		}
	}
}

func TestChunkHeadingStackPopsOnSameLevel(t *testing.T) {
	doc := "## A\n\na text\n\n## B\n\nb text\n"
	sections := ChunkFile("f.md", []byte(doc))
	if len(sections) != 2 {
		t.Fatalf("sections = %v", headings(sections))
	}
	if sections[1].Heading != "f.md › B" {
		t.Errorf("second heading = %q, want f.md › B (A must be popped)", sections[1].Heading)
	}
}

func TestChunkIgnoresHeadingsInsideCodeFences(t *testing.T) {
	doc := "# Real\n\ntext\n\n```sh\n# not a heading, a comment\necho hi\n```\n\nmore text\n"
	sections := ChunkFile("f.md", []byte(doc))
	if len(sections) != 1 {
		t.Fatalf("fenced # treated as heading: %v", headings(sections))
	}
	if !strings.Contains(sections[0].Content, "# not a heading") {
		t.Errorf("fence content lost: %q", sections[0].Content)
	}
}

func TestChunkOversizeSectionSplitsOnParagraphs(t *testing.T) {
	para := strings.Repeat("word ", 150) // ~750 chars
	doc := "# Big\n\n" + para + "\n\n" + para + "\n\n" + para + "\n\n" + para + "\n"
	sections := ChunkFile("f.md", []byte(doc))

	if len(sections) < 2 {
		t.Fatalf("oversize section not split: %d sections", len(sections))
	}
	for i, s := range sections {
		if len(s.Content) > maxChunkChars {
			t.Errorf("section %d is %d chars, over budget %d", i, len(s.Content), maxChunkChars)
		}
		if s.Heading != "f.md › Big" {
			t.Errorf("split section %d lost breadcrumb: %q", i, s.Heading)
		}
	}
}

func TestChunkHardSplitsGiantParagraphRuneSafely(t *testing.T) {
	giant := strings.Repeat("日本語テキスト", 600) // ~3600 runes, multibyte
	sections := ChunkFile("f.md", []byte("# H\n\n"+giant+"\n"))
	if len(sections) < 2 {
		t.Fatalf("giant paragraph not split: %d sections", len(sections))
	}
	for i, s := range sections {
		if !utf8.ValidString(s.Content) {
			t.Errorf("section %d contains invalid UTF-8 (split mid-rune)", i)
		}
		if runeLen := utf8.RuneCountInString(s.Content); runeLen > maxChunkChars {
			t.Errorf("section %d is %d runes", i, runeLen)
		}
	}
}

func TestChunkTxtFallback(t *testing.T) {
	doc := "first paragraph\n\nsecond paragraph\n"
	sections := ChunkFile("notes.txt", []byte(doc))
	if len(sections) != 1 {
		t.Fatalf("sections = %d", len(sections))
	}
	if sections[0].Heading != "notes.txt" {
		t.Errorf("heading = %q", sections[0].Heading)
	}
	if !strings.Contains(sections[0].Content, "second paragraph") {
		t.Errorf("content = %q", sections[0].Content)
	}
}

func TestChunkEmptyAndBlankFiles(t *testing.T) {
	for _, data := range []string{"", "   \n\n  \n"} {
		if sections := ChunkFile("f.md", []byte(data)); len(sections) != 0 {
			t.Errorf("blank file produced %d sections", len(sections))
		}
	}
}

func headings(sections []Section) []string {
	out := make([]string, len(sections))
	for i, s := range sections {
		out[i] = fmt.Sprintf("%q(%d)", s.Heading, len(s.Content))
	}
	return out
}

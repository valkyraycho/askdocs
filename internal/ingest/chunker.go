package ingest

import (
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxChunkChars = 2000
	maxOverlap    = 500
)

type Section struct {
	Heading string
	Content string
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)

func ChunkFile(relPath string, data []byte) []Section {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".md", ".markdown":
		return chunkMarkdown(relPath, string(data))
	default:
		return pack(relPath, string(data))
	}
}

func chunkMarkdown(relPath, text string) []Section {
	type level struct {
		depth int
		title string
	}
	var (
		sections []Section
		stack    []level
		current  []string
		inFence  bool
	)
	breadcrumb := func() string {
		parts := []string{relPath}
		for _, l := range stack {
			parts = append(parts, l.title)
		}
		return strings.Join(parts, " › ")
	}
	flush := func() {
		sections = append(sections, pack(breadcrumb(), strings.Join(current, "\n"))...)
		current = current[:0]
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			current = append(current, line)
			continue
		}
		if !inFence {
			if m := headingRe.FindStringSubmatch(line); m != nil {
				flush()
				depth := len(m[1])
				for len(stack) > 0 && stack[len(stack)-1].depth >= depth {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, level{depth: depth, title: m[2]})
				continue
			}
		}
		current = append(current, line)
	}
	flush()
	return sections
}

// pack splits content into sections within the chunk budget: greedy paragraph
// packing with a capped one-paragraph overlap; paragraphs beyond the budget
// are hard-split rune-safely.
func pack(heading, content string) []Section {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if len([]rune(content)) <= maxChunkChars {
		return []Section{{Heading: heading, Content: content}}
	}

	var paragraphs []string
	for _, p := range regexp.MustCompile(`\n\s*\n`).Split(content, -1) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		paragraphs = append(paragraphs, hardSplit(p)...)
	}

	var sections []Section
	var cur strings.Builder
	var lastParagraph string
	emit := func() {
		if cur.Len() > 0 {
			sections = append(sections, Section{Heading: heading, Content: cur.String()})
			cur.Reset()
		}
	}
	for _, p := range paragraphs {
		if cur.Len() > 0 && utf8Len(cur.String())+utf8Len(p)+2 > maxChunkChars {
			emit()
			overlap := tailRunes(lastParagraph, maxOverlap)
			if overlap != "" && utf8Len(overlap)+utf8Len(p)+2 <= maxChunkChars {
				cur.WriteString(overlap)
				cur.WriteString("\n\n")
			}
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(p)
		lastParagraph = p
	}
	emit()
	return sections
}

func hardSplit(paragraph string) []string {
	runes := []rune(paragraph)
	if len(runes) <= maxChunkChars {
		return []string{paragraph}
	}
	var parts []string
	for start := 0; start < len(runes); start += maxChunkChars {
		end := start + maxChunkChars
		if end > len(runes) {
			end = len(runes)
		}
		parts = append(parts, string(runes[start:end]))
	}
	return parts
}

func utf8Len(s string) int {
	return len([]rune(s))
}

func tailRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

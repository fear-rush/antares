package gateway

import (
	"strings"
	"testing"
)

func TestRenderTableToMonospace(t *testing.T) {
	in := "| Name | Qty |\n|---|---|\n| T-Shirt | 2 |\n| Jeans | 1 |"
	out := renderTelegram(in)
	if !strings.Contains(out, "<pre>") || !strings.Contains(out, "T-Shirt") {
		t.Fatalf("got %q", out)
	}
	if strings.Contains(out, "| Name |") {
		t.Fatalf("raw pipes leaked: %q", out)
	}
}

func TestRenderInlineStyles(t *testing.T) {
	out := renderTelegram("**bold** and `code` and [link](https://x.test/)")
	if !strings.Contains(out, "<b>bold</b>") || !strings.Contains(out, "<code>code</code>") {
		t.Fatalf("got %q", out)
	}
	if !strings.Contains(out, `<a href="https://x.test/">link</a>`) {
		t.Fatalf("got %q", out)
	}
}

func TestRenderFencePreserved(t *testing.T) {
	out := renderTelegram("before\n```go\nfmt.Println(\"**x**\")\n```\nafter")
	if !strings.Contains(out, "<pre><code") || !strings.Contains(out, "after") {
		t.Fatalf("got %q", out)
	}
	if strings.Contains(out, "<b>x</b>") {
		t.Fatalf("fence content styled: %q", out)
	}
}

func TestRenderHeadingQuoteList(t *testing.T) {
	out := renderTelegram("## Title\n> quoted\n- item")
	if !strings.Contains(out, "<b>Title</b>") || !strings.Contains(out, "<blockquote>") || !strings.Contains(out, "• item") {
		t.Fatalf("got %q", out)
	}
}

func TestUseRichMessage(t *testing.T) {
	if !useRichMessage("| A | B |\n|---|---|\n| 1 | 2 |") {
		t.Fatal("table not detected")
	}
	if useRichMessage("plain **bold** text") {
		t.Fatal("false positive")
	}
}

func TestRichMarkdownPayload(t *testing.T) {
	p := richMarkdownPayload("intro\nline two\n\n| A | B |\n|---|---|\n| 1 | 2 |")
	md, ok := p["markdown"].(string)
	if !ok {
		t.Fatalf("got %#v", p)
	}
	if !strings.Contains(md, "intro  \nline two") || !strings.Contains(md, "| A | B |") {
		t.Fatalf("got %q", md)
	}
}

func TestSplitMarkdownSafeKeepsTable(t *testing.T) {
	table := "| A | B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |"
	chunks := splitMarkdownSafe("intro\n\n"+table+"\n\noutro", 40)
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "| 3 | 4 |") {
		t.Fatalf("table split: %q", chunks)
	}
	for _, c := range chunks {
		if len(c) > 80 {
			t.Fatalf("chunk too long: %q", c)
		}
	}
}

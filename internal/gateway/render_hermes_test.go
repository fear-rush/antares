package gateway

import (
	"strings"
	"testing"
)

func TestNeedsRichRendering(t *testing.T) {
	if !needsRichRendering("| A | B |\n|---|---|\n| 1 | 2 |") {
		t.Fatal("table not detected")
	}
	if !needsRichRendering("- [ ] task") {
		t.Fatal("task list not detected")
	}
	if !needsRichRendering("<details>\n<summary>x</summary>\n</details>") {
		t.Fatal("details not detected")
	}
	if !needsRichRendering("math $$x^2$$ here") {
		t.Fatal("block math not detected")
	}
	if needsRichRendering("plain **bold** reply") {
		t.Fatal("plain reply flagged rich")
	}
}

func TestRichSkipDelivery(t *testing.T) {
	if !richSkipDelivery("<details>math $$x$$</details>") {
		t.Fatal("details+math not skipped")
	}
	if !richSkipDelivery("halo \u4e2d\u6587 dunia") {
		t.Fatal("CJK not skipped")
	}
	if richSkipDelivery("plain table | A |\n|---|\n| 1 |") {
		t.Fatal("plain table skipped")
	}
}

func TestRichMarkdownPayloadKeepsRaw(t *testing.T) {
	in := "line one\nline two\n\n| A |\n|---|\n| 1 |"
	p := richMarkdownPayload(in)
	md, ok := p["markdown"].(string)
	if !ok {
		t.Fatalf("got %#v", p)
	}
	if !strings.Contains(md, "line one  \nline two") {
		t.Fatalf("hard break missing: %q", md)
	}
	if !strings.Contains(md, "| A |") {
		t.Fatalf("table mangled: %q", md)
	}
}

func TestRichNormalizeFenceVerbatim(t *testing.T) {
	in := "```go\nline one\nline two\n```"
	if got := richNormalizeLinebreaks(in); got != in {
		t.Fatalf("fence rewritten: %q", got)
	}
}

func TestRichSentRecordLookup(t *testing.T) {
	richSentRecord("c1", "m1", "hello rich")
	if got := richSentLookup("c1", "m1"); got != "hello rich" {
		t.Fatalf("got %q", got)
	}
	if got := richSentLookup("c1", "nope"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayWidthCJK(t *testing.T) {
	if displayWidth("AB") != 2 {
		t.Fatalf("got %d", displayWidth("AB"))
	}
	if displayWidth("\u4e2d\u6587") != 4 {
		t.Fatalf("got %d", displayWidth("\u4e2d\u6587"))
	}
	if displayWidth("") != 0 {
		t.Fatalf("got %d", displayWidth(""))
	}
}

func TestInlineHTMLKeepsMultibyte(t *testing.T) {
	in := "task_1 done — researcher · Riset na…"
	out := inlineHTML(in)
	if !strings.Contains(out, "done — researcher · Riset na…") {
		t.Fatalf("multibyte mangled: %q", out)
	}
}

func TestSplitMarkdownSafeRuneAligned(t *testing.T) {
	in := "done — researcher · na… kip"
	chunks := splitMarkdownSafe(in, 10)
	for _, c := range chunks {
		for _, r := range c {
			if r == '�' {
				t.Fatalf("replacement char in chunk %q", c)
			}
		}
		if len([]rune(c)) > 20 {
			t.Fatalf("chunk too long: %q", c)
		}
	}
}

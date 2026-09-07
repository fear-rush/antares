package tools

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestBingRSSParsesItems(t *testing.T) {
	// Shape check on the parser half: feed a minimal RSS doc through the
	// same struct tags bingRSSSearch uses.
	body := []byte(`<?xml version="1.0"?><rss version="2.0"><channel>` +
		`<item><title>Doe Bank Chief</title><link>https://x.test/doe</link>` +
		`<description>Chief at bank</description></item></channel></rss>`)
	type item struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
		Desc  string `xml:"description"`
	}
	var rss struct {
		Items []item `xml:"channel>item"`
	}
	if err := xml.Unmarshal(body, &rss); err != nil {
		t.Fatal(err)
	}
	if len(rss.Items) != 1 || rss.Items[0].Link != "https://x.test/doe" {
		t.Fatalf("got %+v", rss)
	}
}

func TestUddgURLUnwraps(t *testing.T) {
	got := uddgURL(`href="/l/?uddg=https%3A%2F%2Fx.test%2Fp&amp;rut=x"`)
	if got != "https://x.test/p" {
		t.Fatalf("got %q", got)
	}
}

func TestAttrValuePullsHref(t *testing.T) {
	got := attrValue(`<a href="https://x.test/" class="x">`, "href")
	if got != "https://x.test/" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(tagText(`<a href="u">Title here</a>`, "a"), "Title here") {
		t.Fatal("tagText failed")
	}
}

package gateway

import (
	"strings"
	"testing"
)

func TestFileMethodPicksInlineKinds(t *testing.T) {
	cases := map[string]string{
		"a.jpg":  "sendPhoto",
		"b.jpeg": "sendPhoto",
		"c.png":  "sendPhoto",
		"d.webp": "sendPhoto",
		"e.gif":  "sendAnimation",
		"f.mp4":  "sendVideo",
		"g.mov":  "sendVideo",
		"h.mp3":  "sendAudio",
		"i.ogg":  "sendAudio",
		"j.pdf":  "sendDocument",
		"k.zip":  "sendDocument",
		"l":      "sendDocument",
	}
	for path, want := range cases {
		if got, _, _ := fileMethod(path); got != want {
			t.Fatalf("%s: got %s, want %s", path, got, want)
		}
	}
}

func TestTruncateCaptionRuneAligned(t *testing.T) {
	long := strings.Repeat("é", telegramCaptionMax+100)
	got := string(truncateCaption([]rune(long)))
	if len([]rune(got)) != telegramCaptionMax {
		t.Fatalf("caption len = %d, want %d", len([]rune(got)), telegramCaptionMax)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("replacement char in caption %q", got)
		}
	}
}

func TestFileFallbackText(t *testing.T) {
	got := fileFallbackText(Reply{FilePath: "/w/report.pdf", Caption: "hasil"})
	if !strings.Contains(got, "hasil") || !strings.Contains(got, "/w/report.pdf") {
		t.Fatalf("fallback must carry caption and path: %q", got)
	}
}

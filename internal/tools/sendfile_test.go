package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendFileQueuesExistingFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(fp, []byte("pdf-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := sendFileTool{}.Execute(context.Background(), Input{
		Args:      []byte(`{"path":"report.pdf","caption":"hasil"}`),
		Workspace: dir,
		Emit:      func(Progress) {},
	})
	if res.IsError {
		t.Fatalf("send_file should queue: %s", res.Content)
	}
	got, _ := res.Meta["send_file"].(string)
	if got != fp {
		t.Fatalf("meta send_file = %q, want %q", got, fp)
	}
	if c, _ := res.Meta["caption"].(string); c != "hasil" {
		t.Fatalf("meta caption = %q", c)
	}
}

func TestSendFileRejectsMissing(t *testing.T) {
	res := sendFileTool{}.Execute(context.Background(), Input{
		Args:      []byte(`{"path":"nope.pdf"}`),
		Workspace: t.TempDir(),
		Emit:      func(Progress) {},
	})
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Fatalf("missing file must error: %+v", res)
	}
}

func TestSendFileRejectsDir(t *testing.T) {
	dir := t.TempDir()
	res := sendFileTool{}.Execute(context.Background(), Input{
		Args:      []byte(`{"path":"."}`),
		Workspace: dir,
		Emit:      func(Progress) {},
	})
	if !res.IsError {
		t.Fatalf("directory must error: %+v", res)
	}
}

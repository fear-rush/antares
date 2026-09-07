package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewWorkspaceFilesFindsArrivals(t *testing.T) {
	dir := t.TempDir()
	before := listDirFiles(dir)
	for _, n := range []string{"a.pdf", "b.png", "c.mp4", "d.zip"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Non-deliverable kinds stay out even when new.
	if err := os.WriteFile(filepath.Join(dir, "e.go"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := newWorkspaceFiles(dir, before)
	if len(got) != 4 {
		t.Fatalf("want 4 arrivals, got %v", got)
	}
}

func TestNewWorkspaceFilesIgnoresPreExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.pdf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := listDirFiles(dir)
	if len(before) != 1 {
		t.Fatalf("snapshot want 1, got %v", before)
	}
	got := newWorkspaceFiles(dir, before)
	if len(got) != 0 {
		t.Fatalf("pre-existing must not resend: %v", got)
	}
}

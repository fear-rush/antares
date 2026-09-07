package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// sendAutoExts are the workspace artifacts worth pushing to the person
// without being asked twice: reports, documents, images, audio, video,
// archives. Source files, logs, and temp output stay out — the model uses
// send_file explicitly when one of those is the point.
var sendAutoExts = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true, ".csv": true, ".txt": true, ".md": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".mp4": true, ".mov": true, ".mp3": true, ".ogg": true, ".oga": true,
	".wav": true, ".weba": true, ".zip": true, ".tar": true, ".gz": true,
}

// sendAutoMaxBytes caps one auto-sent file; larger artifacts stay available
// via send_file explicitly.
const sendAutoMaxBytes = 50 << 20

// listDirFiles snapshots the deliverable files directly inside dir.
func listDirFiles(dir string) map[string]int64 {
	out := map[string]int64{}
	if strings.TrimSpace(dir) == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !sendAutoExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		if info, err := e.Info(); err == nil {
			out[filepath.Join(dir, e.Name())] = info.Size()
		}
	}
	return out
}

// newWorkspaceFiles reports deliverable files created (or grown) since the
// snapshot, newest first by mtime. Edits to pre-existing files do not count:
// only arrivals.
func newWorkspaceFiles(dir string, before map[string]int64) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type hit struct {
		path  string
		mtime int64
	}
	var hits []hit
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !sendAutoExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > sendAutoMaxBytes {
			continue
		}
		fp := filepath.Join(dir, e.Name())
		if _, ok := before[fp]; ok {
			continue
		}
		hits = append(hits, hit{fp, info.ModTime().UnixNano()})
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].mtime > hits[j-1].mtime; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.path)
	}
	return out
}

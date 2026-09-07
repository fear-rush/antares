package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sendFileMaxBytes caps one send_file call so a runaway artifact cannot blow
// the gateway upload budget. Telegram's Bot API allows 50MB per file; other
// surfaces fall back to naming the path in text.
const sendFileMaxBytes = 50 << 20

// sendFileTool hands a workspace file to the person on the current surface:
// photo, video, audio, or document. On Telegram it arrives as a real file
// message; on surfaces without file delivery the reply names the path.
type sendFileTool struct{}

func (sendFileTool) Name() string { return "send_file" }

func (sendFileTool) Description() string {
	return "Send a file to the user on the current chat surface: images, video, " +
		"audio, PDF, or any document. Use this when the user asked for a file, " +
		"or when you produced one they need (a report PDF, a chart, a clip). " +
		"The file must already exist under the workspace. On Telegram it arrives " +
		"as a photo/video/document message; elsewhere the reply names its path."
}


func (sendFileTool) RequiresApproval() bool { return false }

func (sendFileTool) Schema() map[string]any {
	return schema(map[string]any{
		"path":    prop("string", "Workspace-relative or absolute path to an existing file to send."),
		"caption": prop("string", "Optional caption shown with the file."),
	}, "path")
}

func (sendFileTool) Execute(_ context.Context, in Input) Result {
	var args struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}
	if err := in.Bind(&args); err != nil {
		return Errorf("%v", err)
	}
	p := strings.TrimSpace(args.Path)
	if p == "" {
		return Errorf("path is required")
	}
	abs, err := resolveSendPath(in.Workspace, p)
	if err != nil {
		return Errorf("%v", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Errorf("file not found: %s", p)
	}
	if st.IsDir() {
		return Errorf("not a file: %s", p)
	}
	if st.Size() > sendFileMaxBytes {
		return Errorf("file too large (%d bytes, max %d): %s", st.Size(), sendFileMaxBytes, p)
	}
	return Result{
		Content: fmt.Sprintf("Queued %s (%d bytes) for delivery.", filepath.Base(abs), st.Size()),
		Meta: map[string]any{
			"send_file": abs,
			"caption":   strings.TrimSpace(args.Caption),
		},
	}
}

// resolveSendPath resolves a send_file path the same way reads do: absolute
// paths must sit under the workspace (or uploads), relative paths resolve
// against it. Reuses the document resolver so attachments work too.
func resolveSendPath(workspace, p string) (string, error) {
	return resolveDocPath(workspace, p)
}

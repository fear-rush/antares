package main

import (
	"strings"
	"testing"
	"time"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/tools"
)

func mkTask(id, status, tool, detail string, n int) agent.BackgroundTask {
	return agent.BackgroundTask{
		TaskInfo: tools.TaskInfo{
			ID: id, Role: "researcher", Task: "Riset x", Status: status,
			StartedAt: time.Now().Add(-90 * time.Second),
		},
		LastTool: tool, LastDetail: detail, ToolCount: n,
	}
}

func TestRenderTaskProgressLines(t *testing.T) {
	tasks := []agent.BackgroundTask{
		mkTask("task_abc123xyz", "running", "web_search", `{"query":"x bank"}`, 3),
		mkTask("task_def456uvw", "done", "", "", 0),
	}
	got := renderTaskProgress("", tasks)
	if !strings.Contains(got, "Workers:") {
		t.Fatalf("missing header: %q", got)
	}
	if !strings.Contains(got, "web_search") || !strings.Contains(got, "x bank") {
		t.Fatalf("missing tool detail: %q", got)
	}
	if !strings.Contains(got, "(#3)") {
		t.Fatalf("missing call count: %q", got)
	}
}

func TestRenderTaskProgressWaiting(t *testing.T) {
	tk := mkTask("task_wait1", "done", "", "", 5)
	tk.WaitingAsk = "ask_x"
	got := renderTaskProgress("", []agent.BackgroundTask{tk})
	if !strings.Contains(got, "waiting on your answer") {
		t.Fatalf("waiting state missing: %q", got)
	}
}

func TestTaskDetailLineEmpty(t *testing.T) {
	if got := taskDetailLine(mkTask("t", "running", "", "", 0)); got != "starting…" {
		t.Fatalf("want starting marker, got %q", got)
	}
	if got := taskDetailLine(mkTask("t", "done", "", "", 0)); got != "" {
		t.Fatalf("done with no tool should be empty, got %q", got)
	}
}

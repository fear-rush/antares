package server

import (
	"testing"
)

// Queued wake notes keep their task ids through a merge, so a resumed turn
// that parks on ask_user traces back to the task row in /tasks and /agents.
func TestMergeWakesKeepsTaskID(t *testing.T) {
	out := mergeWakes([]queuedWake{
		{note: "first", taskID: "task_a"},
		{note: "second", taskID: "task_b"},
	})
	if out.taskID != "task_a" {
		t.Fatalf("want first task id, got %q", out.taskID)
	}
	if out.note != "first\n\n---\n\nsecond" {
		t.Fatalf("notes must merge in order: %q", out.note)
	}
}



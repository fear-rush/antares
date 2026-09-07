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



func TestMergeWakesKeepsRoute(t *testing.T) {
	out := mergeWakes([]queuedWake{
		{note: "a", taskID: "t1", platform: "telegram", channelID: "c1", userID: "u1"},
		{note: "b", taskID: "t2"},
	})
	if out.platform != "telegram" || out.channelID != "c1" || out.userID != "u1" {
		t.Fatalf("route lost: %+v", out)
	}
}

func TestWakeStepBodyTrims(t *testing.T) {
	if got := wakeStepBody("a\nb\nlast", false); got != "\nlast" {
		t.Fatalf("got %q", got)
	}
	if got := wakeStepBody("boom", true); got != "\nboom" {
		t.Fatalf("got %q", got)
	}
	if wakeStepBody("", false) != "" {
		t.Fatal("empty must stay empty")
	}
}

func TestTruncateRunesAligned(t *testing.T) {
	got := truncateRunes("done — researcher · x", 10)
	for _, r := range got {
		if r == '�' {
			t.Fatalf("replacement char in %q", got)
		}
	}
	if len([]rune(got)) != 10 {
		t.Fatalf("want 10 runes, got %q", got)
	}
}

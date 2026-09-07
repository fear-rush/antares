package commands

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cmdAgents lists the sub-agents and background tasks running right now,
// Hermes /agents style: who is working, on what, and for how long. Scoped to
// the calling gateway session when one is attached, so one chat never sees
// another chat's workers.
func cmdAgents(_ context.Context, d Deps, in Input) (Result, error) {
	if d.Agent == nil {
		return Result{}, errNoAgent
	}
	subs := d.Agent.ActiveAgentsFor(in.SessionID)
	tasks := d.Agent.BackgroundTasksScoped()
	if in.SessionID != "" {
		kept := tasks[:0]
		for _, t := range tasks {
			if t.ParentSession == in.SessionID {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}

	if len(subs) == 0 && len(tasks) == 0 {
		return Result{Output: "No agents running. Delegated work shows up here while it runs."}, nil
	}

	var b strings.Builder
	b.WriteString("**Agents running**\n\n")
	for _, s := range subs {
		fmt.Fprintf(&b, "- `%s` %s — %s · %s\n",
			s.ID, s.Role, firstLine(s.Task), age(time.Since(s.StartedAt)))
	}
	for _, t := range tasks {
		line := t.Status
		if line == "done" && t.WaitingAsk != "" {
			line = "done - waiting on your answer"
		}
		if t.Status == "running" || t.WaitingAsk != "" {
			fmt.Fprintf(&b, "- `%s` %s — %s · %s\n",
				t.ID, line, orDash(t.Role), firstLine(t.Task))
		}
	}
	done := 0
	for _, t := range tasks {
		if t.Status != "running" {
			done++
		}
	}
	if done > 0 {
		fmt.Fprintf(&b, "\n%d finished task(s). Ask `/tasks` for results.\n", done)
	}
	return Result{Output: b.String()}, nil
}

// cmdTasks reports background tasks with their outcome, so a finished worker's
// result is readable without opening the dashboard.
func cmdTasks(_ context.Context, d Deps, in Input) (Result, error) {
	if d.Agent == nil {
		return Result{}, errNoAgent
	}
	id := strings.TrimSpace(in.Args)
	if id != "" {
		t, ok := d.Agent.BackgroundTask(id)
		if !ok {
			return Result{}, fmt.Errorf("no task %q", id)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "**Task `%s`** — %s\n\n", t.ID, t.Status)
		fmt.Fprintf(&b, "- Role: %s\n- Task: %s\n", orDash(t.Role), t.Task)
		if t.WaitingAsk != "" {
			fmt.Fprintf(&b, "- Waiting on: your answer\n")
		}
		if t.Error != "" {
			fmt.Fprintf(&b, "- Error: %s\n", t.Error)
		}
		if out := strings.TrimSpace(t.Output); out != "" {
			fmt.Fprintf(&b, "\n%s\n", truncateLines(out, 40))
		}
		return Result{Output: b.String()}, nil
	}

	tasks := d.Agent.BackgroundTasksScoped()
	if in.SessionID != "" {
		kept := tasks[:0]
		for _, t := range tasks {
			if t.ParentSession == in.SessionID {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}
	if len(tasks) == 0 {
		return Result{Output: "No background tasks. Start one with delegate_task(background=true)."}, nil
	}
	var b strings.Builder
	b.WriteString("**Background tasks**\n\n")
	for _, t := range tasks {
		status := t.Status
		if status == "done" && t.WaitingAsk != "" {
			status = "done - waiting on your answer"
		}
		fmt.Fprintf(&b, "- `%s` %s — %s · %s\n",
			t.ID, status, orDash(t.Role), firstLine(t.Task))
	}
	b.WriteString("\nRead one with `/tasks <id>`.")
	return Result{Output: b.String()}, nil
}

// cmdVerbose toggles live tool-activity lines in the gateway placeholder,
// Hermes /verbose style. It flips display.tool_progress for this process.
func cmdVerbose(_ context.Context, d Deps, in Input) (Result, error) {
	if d.Agent == nil {
		return Result{}, errNoAgent
	}
	cfg := d.config()
	arg := strings.ToLower(strings.TrimSpace(in.Args))
	want := !cfg.Display.ToolProgress
	switch arg {
	case "on", "true", "1":
		want = true
	case "off", "false", "0":
		want = false
	case "":
	default:
		return Result{}, fmt.Errorf("usage: /verbose [on|off]")
	}
	cfg.Display.ToolProgress = want
	if want {
		return Result{Output: "Verbose on. Tool activity streams into the reply while it runs."}, nil
	}
	return Result{Output: "Verbose off. Only the final answer streams."}, nil
}

func age(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func truncateLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > max {
		lines = append(lines[:max], fmt.Sprintf("… (%d more lines)", len(lines)-max))
	}
	return strings.Join(lines, "\n")
}

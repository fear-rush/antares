package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/enowdev/antares/internal/agent"
)

// The wake mechanism replaces the old "main agent polls its sub-agents" loop
// with the reverse: a finished background sub-agent pushes its result back to
// the delegating session, and the main agent is resumed to act on it.
//
// Two cases, both funnelled through one per-session queue of pending results:
//   - The session is idle (no live turn): a new turn is started at once, fed the
//     result — the "wake up" the user asked for.
//   - The session is mid-turn (streaming): the result is queued and drained when
//     the current turn ends, so it becomes the next turn without interrupting or
//     losing the streaming turn's context.
type wakeQueue struct {
	mu      sync.Mutex
	pending map[string][]queuedWake // session -> queued result notes
	running map[string]bool         // session -> a turn is in flight
	state   map[string]*wakeState   // session -> live resume-turn state
}

func newWakeQueue() *wakeQueue {
	return &wakeQueue{pending: map[string][]queuedWake{}, running: map[string]bool{}, state: map[string]*wakeState{}}
}

// formatDone renders a finished sub-agent's outcome as the note that resumes the
// main agent. It is injected as hidden context (not a visible user message), so
// it is phrased as input for the model to act on and keeps enough of the output
// to be actionable.
func formatDone(d agent.BackgroundDone) string {
	who := d.Role
	if who == "" {
		who = "sub-agent"
	}
	head := fmt.Sprintf("[Background sub-agent finished] %s (task %s)", who, d.TaskID)
	if strings.TrimSpace(d.Task) != "" {
		head += "\nTask: " + d.Task
	}
	if strings.TrimSpace(d.Err) != "" {
		return head + "\nResult: FAILED — " + d.Err +
			"\n\nDecide how to proceed given this failure and your current work."
	}
	out := strings.TrimSpace(d.Output)
	if out == "" {
		out = "(the sub-agent produced no final answer)"
	}
	return head + "\nResult:\n" + out +
		"\n\nIncorporate this result into the work. If other sub-agents are still running, keep waiting for them; otherwise continue."
}

// queuedWake is a finished sub-agent's note plus the task it belongs to,
// so the resumed turn can trace an ask_user park back to that task's row.
// Platform/ChannelID route the resume turn to the surface that spawned the
// work, so a Telegram chat's workers resume as Telegram turns (step bubbles
// straight to the chat) instead of silent detached web turns.
type queuedWake struct {
	note      string
	taskID    string
	platform  string
	channelID string
	userID    string
}

// onBackgroundDone is registered on the agent. It turns a finished sub-agent
// into either an immediate wake-up turn or a queued follow-up.
func (s *Server) onBackgroundDone(d agent.BackgroundDone) {
	if d.ParentSession == "" {
		return
	}
	s.enqueueWake(d.ParentSession, queuedWake{
		note: formatDone(d), taskID: d.TaskID,
		platform: d.Platform, channelID: d.ChannelID, userID: d.UserID,
	})
}

func (s *Server) enqueueWake(session string, w queuedWake) {
	s.wake.mu.Lock()
	// If a turn is already running for this session (streaming, or an earlier
	// wake-up still going), queue the note; the running turn drains it on finish.
	if s.wake.running[session] {
		s.wake.pending[session] = append(s.wake.pending[session], w)
		s.wake.mu.Unlock()
		return
	}
	// Also queue if a live run exists that this queue does not know about (a
	// user turn started via handleChat). The hub is the source of truth for
	// "is something streaming right now".
	if s.hub.get(session) != nil {
		s.wake.pending[session] = append(s.wake.pending[session], w)
		s.wake.mu.Unlock()
		return
	}
	s.wake.mu.Unlock()

	// Idle session: wake it with a fresh turn carrying this result.
	s.startWakeTurn(session, w)
}

// startWakeTurn wakes an idle session: it starts a detached turn that resumes
// the main agent on a finished sub-agent's result. The result is fed via
// ContextInject (hidden context), NOT as a user message — so the agent simply
// continues rather than the transcript showing a fake user prompt. Events
// publish into a liveRun so a reattaching client sees the resumed turn, and any
// results that queue up while it runs are folded into one more turn.
func (s *Server) startWakeTurn(session string, w queuedWake) {
	s.wake.mu.Lock()
	if s.wake.running[session] {
		// Someone else is driving; just queue.
		s.wake.pending[session] = append(s.wake.pending[session], w)
		s.wake.mu.Unlock()
		return
	}
	s.wake.running[session] = true
	s.wake.mu.Unlock()

	lr := newLiveRun()
	s.hub.put(session, lr)

	go func() {
		defer func() {
			lr.finish()
			s.hub.remove(session, lr)
			s.wake.mu.Lock()
			s.wake.running[session] = false
			queued := s.wake.pending[session]
			s.wake.pending[session] = nil
			s.wake.mu.Unlock()
			// Drain: fold every queued result into one more turn.
			if len(queued) > 0 {
				s.startWakeTurn(session, mergeWakes(queued))
			}
		}()
		platform := w.platform
		if platform == "" {
			platform = "web"
		}
		req := agent.WithWakeTask(agent.Request{
			SessionID:     session,
			ContextInject: w.note,
			Platform:      platform,
			ChannelID:     w.channelID,
			UserID:        w.userID,
		}, w.taskID, session)
		// A gateway resume turn streams back to its own chat: text deltas
		// and notices go through the adapter (step bubbles for tool calls
		// arrive via the worker listener), so the person watches the
		// coordinator work instead of silence until the final reply.
		// Web resume turns keep publishing to the liveRun only.
		if _, err := s.agent.Run(context.Background(), req, func(e agent.Event) error {
			lr.publish(e)
			if platform != "web" {
				s.deliverWakeEvent(session, w, e)
			}
			return nil
		}); err != nil {
			slog.Debug("wake turn failed", "error", err, "session", session)
		}
	}()
}

// wakeState accumulates one resume turn's visible state: answer text plus
// the arguments of calls still running, so each finished call renders one
// bubble with its own args and outcome.
type wakeState struct {
	text     strings.Builder
	callArgs map[string]string
}

func (s *Server) wakeStateFor(session string) *wakeState {
	s.wake.mu.Lock()
	defer s.wake.mu.Unlock()
	if s.wake.state == nil {
		s.wake.state = map[string]*wakeState{}
	}
	st, ok := s.wake.state[session]
	if !ok {
		st = &wakeState{callArgs: map[string]string{}}
		s.wake.state[session] = st
	}
	return st
}

func (s *Server) clearWakeState(session string) {
	s.wake.mu.Lock()
	delete(s.wake.state, session)
	s.wake.mu.Unlock()
}

// deliverWakeEvent streams one resume-turn event back to the chat that
// spawned the work. Text deltas accumulate into a final reply (flushed when
// the turn ends); tool calls, notices, errors, and files go out as their
// own bubbles right away, so the coordinator's work is visible step by step.
func (s *Server) deliverWakeEvent(session string, w queuedWake, e agent.Event) {
	if s.gateway == nil || w.channelID == "" {
		return
	}
	ctx := context.Background()
	target := w.platform + ":" + w.channelID
	st := s.wakeStateFor(session)
	switch e.Type {
	case agent.EventToolCall:
		s.wake.mu.Lock()
		st.callArgs[e.ID] = e.Arguments
		s.wake.mu.Unlock()
	case agent.EventToolResult:
		s.wake.mu.Lock()
		args := st.callArgs[e.ID]
		delete(st.callArgs, e.ID)
		s.wake.mu.Unlock()
		if isSilentWakeTool(e.Name) {
			return
		}
		title := wakeStepTitle(e.Name, args)
		body := wakeStepBody(e.Content, e.IsError)
		_ = s.gateway.Deliver(ctx, target, title+body)
	case agent.EventNotice:
		if strings.TrimSpace(e.Message) != "" {
			_ = s.gateway.Deliver(ctx, target, "\U0001F4CC "+strings.TrimSpace(e.Message))
		}
	case agent.EventError:
		if strings.TrimSpace(e.Err) != "" {
			_ = s.gateway.Deliver(ctx, target, "\u26A0\uFE0F "+strings.TrimSpace(e.Err))
		}
	case agent.EventText:
		s.wake.mu.Lock()
		st.text.WriteString(e.Delta)
		s.wake.mu.Unlock()
	case agent.EventFile:
		if strings.TrimSpace(e.FilePath) != "" {
			_ = s.gateway.DeliverFile(ctx, w.platform, w.channelID, e.FilePath, e.Caption)
		}
	case agent.EventDone:
		s.wake.mu.Lock()
		final := strings.TrimSpace(st.text.String())
		s.wake.mu.Unlock()
		s.clearWakeState(session)
		if final != "" {
			_ = s.gateway.Deliver(ctx, target, final)
		}
	}
}

// isSilentWakeTool skips bubbles for tools whose own surface already speaks:
// ask_user never parks on gateways, send_file arrives as the file itself.
func isSilentWakeTool(name string) bool {
	return name == "ask_user" || name == "send_file"
}

// wakeStepTitle renders one finished call as "icon name — key argument".
func wakeStepTitle(name, args string) string {
	line := wakeToolLine(name, args)
	if line == "" {
		line = name
	}
	return "\U0001F9F0 " + line
}

// wakeStepBody trims an outcome to its last meaningful line; errors go whole
// (capped) so the blockage reads verbatim.
func wakeStepBody(content string, isError bool) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if isError {
		if len([]rune(content)) > 500 {
			content = string([]rune(content)[:499]) + "…"
		}
		return "\n" + content
	}
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.HasPrefix(ln, "<untrusted") || strings.HasPrefix(ln, "</untrusted") {
			continue
		}
		if len([]rune(ln)) > 300 {
			ln = string([]rune(ln)[:299]) + "…"
		}
		return "\n" + ln
	}
	return ""
}

// wakeToolLine mirrors the gateway's tool line (icon + name + key argument)
// without importing the main package. Keep the two in sync when adding tools.
func wakeToolLine(name, args string) string {
	arg := wakeKeyArg(name, args)
	if arg != "" {
		return wakeIcon(name) + " " + name + " — " + arg
	}
	return wakeIcon(name) + " " + name
}

// wakeKeyArg picks the one argument worth showing for a tool.
func wakeKeyArg(name, args string) string {
	var v map[string]any
	if strings.TrimSpace(args) == "" || args == "{}" {
		return ""
	}
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return ""
	}
	str := func(k string) string {
		if s, ok := v[k].(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	for _, k := range wakeArgKeys(name) {
		if s := str(k); s != "" {
			return truncateRunes(s, 120)
		}
	}
	for _, k := range []string{"command", "cmd", "path", "file", "url", "query", "prompt", "goal", "task", "text", "question", "name", "id"} {
		if s := str(k); s != "" {
			return truncateRunes(s, 120)
		}
	}
	return ""
}

func wakeArgKeys(name string) []string {
	switch name {
	case "terminal", "process":
		return []string{"command", "cmd"}
	case "read_file", "read_document", "write_file", "edit_file", "list_files":
		return []string{"path", "file"}
	case "grep", "glob":
		return []string{"pattern", "path"}
	case "web_search":
		return []string{"query"}
	case "web_fetch", "http_request", "browser":
		return []string{"url"}
	case "delegate_task":
		return []string{"goal", "prompt", "task", "role"}
	case "task":
		return []string{"action", "id"}
	case "skill":
		return []string{"action", "name"}
	case "todo":
		return []string{"action"}
	case "memory":
		return []string{"action", "query", "text"}
	case "schedule":
		return []string{"action", "name"}
	}
	return nil
}

func wakeIcon(name string) string {
	switch name {
	case "terminal", "process":
		return "\U0001F4BB"
	case "read_file", "read_document", "list_files", "glob":
		return "\U0001F4C4"
	case "write_file", "edit_file":
		return "\u270F\uFE0F"
	case "grep":
		return "\U0001F50D"
	case "web_search":
		return "\U0001F50D"
	case "web_fetch", "http_request", "browser":
		return "\U0001F310"
	case "delegate_task", "task":
		return "\U0001F916"
	case "skill":
		return "\U0001F9F0"
	case "todo":
		return "\U0001F4CB"
	case "memory":
		return "\U0001F9E0"
	case "schedule":
		return "\u23F0"
	case "ask_user":
		return "\u2753"
	default:
		return "\U0001F527"
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// mergeWakes folds queued results into one turn. The first task id wins for
// ask tracing; every note still reaches the model in order.
func mergeWakes(queued []queuedWake) queuedWake {
	notes := make([]string, 0, len(queued))
	for _, q := range queued {
		notes = append(notes, q.note)
	}
	out := queuedWake{note: strings.Join(notes, "\n\n---\n\n")}
	for _, q := range queued {
		if q.taskID != "" {
			out.taskID = q.taskID
			break
		}
	}
	for _, q := range queued {
		if q.platform != "" {
			out.platform, out.channelID, out.userID = q.platform, q.channelID, q.userID
			break
		}
	}
	return out
}

// drainAfterTurn is called when a user-driven turn ends. If sub-agent results
// queued up while it streamed, it starts a wake turn to act on them — this is
// the "inject as the next turn" path for the streaming case.
func (s *Server) drainAfterTurn(session string) {
	if session == "" {
		return
	}
	s.wake.mu.Lock()
	queued := s.wake.pending[session]
	s.wake.pending[session] = nil
	running := s.wake.running[session]
	s.wake.mu.Unlock()
	if len(queued) == 0 || running {
		return
	}
	s.startWakeTurn(session, mergeWakes(queued))
}



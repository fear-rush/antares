package agent

import (
	"testing"
)

// Only the web dashboard can park a turn on a human question: its SSE stream
// carries the ask card and POST /api/asks/{id} resolves it. Everywhere else
// the turn must yield the questions into the reply text instead of blocking
// with nobody able to answer.
func TestAsAskWaiter(t *testing.T) {
	a := &Agent{}
	for _, p := range []string{"", "web"} {
		if a.asAskWaiter(Request{Platform: p}) == nil {
			t.Fatalf("platform %q should park on ask", p)
		}
	}
	for _, p := range []string{"telegram", "discord", "cron", "background", "subagent", "tui", "cli", "autopilot"} {
		if a.asAskWaiter(Request{Platform: p}) != nil {
			t.Fatalf("platform %q must not park on ask", p)
		}
	}
}

package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// cmdSettings shows the live server config with secrets redacted. Read-only
// by design: writes stay on /config <path> <value> so a chat typo cannot
// rewrite the file. Secrets come back masked (••••), never plaintext.
func cmdSettings(_ context.Context, d Deps, in Input) (Result, error) {
	cfg := d.config()
	if cfg == nil {
		return Result{}, errNoAgent
	}
	arg := strings.TrimSpace(in.Args)
	if arg != "" {
		v, err := cfg.GetPath(arg)
		if err != nil {
			return Result{}, err
		}
		return Result{Output: fmt.Sprintf("`%s` = `%v`", arg, v)}, nil
	}

	redacted := cfg.Redacted()
	flat := map[string]string{}
	flattenSettings("", redacted, flat)

	// Curated sections first: the knobs people actually check in chat.
	sections := []struct {
		title string
		paths []string
	}{
		{"Model", []string{
			"model.default", "model.provider", "model.auxiliary",
			"model.temperature", "model.reasoning_effort",
		}},
		{"Runtime", []string{
			"agent.workspace", "agent.max_turns", "tools.toolset",
			"streaming.enabled", "display.tool_progress",
			"display.interim_assistant_messages",
		}},
		{"Telegram", []string{
			"gateway.enabled", "gateway.telegram.enabled",
			"gateway.telegram.stream_edits", "gateway.telegram.rich_messages",
			"gateway.telegram.require_pairing",
		}},
	}

	var b strings.Builder
	b.WriteString("**Settings** (secrets redacted)\n")
	for _, s := range sections {
		var lines []string
		for _, p := range s.paths {
			if v, ok := flat[p]; ok {
				lines = append(lines, fmt.Sprintf("- `%s` = `%s`", p, v))
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n**%s**\n\n%s\n", s.title, strings.Join(lines, "\n"))
	}
	b.WriteString("\nRead one with `/settings <path>`, change with `/config <path> <value>`.")
	return Result{Output: b.String()}, nil
}

func flattenSettings(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flattenSettings(p, t[k], out)
		}
	case []any:
		if len(t) == 0 {
			out[prefix] = "[]"
			return
		}
		var parts []string
		for _, el := range t {
			parts = append(parts, fmt.Sprintf("%v", el))
		}
		out[prefix] = strings.Join(parts, ", ")
	default:
		out[prefix] = fmt.Sprintf("%v", v)
	}
}

// cmdRestart reconnects gateway adapters or reloads config from disk.
// /restart gateway [platform] drops and re-establishes the adapter
// connection (picks up token changes without killing the process).
// /restart agent (alias: /restart config) re-reads config.yaml in place.
// A full process restart is systemd's job, not the gateway's: the command
// says so instead of pretending.
func cmdRestart(_ context.Context, d Deps, in Input) (Result, error) {
	arg, rest, _ := strings.Cut(strings.ToLower(strings.TrimSpace(in.Args)), " ")
	rest = strings.TrimSpace(rest)
	switch arg {
	case "", "gateway":
		if d.Gateway == nil {
			return Result{}, fmt.Errorf("gateway controls are not wired in this runtime")
		}
		if rest == "" {
			if err := d.Gateway.Restart(""); err != nil {
				return Result{}, err
			}
			return Result{Output: "Gateway restarting. Adapters reconnect in a few seconds."}, nil
		}
		if err := d.Gateway.Restart(rest); err != nil {
			return Result{}, err
		}
		return Result{Output: fmt.Sprintf("`%s` reconnecting.", rest)}, nil
	case "agent", "config", "reload":
		if d.Reload == nil {
			return Result{}, fmt.Errorf("config reload is not wired in this runtime")
		}
		if err := d.Reload(); err != nil {
			return Result{}, err
		}
		return Result{Output: "Config reloaded from disk."}, nil
	default:
		return Result{}, fmt.Errorf("usage: /restart [gateway [platform]|agent]")
	}
}

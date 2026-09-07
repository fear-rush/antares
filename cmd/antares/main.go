// Command antares runs the Antares agent: HTTP API, dashboard, and CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/commands"
	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/cron"
	"github.com/enowdev/antares/internal/gateway"
	"github.com/enowdev/antares/internal/httpshim"
	"github.com/enowdev/antares/internal/hub"
	"github.com/enowdev/antares/internal/logx"
	"github.com/enowdev/antares/internal/mcp"
	"github.com/enowdev/antares/internal/plugin"
	"github.com/enowdev/antares/internal/providers"
	"github.com/enowdev/antares/internal/rag"
	"github.com/enowdev/antares/internal/roles"
	"github.com/enowdev/antares/internal/server"
	"github.com/enowdev/antares/internal/skillpack"
	"github.com/enowdev/antares/internal/skills"
	"github.com/enowdev/antares/internal/socialbrowser"
	"github.com/enowdev/antares/internal/store"
	"github.com/enowdev/antares/internal/tools"
	"github.com/enowdev/antares/internal/tui"
	"github.com/enowdev/antares/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "antares: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]

	// The curl/wget PATH shims re-invoke this binary. Handle it before any
	// heavier bootstrap so an intercepted request stays fast.
	if len(args) >= 1 && args[0] == "_httpshim" {
		tool := ""
		var rest []string
		if len(args) >= 2 {
			tool, rest = args[1], args[2:]
		}
		os.Exit(httpshim.Run(tool, rest))
	}

	// Bare `antares` opens the TUI when there is a terminal to draw on, and
	// falls back to serving when there is not (systemd, Docker, cron).
	command := "tui"
	if len(args) == 0 && !term.IsTerminal(int(os.Stdin.Fd())) {
		command = "serve"
		args = []string{"--foreground"}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}

	switch command {
	case "serve", "start":
		return cmdServe(args)
	case "stop":
		return cmdStop(args)
	case "status":
		return cmdStatus(args)
	case "_serve_foreground":
		return cmdServeForeground()
	case "tui", "chat-ui":
		return cmdTUI()
	case "chat", "repl", "cli":
		if command == "repl" || command == "cli" {
			args = append([]string{"-i"}, args...)
		}
		return cmdChat(args)
	case "config":
		return cmdConfig(args)
	case "model":
		return cmdModel(args)
	case "provider", "providers":
		return cmdProvider(args)
	case "theme":
		return cmdTheme(args)
	case "setup":
		return cmdSetup(args)
	case "autopilot":
		return cmdAutopilot(args)
	case "auth", "login":
		return cmdAuth(args)
	case "cron":
		return cmdCron(args)
	case "rag":
		return cmdRag(args)
	case "backup":
		return cmdBackup(args)
	case "doctor":
		return cmdDoctor()
	case "version", "--version", "-v":
		fmt.Printf("%s %s (commit %s, built %s)\n", version.Display, version.Version, version.Commit, version.Date)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", command)
	}
}

func printUsage() {
	fmt.Printf(`%s %s — AI agent

Usage:
  antares                  Open the terminal UI (serves the API when headless)
  antares serve            Start the API server in the background
  antares serve --foreground  Run attached to this terminal (debug/systemd)
  antares stop             Stop the background API server
  antares status           Show background server status
  antares tui              Open the terminal UI explicitly
  antares setup            Configure Antares (web or terminal wizard)
  antares chat <message>   Send one message and print the reply
  antares chat -i          A plain line-based conversation (no full-screen UI)
  antares repl             The same thing, spelled differently
  antares model [id]       Show, list, or change the active model
  antares model list       List every configured model
  antares provider         List providers and their connection status
  antares provider add <id> [api-key]   Connect a provider
  antares provider use <id>             Switch to a connected provider
  antares theme [name]     Show or set the colour theme
  antares config get <path>
  antares config set <path> <value>
  antares cron list|add|run|rm   Manage scheduled jobs
  antares autopilot add|list|run Run a queue of tasks unattended
  antares auth copilot           Sign in to GitHub Copilot
  antares rag index <path>       Index files into semantic search
  antares backup           Archive everything; also list, restore, prune
  antares doctor           Check configuration and connectivity
  antares version

Environment:
  ANTARES_HOME             State directory (default ~/.antares)
  ANTARES_CONFIG           Configuration file
  ANTARES_PORT             HTTP port
  ANTARES_MODEL            Model override
  ANTARES_LOG_LEVEL        debug|info|warn|error
`, version.Display, version.Version)
}

func cmdTUI() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := bootstrap(ctx)
	if err != nil {
		return err
	}
	defer rt.close()

	// A first run with nothing configured drops into setup rather than an
	// empty prompt the user cannot do anything with.
	if needsSetup(rt.cfg) {
		if err := runSetupWizard(ctx, rt); err != nil {
			return err
		}
		cfg, err := config.Reload()
		if err != nil {
			return err
		}
		rt.cfg = cfg
		rt.agent.SetConfig(cfg)
	}

	// bootstrap defers workspace creation until setup is done; ensure it exists
	// now (idempotent) before the TUI starts using it.
	if err := os.MkdirAll(rt.cfg.Agent.Workspace, 0o755); err != nil {
		return fmt.Errorf("preparing workspace: %w", err)
	}

	return tui.New(rt.agent, rt.cfg, rt.db).Run(ctx)
}

// runtimeServices bundles everything a running server needs, so a config reload
// can rebuild the pieces that depend on configuration.
type runtimeServices struct {
	mu      sync.Mutex
	cfg     *config.Config
	db      store.Store
	shell   *tools.ShellManager
	agent   *agent.Agent
	skills  *skills.Manager
	cron    *cron.Runner
	gateway *gateway.Manager
	mcp     *mcp.Manager
	social  *socialbrowser.Manager
}

func bootstrap(ctx context.Context) (*runtimeServices, error) {
	if err := config.EnsureHome(); err != nil {
		return nil, fmt.Errorf("preparing %s: %w", config.Home(), err)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := logx.Setup(cfg.Logging.Level, cfg.Logging.File, cfg.Logging.JSON); err != nil {
		return nil, fmt.Errorf("setting up logging: %w", err)
	}
	// Don't create the default workspace before the wizard has run — a fresh
	// install shouldn't leave ~/antares-workspace behind if setup is abandoned.
	if !needsSetup(cfg) {
		if err := os.MkdirAll(cfg.Agent.Workspace, 0o755); err != nil {
			return nil, fmt.Errorf("preparing workspace: %w", err)
		}
	}

	db, err := store.Open(ctx, cfg.Database.Driver, cfg.Database.DSN,
		cfg.Database.MaxConns, cfg.Database.Busy, cfg.Database.WAL)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	shell := tools.NewShellManager(cfg.Terminal)
	// Route terminal curl/wget through the fingerprinted HTTP client, unless
	// switched off. If the shim cannot be written, the terminal keeps the real
	// curl/wget — the http_request tool is still there.
	if cfg.Tools.HTTP.WrapTerminal {
		if dir, err := httpshim.Install(); err != nil {
			slog.Debug("http shim unavailable", "error", err)
		} else {
			shell.EnableHTTPShim(dir, cfg.Tools.HTTP.Preset, cfg.Tools.HTTP.Proxy)
		}
	}
	ragProvider, err := rag.New(cfg, db)
	if err != nil {
		slog.Warn("RAG disabled", "error", err)
		ragProvider = nil
	}

	// A fresh install starts with the bundled skills already in place.
	if len(cfg.Skills.Dirs) > 0 {
		if n, err := hub.Seed(config.Expand(cfg.Skills.Dirs[0])); err != nil {
			slog.Warn("could not write the bundled skills", "error", err)
		} else if n > 0 {
			slog.Info("installed the bundled skills", "count", n)
		}
	}

	// The security skill library ships in the binary and unpacks once into its
	// own directory — searchable on demand, kept out of the prompt catalogue.
	packDir := config.Path("security-skills")
	if n, err := skillpack.Install(packDir); err != nil {
		slog.Warn("could not unpack the security skill library", "error", err)
	} else if n > 0 {
		slog.Info("unpacked the security skill library", "count", n)
	}

	skillDirs := append(append([]string{}, cfg.Skills.Dirs...), "~/.antares/security-skills")
	skillMgr := skills.NewManager(expandAll(skillDirs))
	skillMgr.SetPackDirs([]string{packDir})
	if err := skillMgr.Reload(); err != nil {
		slog.Warn("some skills failed to load", "error", err)
	}

	ag := agent.New(cfg, db, tools.Default(), shell, ragProvider)
	ag.SetSkills(skillMgr)

	if cfg.Plugins.Enabled {
		pluginMgr := plugin.NewManager(expandAll(cfg.Plugins.Dirs))
		if err := pluginMgr.Load(); err != nil {
			slog.Warn("some plugins failed to load", "error", err)
		}
		if n := pluginMgr.Count(); n > 0 {
			slog.Info("plugins loaded", "count", n)
		}
		ag.SetPlugins(pluginMgr)
	}

	roleReg := roles.NewRegistry(expandAll(cfg.Roles.Dirs))
	_ = roleReg.Reload()
	ag.SetRoles(roleReg)

	rt := &runtimeServices{cfg: cfg, db: db, shell: shell, agent: ag, skills: skillMgr}
	rt.social = socialbrowser.New()
	ag.SetSocialBrowser(rt.social)

	// MCP servers are optional; a failing one is recorded, never fatal.
	rt.mcp = mcp.NewManager()
	rt.mcp.Connect(ctx, cfg)
	if names := rt.mcp.Register(tools.Default()); len(names) > 0 {
		slog.Info("mcp tools registered", "count", len(names))
	}

	rt.gateway = gateway.NewManager(cfg, db, rt.handleGatewayMessage)
	rt.cron = cron.New(cron.Options{
		Store:         db,
		Execute:       rt.runCronJob,
		Deliver:       rt.gateway.Deliver,
		Timezone:      cfg.Cron.Timezone,
		MaxConcurrent: cfg.Cron.MaxConcurrent,
		HistoryLimit:  cfg.Cron.HistoryLimit,
	})

	// Sync social media autopilot cron job.
	if cfg.Social.Enabled {
		ap := socialbrowser.NewAutopilot(db, cfg)
		if err := ap.Sync(ctx); err != nil {
			slog.Warn("social autopilot sync failed", "error", err)
		}
	}

	return rt, nil
}

// gatewayProgress appends one short status line to the streamed text without
// flooding the placeholder: only the latest status line is kept, capped at
// 200 chars. Telegram's 1s edit throttle in the adapters stays the backstop.
func gatewayProgress(answer, status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return answer
	}
	if len(status) > 200 {
		status = status[:200] + "…"
	}
	base := strings.TrimSpace(answer)
	if base == "" {
		return status
	}
	lines := strings.Split(base, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if isStatusLine(ln) {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n")) + "\n\n" + status
}

func isStatusLine(ln string) bool {
	for _, p := range []string{"🔧 ", "📌 ", "💻 ", "📄 ", "✏️ ", "🔍 ", "🌐 ", "🤖 ", "🧰 ", "📋 ", "🧠 ", "📚 ", "🖥️ ", "⏰ ", "❓ "} {
		if strings.HasPrefix(ln, p) {
			return true
		}
	}
	return false
}

// toolLine renders one tool call as a readable status line: an icon, the
// tool name, and a short human summary of its arguments. This is what the
// gateway shows while a tool runs, so the user sees what is happening
// without reading raw JSON.
func toolLine(name, args string) string {
	icon := toolIcon(name)
	summary := summarizeArgs(name, args)
	if summary == "" {
		return icon + " " + name
	}
	return icon + " " + name + " — " + summary
}

func toolIcon(name string) string {
	switch name {
	case "terminal", "process":
		return "💻"
	case "read_file", "read_document", "list_files", "glob":
		return "📄"
	case "write_file", "edit_file":
		return "✏️"
	case "grep":
		return "🔍"
	case "web_search":
		return "🔍"
	case "web_fetch", "http_request", "browser", "hackbrowser":
		return "🌐"
	case "delegate_task", "task":
		return "🤖"
	case "skill":
		return "🧰"
	case "todo":
		return "📋"
	case "memory":
		return "🧠"
	case "rag_search", "rag_index", "session_search":
		return "📚"
	case "vps_run", "vps_upload", "vps_download":
		return "🖥️"
	case "schedule", "cronjob":
		return "⏰"
	case "ask_user":
		return "❓"
	case "board", "project_info":
		return "📌"
	default:
		return "🔧"
	}
}

// summarizeArgs picks the one argument worth showing for a tool: the command,
// path, URL, query, or task. Falls back to the first short scalar.
func summarizeArgs(name, raw string) string {
	args := parseArgsObject(raw)
	if len(args) == 0 {
		return ""
	}
	keys := toolArgKeys(name)
	for _, k := range keys {
		if v, ok := args[k]; ok && v != "" {
			return truncateStatus(v)
		}
	}
	for _, k := range []string{"command", "cmd", "path", "file", "url", "query", "prompt", "goal", "task", "text", "question", "name", "id"} {
		if v, ok := args[k]; ok && v != "" {
			return truncateStatus(v)
		}
	}
	for _, v := range args {
		if v != "" {
			return truncateStatus(v)
		}
	}
	return ""
}

func toolArgKeys(name string) []string {
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

func parseArgsObject(raw string) map[string]string {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return out
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return out
	}
	for k, val := range v {
		switch t := val.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				out[k] = strings.TrimSpace(t)
			}
		case float64:
			out[k] = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%v", t), "0"), ".")
		case bool:
			out[k] = fmt.Sprintf("%v", t)
		}
	}
	return out
}

// lastStatus remembers the newest status line per gateway turn so text
// deltas can re-attach it. Keyed by nothing: one turn at a time per chat.
var lastStatus = struct {
	sync.Mutex
	s string
}{}

func setLastStatus(s string) {
	lastStatus.Lock()
	lastStatus.s = s
	lastStatus.Unlock()
}

func getLastStatus() string {
	lastStatus.Lock()
	defer lastStatus.Unlock()
	return lastStatus.s
}

// gatewayProgressLive re-attaches the last status line when fresh answer text
// arrives. Without this, the first text delta after a tool call would wipe
// the status and the user would see text-only until the next tool runs.
func gatewayProgressLive(answer string) string {
	if st := getLastStatus(); st != "" {
		return gatewayProgress(answer, st)
	}
	return answer
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func truncateStatus(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:117] + "…"
	}
	return s
}

// lastLine keeps only the final line of a progress message so multi-line tool
// output never spills into the streaming status.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// handleGatewayMessage runs one platform message through the agent, reusing a
// persistent session per channel so conversations stay continuous.
func (rt *runtimeServices) handleGatewayMessage(ctx context.Context, msg gateway.InboundMessage, partial func(string)) (string, error) {
	key := gateway.GatewaySessionKey(config.Get(), msg)
	sessionID, err := rt.db.GetKV(ctx, key)
	if err != nil {
		sessionID = ""
	}

	// A slash command is answered here rather than being sent to the model, so
	// /status in Telegram means what it means in the terminal.
	if name, args, ok := commands.Parse(msg.Text); ok {
		return rt.runGatewayCommand(ctx, key, sessionID, name, args)
	}

	// Per-channel routing. Bindings gate GROUP/server channels: when a platform
	// has any binding, it answers only in bound group channels and ignores
	// unregistered ones (strict allowlist). Direct messages (1:1) are always
	// allowed — the allowlist never locks you out of your own DM with the bot.
	// A binding may still match a DM to give it a role/model, but its absence
	// does not silence a DM.
	bindings := config.Get().Gateway.Bindings
	binding := gateway.ResolveBinding(bindings, msg.Platform, msg.GuildID, msg.ChannelID)
	if binding == nil {
		if !msg.IsDirect && gateway.HasBindings(bindings, msg.Platform) {
			return "", nil // unregistered group channel — stay silent
		}
		// A direct message, or a platform with no bindings at all: fall through
		// to the default agent.
	} else {
		// Access within a matched binding. allowed_users and allowed_roles are
		// OR'd, and an explicit user match always wins. When both lists are
		// empty the binding places no per-sender restriction (anyone who reaches
		// the bound channel is served). DMs are never role-gated.
		if !gateway.BindingAdmits(binding, msg.IsDirect, msg.UserID, msg.Roles) {
			return "", nil
		}
		// Relevance gate: a cheap model call decides whether the message fits
		// the channel's criteria before the full agent runs.
		if strings.TrimSpace(binding.RelevanceFilter) != "" {
			if ok := rt.messageIsRelevant(ctx, binding, msg.Text); !ok {
				return "", nil
			}
		}
	}

	var reply strings.Builder
	var pendingFiles []agent.Event
	req := agent.Request{
		SessionID:       sessionID,
		Message:         msg.Text,
		Platform:        msg.Platform,
		UserID:          msg.UserID,
		UserName:        msg.UserName,
		UserDisplayName: msg.DisplayName,
		ChannelID:       msg.ChannelID,
	}
	if binding != nil {
		req.Role = binding.Role
		req.Model = binding.Model
		req.Toolset = binding.Toolset
		req.SystemExtra = strings.TrimSpace(binding.PromptPrefix)
	}
	// Live background-task progress: while workers run, refresh the
	// placeholder with one status line per task (Opsi A). Ticks every 2s,
	// stops when the turn ends. The turn's own tool lines keep flowing
	// through partial() as before; this only adds the worker table.
	// replyMu guards the answer text: Run appends on the turn goroutine
	// while the ticker reads it.
	var replyMu sync.Mutex
	// safePartial forwards to the adapter; callers must hold replyMu.
	// (The emit func below holds it across the whole switch; the ticker
	// takes it only to snapshot the answer text.)
	safePartial := partial
	progressDone := make(chan struct{})
	var progressWg sync.WaitGroup
	if safePartial != nil {
		progressWg.Add(1)
		go rt.streamTaskProgress(progressDone, &progressWg, sessionID, safePartial, &reply, &replyMu)
	}
	step := gateway.StepFunc(ctx)
	pendingCalls := map[string]string{}
	var pendingSteps []gateway.Step
	// Worker steps: each finished background-tool call becomes a bubble
	// tagged with its task, so parallel workers narrate themselves instead
	// of going quiet behind the placeholder table.
	if step != nil {
		mySession := sessionID
		detachSteps := agent.SetProgressListener(func(taskID, tool, args, content string, isError bool) {
			if isSilentStepTool(tool) {
				return
			}
			// Only narrate workers spawned by this chat, not another
			// session's. ParentSession is checked via the task table.
			if !rt.taskBelongsTo(taskID, mySession, sessionID) {
				return
			}
			short := taskID
			if len(short) > 12 {
				short = short[:12]
			}
			// No turn lock here: this runs on the worker's goroutine and
			// touches no shared turn state. step() does network IO; holding
			// replyMu across it would stall the turn.
			step(gateway.Step{
				Title: "[" + short + "] " + stepTitle(tool, args),
				Body:  stepBody(tool, content, isError),
			})
		})
		defer detachSteps()
	}
	res, err := rt.agent.Run(ctx, req, func(e agent.Event) error {
		replyMu.Lock()
		defer replyMu.Unlock()
		switch e.Type {
		case agent.EventSession:
			if e.ID != "" && e.ID != sessionID {
				sessionID = e.ID
				_ = rt.db.SetKV(ctx, key, e.ID)
			}
		case agent.EventText:
			reply.WriteString(e.Delta)
			if safePartial != nil {
				safePartial(gatewayProgressLive(reply.String()))
			}
		case agent.EventTurn:
			if safePartial != nil && e.Turn > 1 {
				safePartial(gatewayProgress(reply.String(), "⏭️ turn "+itoa(e.Turn)))
			}
		case agent.EventToolCall:
			// Surface tool activity on the gateway: a long task that only
			// streams text looks dead while it reads files, runs shell
			// commands, or fans out to sub-agents. Render the call as a
			// readable line (icon plus key argument), not raw JSON.
			pendingCalls[e.ID] = e.Arguments
			if safePartial != nil {
				line := toolLine(e.Name, e.Arguments)
				setLastStatus(line)
				safePartial(gatewayProgress(reply.String(), line))
			}
		case agent.EventToolProgress:
			if safePartial != nil && strings.TrimSpace(e.Message) != "" {
				line := toolIcon(e.Name) + " " + e.Name + ": " + truncateStatus(lastLine(e.Message))
				setLastStatus(line)
				safePartial(gatewayProgress(reply.String(), line))
			}
		case agent.EventToolResult:
			// One finished tool call = one step bubble: title names the
			// tool plus its key argument, body carries the outcome (or
			// the error). delegate_task on the parent turn only announces
			// the spawn — worker detail streams from the worker table.
			// Network IO happens after the switch unlocks (see below)
			// so a slow send never blocks the turn.
			args := pendingCalls[e.ID]
			delete(pendingCalls, e.ID)
			if step != nil && !isSilentStepTool(e.Name) {
				pendingSteps = append(pendingSteps, gateway.Step{
					Title: stepTitle(e.Name, args),
					Body:  stepBody(e.Name, e.Content, e.IsError),
				})
			}
		case agent.EventNotice:
			if safePartial != nil && strings.TrimSpace(e.Message) != "" {
				line := "📌 " + lastLine(e.Message)
				setLastStatus(line)
				safePartial(gatewayProgress(reply.String(), line))
			}
		case agent.EventAsk:
			// A parked ask on a gateway turn means the model is blocked with
			// nobody able to answer — this is the stall that left finished
			// background tasks reading as "(no final answer)" while the real
			// block sat invisible. Surface it loudly so it gets fixed, not
			// waited on.
			slog.Warn("gateway turn parked on ask_user with no live card",
				"platform", msg.Platform, "session", sessionID, "ask", e.ID)
		case agent.EventFile:
			// send_file queued a file: deliver it straight to the chat now,
			// in turn order, instead of waiting for the final text reply.
			if strings.TrimSpace(e.FilePath) != "" {
				pendingFiles = append(pendingFiles, e)
			}
		}
		// Ship step bubbles outside the turn lock: the closure holds
		// replyMu via defer, so copy the queue, release, send, reacquire.
		steps := pendingSteps
		pendingSteps = nil
		if len(steps) > 0 {
			replyMu.Unlock()
			for _, st := range steps {
				if step != nil {
					step(st)
				}
			}
			replyMu.Lock()
		}
		return nil
	})
	close(progressDone)
	progressWg.Wait()
	if err != nil {
		return "", err
	}
	// Flush queued files before the text reply: the person asked for the
	// file, so it lands first, then the summary text follows as usual.
	// Explicit send_file calls and auto-sent workspace arrivals share one
	// queue; dedupe by path so a file sent both ways goes out once.
	seenFile := map[string]bool{}
	for _, fe := range pendingFiles {
		if seenFile[fe.FilePath] {
			continue
		}
		seenFile[fe.FilePath] = true
		if derr := rt.deliverGatewayFile(ctx, msg, fe.FilePath, fe.Caption); derr != nil {
			slog.Warn("gateway file delivery failed",
				"platform", msg.Platform, "session", sessionID, "file", fe.FilePath, "error", derr)
			reply.WriteString("\n\n(File ready but delivery failed: " + fe.FilePath + ": " + derr.Error() + ")")
		}
	}
	if res != nil && res.Reply != "" {
		return res.Reply, nil
	}
	return reply.String(), nil
}

// streamTaskProgress refreshes the gateway placeholder with one status line
// per background task until done closes. Runs on its own goroutine; every
// tick renders the calling session's tasks and pushes them through partial.
// Throttled to one edit per 2s — Telegram rate-limits edits and the adapter
// already drops sub-second repeats.
func (rt *runtimeServices) streamTaskProgress(done <-chan struct{}, wg *sync.WaitGroup, sessionID string, partial func(string), reply *strings.Builder, mu *sync.Mutex) {
	defer wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			tasks := rt.agent.BackgroundTasksScoped()
			var mine []agent.BackgroundTask
			for _, t := range tasks {
				if sessionID == "" || t.ParentSession == sessionID {
					mine = append(mine, t)
				}
			}
			if len(mine) == 0 {
				continue
			}
			mu.Lock()
			body := renderTaskProgress(reply.String(), mine)
			mu.Unlock()
			partial(body)
		}
	}
}

// renderTaskProgress builds the placeholder body: kept answer text plus one
// line per worker — what tool it runs, on what, how many calls so far, and
// how long. Blocked and finished states surface instead of going quiet.
func renderTaskProgress(answer string, tasks []agent.BackgroundTask) string {
	var b strings.Builder
	if strings.TrimSpace(answer) != "" {
		b.WriteString(strings.TrimSpace(answer))
		b.WriteString("\n\n")
	}
	b.WriteString("Workers:\n")
	for _, t := range tasks {
		short := t.ID
		if len(short) > 12 {
			short = short[:12]
		}
		state := t.Status
		detail := taskDetailLine(t)
		elapsed := ""
		if !t.StartedAt.IsZero() {
			elapsed = " · " + ageString(time.Since(t.StartedAt))
		}
		if t.WaitingAsk != "" {
			state = "waiting on your answer"
			detail = ""
		}
		line := "• " + short + " " + state
		if detail != "" {
			line += " — " + detail
		}
		line += elapsed + "\n"
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// taskDetailLine renders what one worker is doing: its current tool plus the
// interesting argument (URL, query, path), and its call count.
func taskDetailLine(t agent.BackgroundTask) string {
	tool := strings.TrimSpace(t.LastTool)
	if tool == "" {
		if t.Status == "running" {
			return "starting…"
		}
		return ""
	}
	detail := summarizeArgs(tool, t.LastDetail)
	line := toolIcon(tool) + " " + tool
	if detail != "" {
		line += " " + detail
	}
	if t.ToolCount > 0 {
		line += " (#" + itoa(t.ToolCount) + ")"
	}
	return line
}

// ageString renders a duration as 12s / 3m / 2h5m for progress lines.
func ageString(d time.Duration) string {
	if d < time.Minute {
		return itoa(int(d.Seconds())) + "s"
	}
	if d < time.Hour {
		return itoa(int(d.Minutes())) + "m"
	}
	return itoa(int(d.Hours())) + "h" + itoa(int(d.Minutes())%60) + "m"
}

// isSilentStepTool skips step bubbles for tools whose progress already
// surfaces elsewhere: ask_user (its own card/flow) and send_file (the file
// message itself is the bubble).
func isSilentStepTool(name string) bool {
	switch name {
	case "ask_user", "send_file":
		return true
	}
	return false
}

// stepTitle renders one finished tool call as a one-line step header:
// icon, tool name, and the key argument (command, path, URL, query).
func stepTitle(name, args string) string {
	line := toolLine(name, args)
	if line == "" {
		line = toolIcon(name) + " " + name
	}
	return line
}

// stepBody trims a tool outcome to a bubble-sized excerpt: errors in full
// (short), successes as the last meaningful line.
func stepBody(name, content string, isError bool) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if isError {
		return truncateStatus(content)
	}
	// Verbose tools (search, fetch, shell) dump pages of output; the bubble
	// keeps the last line so the person sees the outcome, not the dump.
	lines := strings.Split(content, "\n")
	// Skip wrapper fences the agent adds around untrusted output.
	kept := ""
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.HasPrefix(ln, "<untrusted") || strings.HasPrefix(ln, "</untrusted") {
			continue
		}
		kept = ln
		break
	}
	if kept == "" {
		return ""
	}
	return truncateStatus(kept)
}

// taskBelongsTo reports whether a background task was spawned by this
// chat's session. mySession is captured at turn start; liveSession tracks
// the session id if the turn created one mid-flight. Either match counts:
// a gateway chat owns exactly one session at a time.
func (rt *runtimeServices) taskBelongsTo(taskID, mySession, liveSession string) bool {
	t, ok := rt.agent.BackgroundTask(taskID)
	if !ok {
		return false
	}
	if t.ParentSession == "" {
		return true
	}
	return t.ParentSession == mySession || t.ParentSession == liveSession
}

// deliverGatewayFile pushes one send_file artifact to the chat that asked
// for it. Telegram gets a real file message; surfaces without file
// delivery fall back to naming the path in text (handled by the adapter).
func (rt *runtimeServices) deliverGatewayFile(ctx context.Context, msg gateway.InboundMessage, path, caption string) error {
	if rt.gateway == nil {
		return fmt.Errorf("gateway unavailable")
	}
	return rt.gateway.DeliverFile(ctx, msg.Platform, msg.ChannelID, path, caption)
}

// messageIsRelevant runs a cheap, quiet one-shot classification: does the
// message fit the channel binding's relevance criteria? It keeps a busy channel
// quiet — the bot answers on-topic questions and ignores chatter — without
// paying for a full agent turn. On any error it fails OPEN (answers), so a
// classifier hiccup never makes the bot go mute.
func (rt *runtimeServices) messageIsRelevant(ctx context.Context, b *config.Binding, text string) bool {
	prompt := "You are a message filter for a chat bot. Criteria for messages worth answering:\n" +
		strings.TrimSpace(b.RelevanceFilter) +
		"\n\nMessage:\n" + strings.TrimSpace(text) +
		"\n\nDoes this message fit the criteria and deserve a reply? Answer with exactly one word: YES or NO."

	var out strings.Builder
	_, err := rt.agent.Run(ctx, agent.Request{
		Message:         prompt,
		Model:           b.Model, // use the binding's model (or default) for the gate
		Toolset:         "minimal",
		Quiet:           true,
		MaxTurns:        1,
		ReasoningEffort: "low",
	}, func(e agent.Event) error {
		if e.Type == agent.EventText {
			out.WriteString(e.Delta)
		}
		return nil
	})
	if err != nil {
		slog.Warn("relevance gate failed, answering anyway", "error", err)
		return true
	}
	ans := strings.ToUpper(strings.TrimSpace(out.String()))
	// Fail open: only a clear NO suppresses the reply.
	return !strings.HasPrefix(ans, "NO")
}

// runGatewayCommand answers a slash command typed in a chat platform. The few
// commands that only a screen can carry out are translated into something a
// message thread can actually do.
func (rt *runtimeServices) runGatewayCommand(ctx context.Context, kvKey, sessionID, name, args string) (string, error) {
	res, err := commands.Run(ctx, rt.commandDeps(), commands.Input{
		Name:      name,
		Args:      args,
		SessionID: sessionID,
		Surface:   commands.SurfaceGateway,
	})
	if err != nil {
		return err.Error(), nil
	}
	switch res.Action.Kind {
	case "new", "clear":
		// Forgetting the session id is what "start fresh" means here: the next
		// message opens a new one. Name the active model so the user knows
		// what they are talking to after the reset.
		_ = rt.db.DeleteKV(ctx, kvKey)
		rt.mu.Lock()
		model, provider := rt.cfg.Model.Default, rt.cfg.Model.Provider
		rt.mu.Unlock()
		if strings.TrimSpace(model) == "" {
			model = "(unset)"
		}
		return fmt.Sprintf("Started a fresh session. Model `%s` on provider `%s`.", model, provider), nil
	case "stop":
		if sessionID != "" {
			rt.agent.Interrupt(sessionID)
		}
		return "Stopped.", nil
	}
	if res.Output == "" {
		return "Done.", nil
	}
	return res.Output, nil
}

// commandDeps gives the shared command layer the runtime's services.
func (rt *runtimeServices) commandDeps() commands.Deps {
	rt.mu.Lock()
	cfg := rt.cfg
	rt.mu.Unlock()
	return commands.Deps{
		Config:  func() *config.Config { return cfg },
		Agent:   rt.agent,
		Store:   rt.db,
		Skills:  rt.skills,
		MCP:     rt.mcp,
		Reload:  rt.reload,
		Gateway: rt.gateway,
		Version: version.Version,
	}
}

// runCronJob executes a scheduled prompt in its own throwaway session.
func (rt *runtimeServices) runCronJob(ctx context.Context, job store.CronJob) (string, string, error) {
	var reply strings.Builder
	sessionID := ""
	res, err := rt.agent.Run(ctx, agent.Request{
		Message:     job.Prompt,
		Platform:    "cron",
		SystemExtra: "You are running unattended on a schedule named " + job.Name + ". Nobody can answer follow-up questions, so make reasonable assumptions and finish the task.",
	}, func(e agent.Event) error {
		switch e.Type {
		case agent.EventSession:
			sessionID = e.ID
		case agent.EventText:
			reply.WriteString(e.Delta)
		}
		return nil
	})
	if err != nil {
		return sessionID, "", err
	}
	if res != nil && res.Reply != "" {
		return sessionID, res.Reply, nil
	}
	return sessionID, reply.String(), nil
}

// reload re-reads config and rebuilds config-dependent services in place.
func (rt *runtimeServices) reload() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	cfg, err := config.Reload()
	if err != nil {
		return err
	}
	rt.cfg = cfg
	rt.agent.SetConfig(cfg)

	ragProvider, err := rag.New(cfg, rt.db)
	if err != nil {
		slog.Warn("RAG disabled after reload", "error", err)
		ragProvider = nil
	}
	rt.agent.SetRAG(ragProvider)

	packDir := config.Path("security-skills")
	skillDirs := append(append([]string{}, cfg.Skills.Dirs...), "~/.antares/security-skills")
	rt.skills = skills.NewManager(expandAll(skillDirs))
	rt.skills.SetPackDirs([]string{packDir})
	if err := rt.skills.Reload(); err != nil {
		slog.Warn("some skills failed to load", "error", err)
	}
	rt.agent.SetSkills(rt.skills)

	if cfg.Plugins.Enabled {
		pluginMgr := plugin.NewManager(expandAll(cfg.Plugins.Dirs))
		if err := pluginMgr.Load(); err != nil {
			slog.Warn("some plugins failed to load", "error", err)
		}
		rt.agent.SetPlugins(pluginMgr)
	} else {
		rt.agent.SetPlugins(nil)
	}

	roleReg := roles.NewRegistry(expandAll(cfg.Roles.Dirs))
	_ = roleReg.Reload()
	rt.agent.SetRoles(roleReg)

	// The gateway holds its own pointer; without this it would keep reconciling
	// against the configuration it was constructed with.
	if rt.gateway != nil {
		rt.gateway.SetConfig(cfg)
	}

	slog.Info("configuration reloaded", "model", cfg.Model.Default, "provider", cfg.Model.Provider)
	return nil
}

func (rt *runtimeServices) close() {
	if rt.mcp != nil {
		rt.mcp.Close()
	}
	if rt.gateway != nil {
		rt.gateway.StopAll()
	}
	if rt.social != nil {
		rt.social.Close()
	}
	rt.shell.CloseAll()
	tools.CloseBrowsers()
	_ = rt.db.Close()
}

func cmdServeForeground() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := bootstrap(ctx)
	if err != nil {
		return err
	}
	defer rt.close()

	srv := server.New(server.Options{
		Config:  rt.cfg,
		Agent:   rt.agent,
		Store:   rt.db,
		Dist:    server.EmbeddedDist(),
		Reload:  rt.reload,
		Skills:  rt.skills,
		Cron:    rt.cron,
		Gateway: rt.gateway,
		MCP:     rt.mcp,
		Social:  rt.social,
	})

	if rt.cfg.Cron.Enabled {
		go rt.cron.Start(ctx)
	}
	rt.gateway.Start(ctx)

	// Reap idle shells so long-running servers do not leak processes.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rt.shell.ReapIdle(time.Duration(rt.cfg.Terminal.LifetimeSeconds) * time.Second)
			}
		}
	}()

	printBanner(rt.cfg)

	// A VPS with saved SSH credentials + an unauthenticated dashboard means
	// anyone who reaches the port can run commands on the user's servers. Warn
	// loudly rather than fail (an open dashboard behind Tailscale is a valid
	// setup), so the risk is a conscious choice.
	if strings.TrimSpace(rt.cfg.Server.AuthToken) == "" && strings.TrimSpace(rt.cfg.Server.DashboardPasswordHash) == "" {
		if hosts, err := rt.db.ListVPSHosts(ctx); err == nil && len(hosts) > 0 {
			slog.Warn("VPS servers are saved but the dashboard is unauthenticated — anyone who can reach this port can run commands on them. Set server.auth_token or a dashboard password, or bind to loopback/Tailscale only.",
				"vps_hosts", len(hosts), "host", rt.cfg.Server.Host, "port", rt.cfg.Server.Port)
		}
	}

	if err := srv.Serve(ctx); err != nil {
		return err
	}
	slog.Info("antares stopped")
	return nil
}

func printBanner(cfg *config.Config) {
	fmt.Printf("\n  %s %s\n", version.Display, version.Version)
	fmt.Printf("  ├ API      http://%s:%d\n", displayHost(cfg.Server.Host), cfg.Server.Port)
	fmt.Printf("  ├ Model    %s (%s)\n", orDash(cfg.Model.Default), orDash(cfg.Model.Provider))
	fmt.Printf("  ├ Database %s\n", cfg.Database.Driver)
	fmt.Printf("  ├ Workspace %s\n", cfg.Agent.Workspace)
	if cfg.Server.AuthToken == "" {
		fmt.Printf("  └ Auth     open (set server.auth_token to lock it down)\n\n")
	} else {
		fmt.Printf("  └ Auth     token required\n\n")
	}
}

func displayHost(h string) string {
	if h == "" || h == "0.0.0.0" {
		return "localhost"
	}
	return h
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: antares config get|set|path <path> [value]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	switch args[0] {
	case "path":
		fmt.Println(config.ConfigFile())
		return nil
	case "get":
		if len(args) < 2 {
			return errors.New("usage: antares config get <path>")
		}
		v, err := cfg.GetPath(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("%v\n", v)
		return nil
	case "set":
		if len(args) < 3 {
			return errors.New("usage: antares config set <path> <value>")
		}
		if err := cfg.SetPath(args[1], args[2]); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("%s = %s\n", args[1], args[2])
		return nil
	default:
		return fmt.Errorf("unknown config subcommand: %s", args[0])
	}
}

func cmdModel(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Printf("%s (%s)\n", orDash(cfg.Model.Default), orDash(cfg.Model.Provider))
		return nil
	}
	if args[0] == "list" || args[0] == "ls" {
		provIDs := make([]string, 0, len(cfg.Providers))
		for id := range cfg.Providers {
			provIDs = append(provIDs, id)
		}
		sort.Strings(provIDs)
		seen := map[string]bool{}
		print := func(mid, pid string) {
			key := pid + "\x00" + mid
			if mid == "" || seen[key] {
				return
			}
			seen[key] = true
			mark := "  "
			if mid == cfg.Model.Default {
				mark = "❯ "
			}
			fmt.Printf("%s%-40s %s\n", mark, mid, pid)
		}
		for _, pid := range provIDs {
			// static models from config — when set, treat as whitelist only
			static := cfg.Providers[pid].Models
			for _, mid := range static {
				print(mid, pid)
			}
			// live /models only when no curated list (empty providers.<id>.models)
			if len(static) == 0 && (providers.Connected(cfg, pid) || cfg.Providers[pid].BaseURL != "") {
				if ids, err := providers.FetchModels(context.Background(), cfg, pid); err == nil {
					for _, mid := range ids {
						print(mid, pid)
					}
				}
			}
		}
		if len(seen) == 0 {
			fmt.Println("No models found. Run `antares provider add <id>` to connect one.")
		}
		return nil
	}
	prevProvider := cfg.Model.Provider
	resultProvider := prevProvider
	if len(args) > 1 {
		resultProvider = args[1]
	}
	cfg.Model.Default = args[0]
	cfg.Model.Provider = resultProvider
	if cfg.Model.Provider != prevProvider {
		cfg.ClearInlineModelCredentials()
	} else if p, ok := cfg.Providers[cfg.Model.Provider]; ok &&
		(strings.TrimSpace(p.BaseURL) != "" || strings.TrimSpace(p.APIKey) != "") {
		cfg.ClearInlineModelCredentials()
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Printf("active model: %s (%s)\n", cfg.Model.Default, cfg.Model.Provider)
	return nil
}

func cmdProvider(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list", "ls":
		for _, info := range providers.Catalog() {
			status := "connect"
			if providers.Connected(cfg, info.ID) {
				status = "connected"
			}
			active := ""
			if info.ID == cfg.Model.Provider {
				active = "  (active)"
			}
			fmt.Printf("  %-12s %-11s %s%s\n", info.ID, status, info.Label, active)
		}
		// configured providers not in the catalogue
		for id, p := range cfg.Providers {
			if _, ok := providers.For(id); ok {
				continue
			}
			active := ""
			if id == cfg.Model.Provider {
				active = "  (active)"
			}
			fmt.Printf("  %-12s %-11s %s%s\n", id, "configured", orDash(p.Label), active)
		}
		fmt.Println("\nUse `antares provider add <id> [api-key]` to connect, `antares provider use <id>` to switch.")
		return nil

	case "use", "switch":
		if len(args) < 2 {
			return fmt.Errorf("usage: antares provider use <id>")
		}
		id := args[1]
		if !providers.Connected(cfg, id) {
			return fmt.Errorf("%s is not connected — run `antares provider add %s <api-key>`", id, id)
		}
		providers.Activate(cfg, id, "")
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("active provider: %s (model %s)\n", cfg.Model.Provider, cfg.Model.Default)
		return nil

	case "add", "connect":
		if len(args) < 2 {
			return fmt.Errorf("usage: antares provider add <id> [api-key]")
		}
		id := args[1]
		key := ""
		if len(args) > 2 {
			key = args[2]
		}
		info, known := providers.For(id)
		if known && info.NeedsKey && key == "" && !providers.Connected(cfg, id) {
			return fmt.Errorf("%s needs an API key: antares provider add %s <api-key>", info.Label, id)
		}
		providers.Activate(cfg, id, key)
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("connected %s (model %s)\n", cfg.Model.Provider, cfg.Model.Default)
		return nil

	default:
		return fmt.Errorf("unknown provider command %q (use list|use|add)", sub)
	}
}

func cmdTheme(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		for _, n := range tui.ThemeNames() {
			mark := "  "
			if n == cfg.Display.Theme {
				mark = "❯ "
			}
			fmt.Printf("%s%s\n", mark, n)
		}
		return nil
	}
	if !tui.ThemeExists(args[0]) {
		return fmt.Errorf("unknown theme %q (run `antares theme` to list)", args[0])
	}
	cfg.Display.Theme = args[0]
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Printf("theme: %s\n", args[0])
	return nil
}

func cmdDoctor() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := bootstrap(ctx)
	if err != nil {
		fmt.Printf("✗ bootstrap: %v\n", err)
		return err
	}
	defer rt.close()

	fmt.Printf("✓ config        %s\n", config.ConfigFile())
	fmt.Printf("✓ workspace     %s\n", rt.cfg.Agent.Workspace)

	if err := rt.db.Ping(ctx); err != nil {
		fmt.Printf("✗ database      %v\n", err)
	} else {
		st, _ := rt.db.Stats(ctx)
		fmt.Printf("✓ database      %s · %d sessions, %d messages\n", rt.db.Driver(), st.Sessions, st.Messages)
	}

	ok, detail := rt.agent.Probe(ctx)
	if ok {
		fmt.Printf("✓ provider      %s\n", detail)
	} else {
		fmt.Printf("✗ provider      %s\n", detail)
	}

	if p := rt.agent.RAG(); p != nil {
		status := rag.Describe(ctx, rt.cfg, p)
		mark := "✓"
		if !status.Reachable {
			mark = "✗"
		}
		fmt.Printf("%s rag           %s — %s\n", mark, status.Provider, status.Detail)
	} else {
		fmt.Printf("· rag           disabled\n")
	}

	for _, st := range rt.mcp.Status(rt.cfg) {
		if st.Connected {
			fmt.Printf("✓ mcp %-10s %d tool(s)\n", st.Name, len(st.Tools))
		} else {
			fmt.Printf("✗ mcp %-10s %s\n", st.Name, st.Error)
		}
	}
	fmt.Printf("✓ tools         %d registered\n", len(tools.Default().Names()))
	return nil
}

// expandAll resolves ~ in a list of directories.
func expandAll(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, config.Expand(d))
	}
	return out
}

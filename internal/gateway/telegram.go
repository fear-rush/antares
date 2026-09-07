package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/version"
)

// Telegram talks to the Bot API over long polling, so no public domain or
// webhook is required — it works behind a home NAT.
type Telegram struct {
	cfg     config.Telegram
	mgr     *Manager
	client  *http.Client
	baseURL string

	mu        sync.RWMutex
	connected bool
	offset    int64
	me        string
	// richOff latches after an endpoint-missing failure so later sends skip
	// the doomed rich attempt entirely.
	richOff bool
}

// NewTelegram builds the Telegram adapter.
func NewTelegram(cfg config.Telegram, mgr *Manager) *Telegram {
	return &Telegram{
		cfg: cfg, mgr: mgr,
		// The read timeout must exceed the long-poll window.
		client:  &http.Client{Timeout: 90 * time.Second},
		baseURL: "https://api.telegram.org/bot" + cfg.BotToken,
	}
}

func (t *Telegram) Name() string { return "telegram" }

func (t *Telegram) Connected() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.connected
}

func (t *Telegram) setConnected(v bool) {
	t.mu.Lock()
	t.connected = v
	t.mu.Unlock()
}

// call performs one Bot API method.
func (t *Telegram) call(ctx context.Context, method string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.baseURL+"/"+method, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode %s: %w", method, err)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram %s failed (%d): %s", method, envelope.ErrorCode, envelope.Description)
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}

type tgUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type tgChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type tgMessage struct {
	MessageID   int64             `json:"message_id"`
	From        *tgUser           `json:"from"`
	Chat        tgChat            `json:"chat"`
	Text        string            `json:"text"`
	Caption     string            `json:"caption"`
	Date        int64             `json:"date"`
	ReplyMarkup *tgInlineKeyboard `json:"reply_markup,omitempty"`
}

type tgInlineKeyboard struct {
	InlineKeyboard [][]tgInlineButton `json:"inline_keyboard"`
}

type tgInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

// Start long-polls for updates until ctx is cancelled.
func (t *Telegram) Start(ctx context.Context) error {
	// Hydrate the rich-sent index so replies to rich messages sent before a
	// restart still resolve their quoted text.
	richSentLoad()
	var me tgUser
	if err := t.call(ctx, "getMe", nil, &me); err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	t.mu.Lock()
	t.me = me.Username
	token := t.cfg.BotToken
	t.mu.Unlock()
	t.setConnected(true)
	defer t.setConnected(false)
	slog.Info("telegram connected", "bot", "@"+me.Username)

	// Publish the "/" command menu. Off the main path so a hiccup never blocks
	// polling for messages.
	go func() {
		if err := SetTelegramCommands(context.Background(), token); err != nil {
			slog.Warn("telegram: could not set command menu", "error", err)
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		var updates []tgUpdate
		payload := map[string]any{
			"timeout":         50,
			"allowed_updates": []string{"message", "callback_query"},
		}
		if t.offset > 0 {
			payload["offset"] = t.offset
		}
		if err := t.call(ctx, "getUpdates", payload, &updates); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("getUpdates: %w", err)
		}

		for _, u := range updates {
			if u.UpdateID >= t.offset {
				t.offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				go t.handleCallback(ctx, *u.CallbackQuery)
				continue
			}
			if u.Message == nil || u.Message.From == nil || u.Message.From.IsBot {
				continue
			}
			go t.handleMessage(ctx, *u.Message)
		}
	}
}

// handleMessage authorises, runs the agent, and streams the reply back by
// editing a placeholder message.
func (t *Telegram) handleMessage(ctx context.Context, m tgMessage) {
	text := strings.TrimSpace(firstNonEmpty(m.Text, m.Caption))
	if text == "" {
		return
	}

	chatID := strconv.FormatInt(m.Chat.ID, 10)
	userID := strconv.FormatInt(m.From.ID, 10)
	isDirect := m.Chat.Type == "private"

	// In groups the bot only answers when mentioned or commanded.
	if !isDirect {
		t.mu.RLock()
		mention := "@" + t.me
		t.mu.RUnlock()
		if !strings.Contains(text, mention) && !strings.HasPrefix(text, "/") {
			return
		}
		text = strings.TrimSpace(strings.ReplaceAll(text, mention, ""))
	}

	msg := InboundMessage{
		Platform: "telegram", ChannelID: chatID, UserID: userID,
		UserName: firstNonEmpty(m.From.Username, m.From.FirstName),
		// Telegram's first name is the friendliest way to address someone.
		DisplayName: firstNonEmpty(m.From.FirstName, m.From.Username),
		Text:        text, IsDirect: isDirect,
		MessageID: strconv.FormatInt(m.MessageID, 10),
	}

	// Pairing is a DM-only handshake: a group chat must never receive a pairing
	// code. In a group, access is governed by the allow-lists and bindings; an
	// un-listed user gets silence, not a code.
	allowed, denial := t.mgr.authorize(ctx, msg, t.cfg.AllowedUsers, t.cfg.AllowedChats, isDirect && t.cfg.RequirePairing)
	if !allowed {
		if denial != "" && isDirect {
			_, _ = t.Send(ctx, Reply{ChannelID: chatID, Text: denial, ReplyTo: msg.MessageID})
		}
		return
	}

	if cmd, handled := t.builtinCommand(ctx, msg, text); handled {
		_, _ = t.Send(ctx, Reply{ChannelID: chatID, Text: cmd, ReplyTo: msg.MessageID})
		return
	}

	// Picker commands render inline buttons instead of plain text: /model and
	// /provider without args list the configured choices as tappable buttons.
	if kb, text, ok := t.pickerReply(ctx, msg, text); ok {
		_, _ = t.SendKeyboard(ctx, Reply{ChannelID: chatID, Text: text, ReplyTo: msg.MessageID}, kb)
		return
	}

	// React so the user sees work started instantly; the placeholder below is
	// only the streaming surface, not the acknowledgement.
	t.setReaction(ctx, chatID, msg.MessageID, "👀")
	_ = t.sendChatAction(ctx, chatID, "typing")

	// Keep the typing indicator alive on long tasks: Telegram drops it after
	// ~5s, which is why a slow agent looked dead.
	typingDone := make(chan struct{})
	defer close(typingDone)
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-typingDone:
				return
			case <-ticker.C:
				_ = t.sendChatAction(ctx, chatID, "typing")
			}
		}
	}()

	// A placeholder gives the user immediate feedback; it is edited as the
	// answer streams in, then finalised. Streaming previews stay on the
	// legacy HTML edit path; only finals go rich. A rich draft would need
	// sendRichMessageDraft plus a separate opt-in (rich_drafts): desktop
	// clients can leave rich draft frames overlaid until the chat redraws.
	placeholderID, err := t.Send(ctx, Reply{ChannelID: chatID, Text: "…", ReplyTo: msg.MessageID})
	if err != nil {
		slog.Warn("telegram: cannot send placeholder", "error", err)
		placeholderID = ""
	}

	var (
		mu       sync.Mutex
		lastEdit time.Time
		lastText string
	)
	partial := func(s string) {
		if !t.cfg.StreamEdits || placeholderID == "" || strings.TrimSpace(s) == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		// Telegram rate-limits edits; one per second is comfortably safe.
		// Render through the same HTML path as the final send so the stream
		// never shows raw Markdown the final message will restyle.
		html := renderTelegram(s)
		if time.Since(lastEdit) < time.Second || html == lastText {
			return
		}
		lastEdit, lastText = time.Now(), html
		_, _ = t.sendRendered(ctx, Reply{ChannelID: chatID, Text: truncateTG(s) + " ▌", EditID: placeholderID}, nil)
	}

	reply, runErr := t.mgr.handle(ctx, msg, partial)
	if runErr != nil {
		reply = "⚠️ " + runErr.Error()
		t.setReaction(ctx, chatID, msg.MessageID, "❌")
	} else if strings.TrimSpace(reply) != "" {
		t.setReaction(ctx, chatID, msg.MessageID, "✅")
	} else {
		t.setReaction(ctx, chatID, msg.MessageID, "")
	}
	// An empty reply with no error is intentional silence (disallowed user,
	// unregistered chat, relevance gate). Remove the placeholder and send
	// nothing rather than a "(no reply)" message.
	if strings.TrimSpace(reply) == "" {
		if placeholderID != "" {
			_ = t.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": placeholderID}, nil)
		}
		return
	}

	for i, chunk := range splitMarkdownSafe(reply, telegramLimit) {
		if i == 0 && placeholderID != "" {
			// First chunk finalizes the streamed preview in place: rich edit
			// when eligible (no duplicate preview), else the legacy HTML
			// edit on the same message.
			if _, ok := t.tryEditRich(ctx, chatID, placeholderID, chunk); !ok {
				if _, err := t.sendRendered(ctx, Reply{ChannelID: chatID, Text: chunk, EditID: placeholderID}, nil); err != nil {
					slog.Warn("telegram: send failed", "error", err)
				}
			}
			continue
		}
		replyTo := ""
		if i == 0 {
			replyTo = msg.MessageID
		}
		if _, err := t.sendRich(ctx, chatID, chunk, replyTo); err != nil {
			slog.Warn("telegram: send failed", "error", err)
		}
	}
}

// pickerReply answers /model and /provider without args with an inline
// keyboard of the configured choices. It reports false for anything else so
// the caller falls through to the agent.
func (t *Telegram) pickerReply(ctx context.Context, msg InboundMessage, text string) ([][]tgInlineButton, string, bool) {
	name, args, ok := parseCommand(text)
	if !ok || args != "" {
		return nil, "", false
	}
	cfg := t.mgr.config()
	switch strings.ToLower(name) {
	case "model":
		reply, err := t.mgr.handle(ctx, withText(msg, "/model "), nil)
		if err != nil {
			reply = "⚠️ " + err.Error()
		}
		return keyboardFor(cfg, "model"), reply, true
	case "provider":
		reply, err := t.mgr.handle(ctx, withText(msg, "/provider "), nil)
		if err != nil {
			reply = "⚠️ " + err.Error()
		}
		return keyboardFor(cfg, "provider"), reply, true
	}
	return nil, "", false
}

func parseCommand(text string) (name, args string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") || len(text) < 2 {
		return "", "", false
	}
	name, args, _ = strings.Cut(strings.TrimPrefix(text, "/"), " ")
	name, _, _ = strings.Cut(name, "@")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", "", false
	}
	return name, strings.TrimSpace(args), true
}

func withText(msg InboundMessage, text string) InboundMessage {
	msg.Text = text
	return msg
}

// handleCallback answers inline-keyboard taps. Only callbacks the bot itself
// issued are honoured: model picks (model:<id>) and provider picks
// (provider:<id>) are checked against the live config, everything else gets a
// short toast and is ignored.
func (t *Telegram) handleCallback(ctx context.Context, q tgCallbackQuery) {
	answer := func(text string) {
		_ = t.call(ctx, "answerCallbackQuery", map[string]any{
			"callback_query_id": q.ID, "text": text,
		}, nil)
	}
	if q.From == nil || q.From.IsBot || q.Message == nil {
		answer("")
		return
	}
	data := strings.TrimSpace(q.Data)
	kind, value, _ := strings.Cut(data, ":")
	chatID := strconv.FormatInt(q.Message.Chat.ID, 10)

	msg := InboundMessage{
		Platform: "telegram", ChannelID: chatID, UserID: strconv.FormatInt(q.From.ID, 10),
		UserName:    firstNonEmpty(q.From.Username, q.From.FirstName),
		DisplayName: firstNonEmpty(q.From.FirstName, q.From.Username),
		Text:        "", IsDirect: q.Message.Chat.Type == "private",
		MessageID: strconv.FormatInt(q.Message.MessageID, 10),
	}
	allowed, _ := t.mgr.authorize(ctx, msg, t.cfg.AllowedUsers, t.cfg.AllowedChats, msg.IsDirect && t.cfg.RequirePairing)
	if !allowed {
		answer("Not allowed.")
		return
	}
	mid, _ := strconv.ParseInt(msg.MessageID, 10, 64)

	removeKeyboard := func() {
		_ = t.call(ctx, "editMessageReplyMarkup", map[string]any{
			"chat_id": chatID, "message_id": mid,
			"reply_markup": map[string]any{"inline_keyboard": []any{}},
		}, nil)
	}

	var line, cmdName string
	switch kind {
	case "model":
		if !validModelChoice(t.mgr.config(), value) {
			answer("Unknown model.")
			return
		}
		cmdName, line = "model", "/model "+value
	case "provider":
		if !validProviderChoice(t.mgr.config(), value) {
			answer("Unknown provider.")
			return
		}
		cmdName, line = "provider", "/provider "+value
	default:
		answer("That button is no longer active.")
		return
	}
	reply, err := t.mgr.handle(ctx, InboundMessage{
		Platform: msg.Platform, ChannelID: msg.ChannelID, UserID: msg.UserID,
		UserName: msg.UserName, DisplayName: msg.DisplayName,
		Text: line, IsDirect: msg.IsDirect,
	}, nil)
	if err != nil {
		reply = "⚠️ " + err.Error()
	}
	removeKeyboard()
	answer(cmdName + " set to " + value)
	_, _ = t.Send(ctx, Reply{ChannelID: chatID, Text: reply})
}

// validModelChoice reports whether id is a configured model: the provider
// list, fallbacks, auxiliary, or the current default. Kept in gateway (not
// commands) to avoid an import cycle: commands never imports gateway.
func validModelChoice(cfg *config.Config, id string) bool {
	if cfg == nil || strings.TrimSpace(id) == "" {
		return false
	}
	if id == cfg.Model.Default {
		return true
	}
	seen := map[string]bool{}
	var check []string
	if p, ok := cfg.Providers[cfg.Model.Provider]; ok {
		check = append(check, p.Models...)
	}
	check = append(check, cfg.Model.Fallback...)
	if aux := strings.TrimSpace(cfg.Model.Auxiliary); aux != "" {
		check = append(check, aux)
	}
	for _, c := range check {
		if strings.TrimSpace(c) != "" {
			seen[c] = true
		}
	}
	return seen[id]
}

func validProviderChoice(cfg *config.Config, id string) bool {
	if cfg == nil || strings.TrimSpace(id) == "" {
		return false
	}
	_, ok := cfg.Providers[id]
	return ok
}

// keyboardFor builds the inline keyboard for a picker action: "model" gives
// one button per configured model, "provider" one per provider.
func keyboardFor(cfg *config.Config, kind string) [][]tgInlineButton {
	var items []string
	switch kind {
	case "provider":
		for n := range cfg.Providers {
			items = append(items, n)
		}
		sort.Strings(items)
	default:
		if p, ok := cfg.Providers[cfg.Model.Provider]; ok {
			items = append(items, p.Models...)
		}
		items = append(items, cfg.Model.Fallback...)
		if aux := strings.TrimSpace(cfg.Model.Auxiliary); aux != "" {
			items = append(items, aux)
		}
	}
	seen := map[string]bool{}
	rows := make([][]tgInlineButton, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		if len(rows) >= 12 {
			break
		}
		label := it
		if (kind == "model" && it == cfg.Model.Default) || (kind == "provider" && it == cfg.Model.Provider) {
			label = "● " + it
		}
		rows = append(rows, []tgInlineButton{{Text: label, CallbackData: kind + ":" + it}})
	}
	return rows
}

// builtinCommand handles slash commands that never reach the model.
func (t *Telegram) builtinCommand(ctx context.Context, msg InboundMessage, text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	cmd, _, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	cmd, _, _ = strings.Cut(cmd, "@")

	switch strings.ToLower(cmd) {
	case "start", "help":
		return "*Antares*\n\nSend me anything and I will work on it. I can read files, run commands, " +
			"search the web, and remember what matters across sessions.\n\n" +
			"/new — start a fresh session\n/pair — show the pairing code\n/help — this message", true
	case "pair":
		p, err := t.mgr.db.GetPairing(ctx, "telegram", msg.UserID)
		if err != nil {
			return "No pairing request found. Send any message to create one.", true
		}
		return fmt.Sprintf("Status: %s\nPairing code: %s", p.Status, p.Code), true
	case "new":
		if err := t.mgr.db.DeleteKV(ctx, GatewaySessionKey(t.mgr.config(), msg)); err != nil {
			slog.Debug("telegram: cannot clear session", "error", err)
		}
		return "Started a fresh session.", true
	}
	return "", false
}

func (t *Telegram) sendChatAction(ctx context.Context, chatID, action string) error {
	return t.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, nil)
}

// setReaction puts an emoji reaction on a user message via setMessageReaction.
// An empty emoji clears the bot's reaction. Failures are swallowed: a reaction
// is feedback, never fatal to the reply.
func (t *Telegram) setReaction(ctx context.Context, chatID, messageID, emoji string) {
	mid, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return
	}
	reaction := []map[string]string{}
	if emoji != "" {
		reaction = []map[string]string{{"type": "emoji", "emoji": emoji}}
	}
	_ = t.call(ctx, "setMessageReaction", map[string]any{
		"chat_id": chatID, "message_id": mid, "reaction": reaction, "is_big": false,
	}, nil)
}

// Send posts or edits a message and returns its id. Use SendKeyboard for a
// message with tappable inline buttons.
func (t *Telegram) Send(ctx context.Context, r Reply) (string, error) {
	return t.sendWithKeyboard(ctx, r, nil)
}

// SendKeyboard posts a message with an inline keyboard built by keyboardFor.
func (t *Telegram) SendKeyboard(ctx context.Context, r Reply, keyboard [][]tgInlineButton) (string, error) {
	return t.sendWithKeyboard(ctx, r, keyboard)
}

func (t *Telegram) sendWithKeyboard(ctx context.Context, r Reply, keyboard [][]tgInlineButton) (string, error) {
	if strings.TrimSpace(r.FilePath) != "" {
		return t.sendFile(ctx, r)
	}
	return t.sendRendered(ctx, r, keyboard)
}

// sendFileMaxBytes caps one gateway file send so a runaway artifact cannot
// exhaust disk or blow the Bot API upload budget.
const sendFileMaxBytes = 50 << 20

// sendFile delivers a local file to the chat: photos and videos render
// inline, everything else goes as a document. Text becomes the caption
// (Telegram caps captions at 1024 chars) so nothing is silently dropped.
func (t *Telegram) sendFile(ctx context.Context, r Reply) (string, error) {
	path := strings.TrimSpace(r.FilePath)
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", path)
	}
	if st.IsDir() {
		return "", fmt.Errorf("not a file: %s", path)
	}
	if st.Size() > sendFileMaxBytes {
		return "", fmt.Errorf("file too large (%d bytes, max %d): %s", st.Size(), sendFileMaxBytes, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	caption := firstNonEmpty(r.Caption, r.Text)
	caption = string(truncateCaption([]rune(strings.TrimSpace(caption))))
	method, field, _ := fileMethod(path)
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	_ = w.WriteField("chat_id", r.ChannelID)
	if r.ReplyTo != "" {
		_ = w.WriteField("reply_parameters", `{"message_id":`+r.ReplyTo+`,"allow_sending_without_reply":true}`)
	}
	if caption != "" {
		_ = w.WriteField("caption", caption)
		_ = w.WriteField("parse_mode", "HTML")
	}
	part, err := w.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.baseURL+"/"+method, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return "", fmt.Errorf("decode %s: %w", method, err)
	}
	if !envelope.OK {
		return "", fmt.Errorf("telegram %s failed (%d): %s", method, envelope.ErrorCode, envelope.Description)
	}
	var result tgMessage
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return "", err
	}
	return strconv.FormatInt(result.MessageID, 10), nil
}

// telegramCaptionMax is the Bot API caption cap.
const telegramCaptionMax = 1024

func truncateCaption(r []rune) []rune {
	if len(r) <= telegramCaptionMax {
		return r
	}
	return append(r[:telegramCaptionMax-1], '…')
}

// fileMethod picks the Bot API method and form field for a path: photos and
// videos render inline, audio goes as audio, everything else as a document.
func fileMethod(path string) (method, field, contentType string) {
	switch strings.ToLower(strings.TrimSuffix(filepath.Ext(path), "")) {
	case ".jpg", ".jpeg":
		return "sendPhoto", "photo", "image/jpeg"
	case ".png":
		return "sendPhoto", "photo", "image/png"
	case ".gif":
		return "sendAnimation", "animation", "image/gif"
	case ".webp":
		return "sendPhoto", "photo", "image/webp"
	case ".mp4":
		return "sendVideo", "video", "video/mp4"
	case ".mov":
		return "sendVideo", "video", "video/quicktime"
	case ".mp3":
		return "sendAudio", "audio", "audio/mpeg"
	case ".ogg", ".oga":
		return "sendAudio", "audio", "audio/ogg"
	case ".wav":
		return "sendAudio", "audio", "audio/wav"
	case ".weba":
		return "sendAudio", "audio", "audio/webm"
	default:
		return "sendDocument", "document", "application/octet-stream"
	}
}

// richMaxChars is the one hard rich limit counted locally (32,768 UTF-8
// chars). Other Bot API rich limits (500 blocks, 16 nesting, 20 columns)
// are not pre-counted; Telegram rejects with BadRequest and the send
// degrades to the legacy path.
const richMaxChars = 32768

// shouldAttemptRich mirrors Hermes _should_attempt_rich: rich only for final
// sends (never streaming previews), only when opted in, only for constructs
// the legacy HTML path degrades, never for known-bad client shapes, and only
// inside the char cap.
func (t *Telegram) shouldAttemptRich(text string, isFinal bool) bool {
	if !isFinal || !t.cfg.RichMessages || t.richSendDisabled() {
		return false
	}
	if strings.TrimSpace(text) == "" || !needsRichRendering(text) {
		return false
	}
	if richSkipDelivery(text) {
		return false
	}
	return len(text) <= richMaxChars
}

func (t *Telegram) richSendDisabled() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.richOff
}

func (t *Telegram) latchRichOff() {
	t.mu.Lock()
	t.richOff = true
	t.mu.Unlock()
}

// isRichCapabilityError reports endpoint-missing failures (old server): latch
// rich off, never retry per message. Per-message BadRequests stay transient
// eligible for legacy fallback.
func isRichCapabilityError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "no such method") || strings.Contains(s, "not implemented") {
		return true
	}
	if (strings.Contains(s, "method") || strings.Contains(s, "endpoint")) &&
		(strings.Contains(s, "not found") || strings.Contains(s, "does not exist")) {
		return true
	}
	return strings.Contains(s, "(404)")
}

func isRichFallbackError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "bad request") || isRichCapabilityError(err) ||
		strings.Contains(s, "unsupported")
}

// sendRich posts one chunk via sendRichMessage with the raw agent markdown
// so tables, task lists, details, and math render natively. Never pass
// rendered HTML here: that would escape and destroy rich syntax. Falls back
// to sendMessage HTML on permanent rejections; transient failures surface
// without a legacy resend so one send never delivers twice.
func (t *Telegram) sendRich(ctx context.Context, chatID, text, replyTo string) (string, error) {
	if !t.shouldAttemptRich(text, true) {
		return t.sendRendered(ctx, Reply{ChannelID: chatID, Text: text, ReplyTo: replyTo}, nil)
	}
	payload := map[string]any{"chat_id": chatID, "rich_message": richMarkdownPayload(text)}
	if replyTo != "" {
		payload["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	var result tgMessage
	if err := t.call(ctx, "sendRichMessage", payload, &result); err != nil {
		if isRichFallbackError(err) {
			if isRichCapabilityError(err) {
				t.latchRichOff()
			}
			slog.Debug("telegram: sendRichMessage rejected, falling back to HTML", "error", err)
			return t.sendRendered(ctx, Reply{ChannelID: chatID, Text: text, ReplyTo: replyTo}, nil)
		}
		// Transient or unknown: the request may have reached Telegram, so do
		// NOT legacy-resend and risk a duplicate.
		return "", err
	}
	mid := strconv.FormatInt(result.MessageID, 10)
	richSentRecord(chatID, mid, text)
	return mid, nil
}

// tryEditRich finalizes a streamed preview in place via editMessageText with
// the rich_message param: no fresh send plus delete, no duplicate preview.
// Returns false when the caller should fall back to the legacy HTML edit.
func (t *Telegram) tryEditRich(ctx context.Context, chatID, editID, text string) (string, bool) {
	if !t.shouldAttemptRich(text, true) {
		return "", false
	}
	payload := map[string]any{
		"chat_id": chatID, "message_id": editID,
		"rich_message": richMarkdownPayload(text),
	}
	var result tgMessage
	if err := t.call(ctx, "editMessageText", payload, &result); err != nil {
		if isRichFallbackError(err) {
			if isRichCapabilityError(err) {
				t.latchRichOff()
			}
			if strings.Contains(strings.ToLower(err.Error()), "not modified") {
				return editID, true
			}
			slog.Debug("telegram: rich editMessageText rejected, falling back to HTML edit", "error", err)
			return "", false
		}
		if strings.Contains(strings.ToLower(err.Error()), "not modified") {
			return editID, true
		}
		slog.Warn("telegram: rich edit transient failure, no legacy resend", "error", err)
		return "", true
	}
	mid := strconv.FormatInt(result.MessageID, 10)
	if mid == "0" {
		mid = editID
	}
	richSentRecord(chatID, mid, text)
	return mid, true
}

func (t *Telegram) sendRendered(ctx context.Context, r Reply, keyboard [][]tgInlineButton) (string, error) {
	html := renderTelegram(r.Text)
	payload := map[string]any{
		"chat_id":    r.ChannelID,
		"text":       truncateTG(html),
		"parse_mode": "HTML",
	}
	method := "sendMessage"
	if r.EditID != "" {
		method = "editMessageText"
		payload["message_id"] = r.EditID
	} else if r.ReplyTo != "" {
		payload["reply_parameters"] = map[string]any{"message_id": r.ReplyTo, "allow_sending_without_reply": true}
	}
	if len(keyboard) > 0 {
		rows := make([][]tgInlineButton, 0, len(keyboard))
		for _, row := range keyboard {
			if len(row) == 0 {
				continue
			}
			btns := make([]tgInlineButton, 0, len(row))
			for _, b := range row {
				if strings.TrimSpace(b.Text) == "" || strings.TrimSpace(b.CallbackData) == "" {
					continue
				}
				btns = append(btns, tgInlineButton{Text: b.Text, CallbackData: b.CallbackData})
			}
			if len(btns) > 0 {
				rows = append(rows, btns)
			}
		}
		if len(rows) > 0 {
			payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
		}
	}
	var result tgMessage
	err := t.call(ctx, method, payload, &result)
	if err != nil && (strings.Contains(err.Error(), "can't parse entities") || strings.Contains(err.Error(), "Bad Request")) {
		// Model output the HTML renderer could not balance; resend as plain
		// text so a formatting bug never eats the reply.
		plain := map[string]any{"chat_id": r.ChannelID, "text": truncateTG(r.Text)}
		if r.EditID != "" {
			plain["message_id"] = r.EditID
			method = "editMessageText"
		} else {
			method = "sendMessage"
			if r.ReplyTo != "" {
				plain["reply_parameters"] = map[string]any{"message_id": r.ReplyTo, "allow_sending_without_reply": true}
			}
		}
		err = t.call(ctx, method, plain, &result)
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(result.MessageID, 10), nil
}

// Telegram caps a message at 4096 characters.
const telegramLimit = 4000

func truncateTG(s string) string {
	// Rune cut: a byte cut inside a multi-byte character renders as
	// mojibake on the client.
	if len([]rune(s)) <= telegramLimit {
		return s
	}
	return string([]rune(s)[:telegramLimit]) + "…"
}

// splitForTelegram breaks a long reply on paragraph boundaries where possible.
// Cuts are rune-aligned: a byte cut inside a multi-byte character renders
// as mojibake on the client.
func splitForTelegram(s string) []string {
	if len([]rune(s)) <= telegramLimit {
		return []string{s}
	}
	var out []string
	for len([]rune(s)) > telegramLimit {
		head := string([]rune(s)[:telegramLimit])
		cut := strings.LastIndex(head, "\n\n")
		if cut < telegramLimit/2 {
			cut = strings.LastIndex(head, "\n")
		}
		if cut < telegramLimit/2 {
			out = append(out, strings.TrimSpace(head))
			s = strings.TrimSpace(string([]rune(s)[telegramLimit:]))
			continue
		}
		// cut is a byte index into head, which is a rune-aligned prefix of
		// s, so it is a valid cut point in s as well.
		out = append(out, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// GatewaySessionKey maps an inbound message to the KV key that holds its
// persistent session id. In a group channel with group_sessions_per_user set
// (the default), the sender's user id is folded in so each person gets their
// own conversation rather than sharing one channel-wide thread; a DM is already
// 1:1 so its key stays per-channel. Every place that reads, writes, or clears a
// gateway session must use this so a per-user session and its /new both target
// the same key.
func GatewaySessionKey(cfg *config.Config, msg InboundMessage) string {
	base := "gateway_session:" + msg.Platform + ":" + msg.ChannelID
	if !msg.IsDirect && cfg != nil && cfg.GroupSessionsPerUser && msg.UserID != "" {
		return base + ":" + msg.UserID
	}
	return base
}

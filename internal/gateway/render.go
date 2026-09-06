package gateway

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// This file renders model Markdown into Telegram HTML so tables and rich text
// survive the trip. Telegram has no table entity in sendMessage: pipe tables
// are re-laid as padded monospace inside <pre>, everything else maps to the
// HTML subset the Bot API accepts (b, i, u, s, code, pre, blockquote, a).

// renderTelegram converts Markdown-ish model output to Telegram-safe HTML:
// fenced code is preserved verbatim, pipe tables become monospace grids,
// inline styles map to <b>/<i>/<code>/<a>, headings become bold lines.
func renderTelegram(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")

	var out strings.Builder
	inFence := false
	fenceLang := ""
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				fenceLang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			} else {
				inFence = false
				fenceLang = ""
				out.WriteString("</pre>\n")
			}
			if inFence {
				if fenceLang != "" {
					out.WriteString("<pre><code class=\"language-" + escapeHTMLAttr(fenceLang) + "\">")
				} else {
					out.WriteString("<pre>")
				}
			}
			i++
			continue
		}
		if inFence {
			out.WriteString(escapeHTML(line) + "\n")
			i++
			continue
		}

		if isTableRow(trimmed) && i+1 < len(lines) && isTableDelimiter(lines[i+1]) {
			end := i + 2
			for end < len(lines) && isTableRow(strings.TrimSpace(lines[end])) {
				end++
			}
			out.WriteString(renderTableBlock(lines[i:end]))
			i = end
			continue
		}

		out.WriteString(renderLineHTML(line) + "\n")
		i++
	}
	if inFence {
		out.WriteString("</pre>\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// needsRichRendering reports whether the text has constructs the legacy
// HTML path degrades: pipe tables, GFM task lists, collapsible details,
// block math. Ordinary replies stay on the legacy path so clients render a
// consistent weight and spacing; rich is reserved for content where it
// materially improves output.
func needsRichRendering(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	if useRichMessage(s) {
		return true
	}
	lines := strings.Split(s, "\n")
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		for _, m := range []string{"- [ ] ", "- [x] ", "- [X] ", "* [ ] ", "* [x] ", "* [X] "} {
			if strings.HasPrefix(t, m) {
				return true
			}
		}
		if strings.HasPrefix(t, "<details") || t == "</details>" ||
			strings.HasPrefix(t, "<summary") || t == "</summary>" {
			return true
		}
	}
	return strings.Contains(s, "$$")
}

// richSkipDelivery reports content known to render badly or crash current
// desktop clients: math inside collapsible details, and CJK text that leaves
// overlay glyph artifacts in rich drafts. Both fall back to legacy HTML.
func richSkipDelivery(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	lower := strings.ToLower(s)
	if di := strings.Index(lower, "<details"); di >= 0 {
		if end := strings.Index(lower[di:], "</details>"); end >= 0 {
			block := s[di : di+end+len("</details>")]
			if strings.Contains(block, "$$") || hasBlockMath(block) {
				return true
			}
		}
	}
	return hasCJK(s)
}

func hasBlockMath(s string) bool {
	for _, re := range []string{`\\[`, `\\(`, `\\sum`, `\\frac`, `\\sqrt`, `\\int`, `\\begin`} {
		for i := 0; i+len(re) <= len(s); i++ {
			if s[i:i+len(re)] == re {
				return true
			}
		}
	}
	return false
}

// hasCJK reports Hiragana/Katakana, CJK Unified, Hangul, or compatibility
// ideographs. Byte-level scan avoids pulling a regex engine into the hot
// send path.
func hasCJK(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x3040 && r <= 0x30FF,
			r >= 0x3400 && r <= 0x4DBF,
			r >= 0x4E00 && r <= 0x9FFF,
			r >= 0xAC00 && r <= 0xD7AF,
			r >= 0xF900 && r <= 0xFAFF,
			r >= 0x20000 && r <= 0x323AF:
			return true
		}
	}
	return false
}

// richMarkdownPayload builds the InputRichMessage from raw agent markdown.
// Never pass rendered HTML here: that would escape and destroy rich syntax
// like table pipes. Single newlines become hard breaks so multi-line content
// does not collapse; fenced code and pipe-table blocks stay verbatim.
func richMarkdownPayload(s string) map[string]any {
	return map[string]any{"markdown": richNormalizeLinebreaks(s)}
}

func richNormalizeLinebreaks(s string) string {
	if s == "" || !strings.Contains(s, "\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	type span struct{ start, end int }
	var protected []span
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") {
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				j++
			}
			if j < len(lines) {
				j++
			}
			protected = append(protected, span{i, j})
			i = j
			continue
		}
		if isTableRow(trimmed) && i+1 < len(lines) && isTableDelimiter(lines[i+1]) {
			j := i + 2
			for j < len(lines) && isTableRow(strings.TrimSpace(lines[j])) {
				j++
			}
			protected = append(protected, span{i, j})
			i = j
			continue
		}
		i++
	}
	inProtected := func(idx int) bool {
		for _, p := range protected {
			if idx >= p.start && idx < p.end {
				return true
			}
		}
		return false
	}
	var out strings.Builder
	for idx, ln := range lines {
		if idx > 0 {
			prev, cur := lines[idx-1], ln
			if prev != "" && cur != "" && !inProtected(idx-1) && !inProtected(idx) {
				out.WriteString("  ")
			}
			out.WriteString("\n")
		}
		out.WriteString(ln)
	}
	return out.String()
}

// useRichMessage reports whether the text has a pipe table worth sending as a
// native rich table instead of monospace.
func useRichMessage(s string) bool {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if isTableRow(strings.TrimSpace(lines[i])) && isTableDelimiter(lines[i+1]) {
			return true
		}
	}
	return false
}

func splitMarkdownSafe(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	lines := strings.Split(s, "\n")
	type block struct{ start, end int }
	var blocks []block
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") {
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				j++
			}
			if j < len(lines) {
				j++
			}
			blocks = append(blocks, block{i, j})
			i = j
			continue
		}
		if isTableRow(trimmed) {
			j := i + 1
			if j < len(lines) && isTableDelimiter(lines[j]) {
				j++
				for j < len(lines) && isTableRow(strings.TrimSpace(lines[j])) {
					j++
				}
			}
			blocks = append(blocks, block{i, j})
			i = j
			continue
		}
		if trimmed == "" {
			i++
			continue
		}
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) != "" &&
			!strings.HasPrefix(strings.TrimSpace(lines[j]), "```") &&
			!isTableRow(strings.TrimSpace(lines[j])) {
			j++
		}
		blocks = append(blocks, block{i, j})
		i = j
	}

	var out []string
	var cur []string
	curLen := 0
	push := func() {
		if t := strings.TrimSpace(strings.Join(cur, "\n")); t != "" {
			out = append(out, t)
		}
		cur = nil
		curLen = 0
	}
	for _, b := range blocks {
		text := strings.Join(lines[b.start:b.end], "\n")
		if curLen+len(text)+1 > limit && curLen > 0 {
			push()
		}
		if len(text) > limit {
			for len(text) > limit {
				cut := strings.LastIndex(text[:limit], "\n")
				if cut < limit/2 {
					cut = limit
				}
				out = append(out, strings.TrimSpace(text[:cut]))
				text = strings.TrimSpace(text[cut:])
			}
			if text != "" {
				cur = append(cur, text)
				curLen += len(text) + 1
			}
			continue
		}
		cur = append(cur, text)
		curLen += len(text) + 1
	}
	push()
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

func isTableRow(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.Contains(line[1:], "|") {
		return false
	}
	for _, cell := range splitPipeRow(line) {
		if strings.TrimSpace(cell) != "" {
			return true
		}
	}
	return false
}

func isTableDelimiter(line string) bool {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "|")
	if line == "" {
		return false
	}
	for _, cell := range strings.Split(line, "|") {
		c := strings.TrimSpace(cell)
		if c == "" {
			continue
		}
		for _, r := range c {
			if r != '-' && r != ':' && r != ' ' {
				return false
			}
		}
		if !strings.Contains(c, "-") {
			return false
		}
	}
	return true
}

func splitPipeRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	return strings.Split(line, "|")
}

// renderTableBlock lays a pipe table out as padded monospace.
func renderTableBlock(lines []string) string {
	rows := make([][]string, 0, len(lines))
	width := 0
	for n, ln := range lines {
		if n == 1 {
			continue
		}
		cells := splitPipeRow(ln)
		for i := range cells {
			cells[i] = strings.TrimSpace(stripInline(cells[i]))
		}
		rows = append(rows, cells)
		if len(cells) > width {
			width = len(cells)
		}
	}
	if width == 0 {
		return "<pre>" + escapeHTML(strings.Join(lines, "\n")) + "</pre>\n"
	}
	colW := make([]int, width)
	for _, r := range rows {
		for i := 0; i < width; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			if w := displayWidth(cell); w > colW[i] {
				colW[i] = w
			}
			if colW[i] > 30 {
				colW[i] = 30
			}
		}
	}
	var b strings.Builder
	b.WriteString("<pre>")
	for n, r := range rows {
		for i := 0; i < width; i++ {
			cell := ""
			if i < len(r) {
				cell = truncateCell(r[i], 30)
			}
			b.WriteString(padCell(cell, colW[i]))
			if i < width-1 {
				b.WriteString(" | ")
			}
		}
		if n == 0 {
			b.WriteString("\n")
			for i := 0; i < width; i++ {
				b.WriteString(strings.Repeat("-", colW[i]))
				if i < width-1 {
					b.WriteString("-+-")
				}
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("</pre>\n")
	return b.String()
}

// displayWidth is the terminal cell width: CJK and most emoji count two.
// A negative or zero result (control chars, unknown sequences) clamps to 0
// so one bad glyph cannot corrupt the column math.
func displayWidth(s string) int {
	w := runewidth.StringWidth(s)
	if w < 0 {
		return 0
	}
	return w
}

func truncateCell(s string, max int) string {
	if runewidth.StringWidth(s) <= max {
		return s
	}
	return runewidth.Truncate(s, max-1, "…")
}

func padCell(s string, w int) string {
	if d := w - displayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// renderLineHTML maps one non-table, non-fence line to HTML.
func renderLineHTML(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	if h, ok := strings.CutPrefix(trimmed, "### "); ok {
		return "<b>" + inlineHTML(h) + "</b>"
	}
	if h, ok := strings.CutPrefix(trimmed, "## "); ok {
		return "<b>" + inlineHTML(h) + "</b>"
	}
	if h, ok := strings.CutPrefix(trimmed, "# "); ok {
		return "<b>" + inlineHTML(h) + "</b>"
	}
	if q, ok := strings.CutPrefix(trimmed, "&gt; "); ok {
		_ = q
		return "<blockquote>" + inlineHTML(strings.TrimPrefix(trimmed, "&gt; ")) + "</blockquote>"
	}
	if q, ok := strings.CutPrefix(trimmed, "> "); ok {
		return "<blockquote>" + inlineHTML(q) + "</blockquote>"
	}
	for _, m := range []string{"- ", "* ", "+ "} {
		if item, ok := strings.CutPrefix(trimmed, m); ok {
			return "• " + inlineHTML(item)
		}
	}
	return inlineHTML(line)
}

// inlineHTML maps inline Markdown to the HTML subset Telegram accepts.
func inlineHTML(s string) string {
	var out strings.Builder
	i := 0
	n := len(s)
	bold, italic, code, strike := false, false, false, false
	flush := func(tag string, open *bool) {
		if *open {
			out.WriteString("</" + tag + ">")
		} else {
			out.WriteString("<" + tag + ">")
		}
		*open = !*open
	}
	for i < n {
		switch {
		case s[i] == '\\' && i+1 < n && strings.ContainsRune(`_*[]()~`+"`"+`>#+-=|{}.!`, rune(s[i+1])):
			out.WriteString(escapeHTML(string(s[i+1])))
			i += 2
		case strings.HasPrefix(s[i:], "**") && !code:
			flush("b", &bold)
			i += 2
		case s[i] == '`' && !code && i+1 < n && s[i+1] == '`':
			i += 2
		case s[i] == '`':
			flush("code", &code)
			i++
		case strings.HasPrefix(s[i:], "~~") && !code:
			flush("s", &strike)
			i += 2
		case s[i] == '~' && !code:
			flush("s", &strike)
			i++
		case (s[i] == '*' || s[i] == '_') && !code:
			flush("i", &italic)
			i++
		case s[i] == '[' && !code:
			end := strings.Index(s[i:], "](")
			if end > 0 {
				label := s[i+1 : i+end]
				rest := s[i+end+2:]
				close := strings.Index(rest, ")")
				if close >= 0 {
					url := rest[:close]
					out.WriteString("<a href=\"" + escapeHTMLAttr(url) + "\">" + inlineHTML(label) + "</a>")
					i += end + 2 + close + 1
					continue
				}
			}
			out.WriteString(escapeHTML(string(s[i])))
			i++
		default:
			out.WriteString(escapeHTML(string(s[i])))
			i++
		}
	}
	if code {
		out.WriteString("</code>")
	}
	if strike {
		out.WriteString("</s>")
	}
	if italic {
		out.WriteString("</i>")
	}
	if bold {
		out.WriteString("</b>")
	}
	return out.String()
}

// stripInline drops Markdown markers for monospace table cells.
func stripInline(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "~~", "")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "*", "")
	s = strings.ReplaceAll(s, "_", "")
	if i := strings.Index(s, "]("); i >= 0 {
		if j := strings.LastIndex(s[:i], "["); j >= 0 {
			s = s[j+1 : i]
		}
	}
	return strings.TrimSpace(s)
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func escapeHTMLAttr(s string) string {
	s = escapeHTML(s)
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

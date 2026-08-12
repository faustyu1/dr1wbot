// ConvertMarkdown converts Markdown to Telegram-compatible HTML.
//
// Telegram parse_mode=HTML supports: b, strong, i, em, u, ins, s, strike, del,
// code, pre, a, blockquote, tg-spoiler, tg-emoji. It does NOT support h1-h6,
// ul, ol, li, table, sup, sub, mark.
//
// Existing HTML tags in the input (e.g. <tg-emoji>, <a>) are preserved verbatim.
package mdtext

import (
	"regexp"
	"strings"
)

// ConvertMarkdown converts Markdown text to Telegram-compatible HTML.
func ConvertMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	var out strings.Builder

	var inPre bool
	var preLang string
	var preBuf strings.Builder

	var inQuote bool
	var quoteBuf strings.Builder

	flushQuote := func() {
		if !inQuote {
			return
		}
		content := strings.TrimSpace(quoteBuf.String())
		if content != "" {
			out.WriteString("<blockquote>")
			out.WriteString(convertInline(content))
			out.WriteString("</blockquote>\n")
		}
		quoteBuf.Reset()
		inQuote = false
	}

	// paraBuf accumulates consecutive non-empty lines into one paragraph.
	var paraBuf strings.Builder
	flushPara := func() {
		if paraBuf.Len() == 0 {
			return
		}
		text := strings.TrimSpace(paraBuf.String())
		if text != "" {
			out.WriteString(convertInline(escapeTextNotTags(text)))
			out.WriteString("\n")
		}
		paraBuf.Reset()
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimLeft(line, " ")

		// Fenced code block.
		if strings.HasPrefix(trimmed, "```") {
			flushPara()
			flushQuote()
			if !inPre {
				inPre = true
				preLang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				preBuf.Reset()
				continue
			}
			inPre = false
			code := escapeHTML(preBuf.String())
			code = strings.TrimSuffix(code, "\n")
			if preLang != "" {
				out.WriteString(`<pre><code class="language-` + preLang + `">` + code + "</code></pre>\n")
			} else {
				out.WriteString("<pre>" + code + "</pre>\n")
			}
			preLang = ""
			preBuf.Reset()
			continue
		}
		if inPre {
			preBuf.WriteString(line + "\n")
			continue
		}

		// Blockquote.
		if strings.HasPrefix(trimmed, ">") {
			flushPara()
			rest := strings.TrimPrefix(trimmed, ">")
			rest = strings.TrimPrefix(rest, " ")
			if quoteBuf.Len() > 0 {
				quoteBuf.WriteString("\n")
			}
			quoteBuf.WriteString(rest)
			inQuote = true
			continue
		}
		flushQuote()

		// Empty line → paragraph break.
		if strings.TrimSpace(line) == "" {
			flushPara()
			out.WriteString("\n")
			continue
		}

		// Heading.
		if h := headingLevel(line); h > 0 {
			flushPara()
			rest := strings.TrimSpace(line[h:])
			out.WriteString("<b>" + convertInline(escapeTextNotTags(rest)) + "</b>\n")
			continue
		}

		// Horizontal rule.
		if isHRule(trimmed) {
			flushPara()
			out.WriteString("\n")
			continue
		}

		// Unordered list item.
		if isULItem(trimmed) {
			flushPara()
			indent := len(line) - len(strings.TrimLeft(line, " "))
			prefix := strings.Repeat("  ", indent/2)
			content := trimListMarker(trimmed)
			out.WriteString(prefix + "• " + convertInline(escapeTextNotTags(content)) + "\n")
			continue
		}

		// Ordered list item.
		if num, content, ok := parseOLItem(trimmed); ok {
			flushPara()
			indent := len(line) - len(strings.TrimLeft(line, " "))
			prefix := strings.Repeat("  ", indent/2)
			out.WriteString(prefix + num + ". " + convertInline(escapeTextNotTags(content)) + "\n")
			continue
		}

		// Table separator row (|---|---|) — skip.
		if isTableSeparator(trimmed) {
			flushPara()
			continue
		}

		// Table row → plain text.
		if isTableRow(trimmed) {
			flushPara()
			cells := splitTableRow(trimmed)
			parts := make([]string, len(cells))
			for j, c := range cells {
				parts[j] = convertInline(escapeTextNotTags(strings.TrimSpace(c)))
			}
			out.WriteString(strings.Join(parts, " | ") + "\n")
			continue
		}

		// Regular paragraph line — accumulate.
		if paraBuf.Len() > 0 {
			paraBuf.WriteString("\n")
		}
		paraBuf.WriteString(line)
	}

	flushPara()
	if inPre {
		code := escapeHTML(preBuf.String())
		code = strings.TrimSuffix(code, "\n")
		out.WriteString("<pre>" + code + "</pre>\n")
	}
	flushQuote()

	return strings.TrimRight(out.String(), "\n")
}

// --- inline conversion ---

var (
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reStrike     = regexp.MustCompile(`~~(.+?)~~`)
	reSpoiler    = regexp.MustCompile(`\|\|(.+?)\|\|`)
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reItalicStar = regexp.MustCompile(`(^|[^*])\*([^*\n]+?)\*([^*]|$)`)
	reItalicUnd  = regexp.MustCompile(`(^|[^_])_([^_\n]+?)_([^_]|$)`)
)

func convertInline(text string) string {
	text = reBold.ReplaceAllString(text, "<b>$1</b>")
	text = reStrike.ReplaceAllString(text, "<s>$1</s>")
	text = reSpoiler.ReplaceAllString(text, "<tg-spoiler>$1</tg-spoiler>")
	text = reInlineCode.ReplaceAllString(text, "<code>$1</code>")
	text = reItalicStar.ReplaceAllString(text, "$1<i>$2</i>$3")
	text = reItalicUnd.ReplaceAllString(text, "$1<i>$2</i>$3")
	text = reLink.ReplaceAllString(text, `<a href="$2">$1</a>`)
	return text
}

// --- HTML escaping ---

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

var reHTMLTag = regexp.MustCompile(`<[^>]+>`)

func escapeTextNotTags(s string) string {
	parts := reHTMLTag.Split(s, -1)
	tags := reHTMLTag.FindAllString(s, -1)
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(escapeAMPPreserveEntities(p))
		if i < len(tags) {
			b.WriteString(tags[i])
		}
	}
	return b.String()
}

func escapeAMPPreserveEntities(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// --- helpers ---

func headingLevel(line string) int {
	s := strings.TrimLeft(line, " ")
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n >= 1 && n <= 6 && n < len(s) && s[n] == ' ' {
		return n
	}
	return 0
}

func isHRule(s string) bool {
	return s == "---" || s == "***" || s == "___"
}

func isULItem(s string) bool {
	if len(s) < 2 {
		return false
	}
	return (s[0] == '-' || s[0] == '*' || s[0] == '+') && s[1] == ' '
}

func trimListMarker(s string) string {
	return s[2:]
}

func parseOLItem(s string) (num, content string, ok bool) {
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j > 0 && j+1 < len(s) && s[j] == '.' && s[j+1] == ' ' {
		return s[:j], s[j+2:], true
	}
	return "", "", false
}

func isTableRow(s string) bool {
	return strings.HasPrefix(s, "|") && strings.HasSuffix(strings.TrimRight(s, " "), "|")
}

func isTableSeparator(s string) bool {
	if !isTableRow(s) {
		return false
	}
	for _, cell := range splitTableRow(s) {
		cell = strings.TrimSpace(cell)
		cell = strings.ReplaceAll(cell, "-", "")
		cell = strings.ReplaceAll(cell, ":", "")
		if cell != "" {
			return false
		}
	}
	return true
}

func splitTableRow(s string) []string {
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	return strings.Split(s, "|")
}

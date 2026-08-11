// Package mdtext turns an incoming Telegram message into an LLM prompt and
// keeps the answer within Telegram's size limits.
//
// It deliberately knows nothing about telego so it stays trivially testable.
package mdtext

import (
	"strings"
	"unicode/utf16"
)

// Entity mirrors the parts of telego.MessageEntity we need. Telegram reports
// offsets and lengths in UTF-16 code units, not bytes and not runes, which
// matters as soon as the text contains Cyrillic or emoji.
type Entity struct {
	Type   string
	Offset int
	Length int
}

// EntityTypeMention is the entity type Telegram uses for "@username".
const EntityTypeMention = "mention"

// StripMention removes every "@botUsername" mention from text. In guest mode
// the mention is only the summoning token; feeding it to the model just invites
// the model to talk about itself in the third person.
func StripMention(text string, entities []Entity, botUsername string) string {
	if text == "" || botUsername == "" {
		return strings.TrimSpace(text)
	}

	units := utf16.Encode([]rune(text))
	want := strings.ToLower("@" + botUsername)

	// Collect matching ranges first, then cut from the end so earlier offsets
	// stay valid.
	var cuts [][2]int
	for _, e := range entities {
		if e.Type != EntityTypeMention {
			continue
		}
		if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > len(units) {
			continue // malformed or out of range; ignore rather than panic
		}
		got := string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
		if strings.ToLower(got) == want {
			cuts = append(cuts, [2]int{e.Offset, e.Offset + e.Length})
		}
	}

	for i := len(cuts) - 1; i >= 0; i-- {
		start, end := cuts[i][0], cuts[i][1]
		// A mention surrounded by spaces would leave a double space behind, so
		// absorb one of them. Anything further from the cut is left alone: the
		// message may contain indented code we must not reflow.
		if start > 0 && end < len(units) && units[start-1] == ' ' && units[end] == ' ' {
			end++
		}
		units = append(units[:start], units[end:]...)
	}

	return strings.TrimSpace(string(utf16.Decode(units)))
}

// RawFlag drops the house style for one question. It is a leading token so it
// cannot be hit by accident: a question that merely mentions a command flag
// somewhere in the middle is still an ordinary question.
const RawFlag = "-s"

// StripRawFlag removes a leading RawFlag and reports whether it was there. The
// caller decides who is allowed to use it; for everyone else the text comes
// back untouched and reads as part of the question.
func StripRawFlag(text string) (rest string, raw bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == RawFlag {
		return "", true
	}
	after, found := strings.CutPrefix(trimmed, RawFlag)
	if !found {
		return trimmed, false
	}
	// Require whitespace after the flag, so "-solid" is not read as a flag plus
	// the word "olid".
	if r := after[0]; r != ' ' && r != '\n' && r != '\t' {
		return trimmed, false
	}
	return strings.TrimSpace(after), true
}

// BuildPrompt assembles the model input. Guest mode hands us only the summoning
// message and, when present, the message it replied to — there is no chat
// history to draw on, so this is the entire context that exists.
func BuildPrompt(question, quoted string, quotedIsOwn bool) string {
	question = strings.TrimSpace(question)
	quoted = strings.TrimSpace(quoted)

	if quoted == "" {
		return question
	}

	// Guest mode carries no history, so a reply to our own answer is the only
	// way a conversation continues. Naming the quote as ours is what lets the
	// model treat the follow-up as one, instead of as a stranger's text to
	// analyse.
	label := "Сообщение, на которое ответили:"
	if quotedIsOwn {
		label = "Твой предыдущий ответ:"
	}

	if question == "" {
		return label + "\n" + quoted
	}
	if quotedIsOwn {
		return label + "\n" + quoted + "\n\nУточняющий вопрос по нему:\n" + question
	}
	return label + "\n" + quoted + "\n\nВопрос:\n" + question
}

// Truncate shortens s to at most limit runes, cutting on a paragraph break when
// one is close to the end and on a word boundary otherwise.
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}

	head := string(runes[:limit])
	// Only accept a boundary in the last quarter, otherwise we would throw away
	// a large part of an answer that simply has no break near the cut.
	minKeep := limit * 3 / 4
	if i := strings.LastIndex(head, "\n\n"); i >= minKeep {
		return strings.TrimRight(head[:i], " \n\t") + "\n\n…"
	}
	if i := strings.LastIndexAny(head, " \n\t"); i >= minKeep {
		return strings.TrimRight(head[:i], " \n\t") + " …"
	}
	return head + "…"
}

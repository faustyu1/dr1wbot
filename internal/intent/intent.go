// Package intent decides whether a summon asks for a picture or for text.
//
// Detection is a deliberate keyword match rather than a model call: a picture
// costs real money and takes far longer than an answer, so a wrong guess is
// expensive in both directions. Anchoring the triggers to the start of the
// message keeps the rule predictable — "нарисуй кота" generates, while
// "как нарисуй пишется" does not.
package intent

import (
	"strings"
	"unicode"
)

// Kind is what the user asked for.
type Kind int

const (
	// Text is an ordinary question for the model.
	Text Kind = iota
	// Image is a request to generate a picture.
	Image
)

// Command is the explicit way to ask for a picture, for when the phrasing does
// not trip any trigger.
const Command = "/img"

// triggers start an image request. Longer variants must precede their own
// prefixes so the whole phrase is stripped from the prompt.
var triggers = []string{
	"сгенерируй картинку", "сгенерируй изображение", "сгенерируй фото", "сгенерируй пикчу",
	"сгенерируй арт", "сгенери картинку",
	"создай картинку", "создай изображение", "создай фото",
	"нарисуй мне", "нарисуйте", "нарисуй", "нарисовать", "рисуй",
	"изобрази", "отрисуй",
	"generate an image", "generate image", "create an image", "make an image",
	"draw me", "draw", "picture of", "image of",
}

// Detect classifies text and returns the prompt with the trigger removed.
// For Text the prompt is the input unchanged.
func Detect(text string) (prompt string, kind Kind) {
	trimmed := strings.TrimSpace(text)

	// Telegram writes "/img@botname" when several bots share a chat. Drop the
	// suffix so it does not end up inside the prompt.
	if first, rest, _ := strings.Cut(trimmed, " "); strings.HasPrefix(first, "/") {
		if name, _, found := strings.Cut(first, "@"); found {
			trimmed = strings.TrimSpace(name + " " + rest)
		}
	}

	lower := strings.ToLower(trimmed)

	if rest, ok := afterPrefix(trimmed, lower, Command); ok {
		return rest, Image
	}
	for _, t := range triggers {
		if rest, ok := afterPrefix(trimmed, lower, t); ok {
			return rest, Image
		}
	}
	return trimmed, Text
}

// afterPrefix reports whether lower starts with prefix on a word boundary and
// returns what follows it, taken from the original text so the prompt keeps its
// capitalisation.
func afterPrefix(original, lower, prefix string) (string, bool) {
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}

	rest := original[len(prefix):]
	// A trigger must be a whole word: "drawing" is not "draw", and "нарисуйка"
	// is not "нарисуй". A trailing space in the trigger already guarantees this.
	if r := []rune(rest); len(r) > 0 && !unicode.IsSpace(r[0]) && !unicode.IsPunct(r[0]) {
		return "", false
	}

	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(rest), ",:;—-")), true
}

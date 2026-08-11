package tgemoji

import (
	"strings"
	"testing"
)

func TestTag(t *testing.T) {
	got := Tag("123", "⚙️")
	want := `<tg-emoji emoji-id="123">⚙️</tg-emoji>`
	if got != want {
		t.Errorf("Tag() = %q, want %q", got, want)
	}
}

func TestTagWithoutAnIDIsJustTheFallback(t *testing.T) {
	if got := Tag("", "⚙️"); got != "⚙️" {
		t.Errorf("Tag() = %q, want the bare fallback", got)
	}
}

func TestTagEscapes(t *testing.T) {
	// The fallback ends up inside HTML, so a stray angle bracket would break
	// the whole message rather than one icon.
	if got := Tag("1", "<b>"); got != `<tg-emoji emoji-id="1">&lt;b&gt;</tg-emoji>` {
		t.Errorf("Tag() = %q, want the fallback escaped", got)
	}
}

func TestStripLeavesTheFallback(t *testing.T) {
	in := "Ключи: " + Tag("1", "🔓") + " свободны, " + Tag("2", "🔒") + " заняты"
	want := "Ключи: 🔓 свободны, 🔒 заняты"
	if got := Strip(in); got != want {
		t.Errorf("Strip() = %q, want %q", got, want)
	}
}

func TestStripKeepsOtherMarkup(t *testing.T) {
	in := "<b>Панель</b> " + Tag("1", "⚙️")
	if got := Strip(in); got != "<b>Панель</b> ⚙️" {
		t.Errorf("Strip() = %q, want the bold left alone", got)
	}
}

func TestStripHandlesTagsBackToBack(t *testing.T) {
	// A non-greedy match matters here: a greedy one would swallow everything
	// between the first opening tag and the last closing one.
	in := Tag("1", "✅") + Tag("2", "❌")
	if got := Strip(in); got != "✅❌" {
		t.Errorf("Strip() = %q, want both fallbacks", got)
	}
}

func TestHas(t *testing.T) {
	if !Has(Tag("1", "✅")) {
		t.Error("Has() = false for text with a custom emoji")
	}
	if Has("обычный текст ✅") {
		t.Error("Has() = true for text with none, which would cost a pointless retry")
	}
}

func TestPlaceholderIsThreeEmoji(t *testing.T) {
	got := Placeholder()
	if n := strings.Count(got, "<tg-emoji"); n != 3 {
		t.Errorf("Placeholder() has %d emoji, want 3", n)
	}
	// The plain variant has to match, or a bot that cannot send custom emoji
	// would show a different marker.
	if n := strings.Count(PlaceholderPlain(), PlaceholderAlt); n != 3 {
		t.Errorf("PlaceholderPlain() = %q, want three fallbacks", PlaceholderPlain())
	}
	if Strip(got) != PlaceholderPlain() {
		t.Errorf("Strip(Placeholder()) = %q, want %q", Strip(got), PlaceholderPlain())
	}
}

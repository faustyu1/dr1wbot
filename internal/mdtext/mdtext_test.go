package mdtext

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// mentionEntity builds the entity Telegram would send for the first occurrence
// of want in text, using UTF-16 offsets the way Telegram does.
func mentionEntity(t *testing.T, text, want string) Entity {
	t.Helper()
	byteIdx := strings.Index(text, want)
	if byteIdx < 0 {
		t.Fatalf("%q not found in %q", want, text)
	}
	return Entity{
		Type:   EntityTypeMention,
		Offset: len(utf16.Encode([]rune(text[:byteIdx]))),
		Length: len(utf16.Encode([]rune(want))),
	}
}

func TestStripMention(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		mention  string
		username string
		want     string
	}{
		{
			name:     "leading mention",
			text:     "@dr1wbot что такое CAP теорема?",
			mention:  "@dr1wbot",
			username: "dr1wbot",
			want:     "что такое CAP теорема?",
		},
		{
			name:     "mention in the middle keeps single space",
			text:     "слушай @dr1wbot объясни",
			mention:  "@dr1wbot",
			username: "dr1wbot",
			want:     "слушай объясни",
		},
		{
			name:     "trailing mention",
			text:     "объясни это @dr1wbot",
			mention:  "@dr1wbot",
			username: "dr1wbot",
			want:     "объясни это",
		},
		{
			name:     "username match is case insensitive",
			text:     "@DR1WBot привет",
			mention:  "@DR1WBot",
			username: "dr1wbot",
			want:     "привет",
		},
		{
			name:     "another bot's mention is left alone",
			text:     "@otherbot @dr1wbot привет",
			mention:  "@dr1wbot",
			username: "dr1wbot",
			want:     "@otherbot привет",
		},
		{
			name:     "emoji before mention keeps utf-16 offsets aligned",
			text:     "🎯🎯 @dr1wbot считай",
			mention:  "@dr1wbot",
			username: "dr1wbot",
			want:     "🎯🎯 считай",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripMention(tt.text, []Entity{mentionEntity(t, tt.text, tt.mention)}, tt.username)
			if got != tt.want {
				t.Errorf("StripMention() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripMentionPreservesIndentedCode(t *testing.T) {
	text := "@dr1wbot почини\n```go\n\tif x {\n\t\treturn 1\n\t}\n```"
	want := "почини\n```go\n\tif x {\n\t\treturn 1\n\t}\n```"

	got := StripMention(text, []Entity{mentionEntity(t, text, "@dr1wbot")}, "dr1wbot")
	if got != want {
		t.Errorf("StripMention() = %q, want %q", got, want)
	}
}

func TestStripMentionIgnoresMalformedEntities(t *testing.T) {
	text := "@dr1wbot привет"
	entities := []Entity{
		{Type: EntityTypeMention, Offset: -1, Length: 8},
		{Type: EntityTypeMention, Offset: 0, Length: 9999},
		{Type: EntityTypeMention, Offset: 0, Length: 0},
	}

	if got := StripMention(text, entities, "dr1wbot"); got != text {
		t.Errorf("StripMention() = %q, want the text unchanged (%q)", got, text)
	}
}

func TestBuildPrompt(t *testing.T) {
	tests := []struct {
		name     string
		question string
		quoted   string
		want     string
	}{
		{
			name:     "question only",
			question: "что такое кворум?",
			want:     "что такое кворум?",
		},
		{
			name:   "bare mention on a reply uses the quoted message",
			quoted: "деплой упал с OOM",
			want:   "Сообщение, на которое ответили:\nдеплой упал с OOM",
		},
		{
			name:     "question and quote are both included",
			question: "почему?",
			quoted:   "деплой упал с OOM",
			want:     "Сообщение, на которое ответили:\nдеплой упал с OOM\n\nВопрос:\nпочему?",
		},
		{
			name: "nothing at all",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildPrompt(tt.question, tt.quoted, false); got != tt.want {
				t.Errorf("BuildPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Run("short text is untouched", func(t *testing.T) {
		if got := Truncate("привет", 100); got != "привет" {
			t.Errorf("Truncate() = %q, want %q", got, "привет")
		}
	})

	t.Run("counts runes not bytes", func(t *testing.T) {
		// 10 Cyrillic runes are 20 bytes; a byte-based limit would cut this.
		s := "абвгдеёжзи"
		if got := Truncate(s, 10); got != s {
			t.Errorf("Truncate() = %q, want it unchanged", got)
		}
	})

	t.Run("cuts on a paragraph break near the end", func(t *testing.T) {
		s := strings.Repeat("a", 80) + "\n\n" + strings.Repeat("b", 40)
		got := Truncate(s, 100)
		if got != strings.Repeat("a", 80)+"\n\n…" {
			t.Errorf("Truncate() = %q, want the paragraph boundary", got)
		}
	})

	t.Run("falls back to a word boundary", func(t *testing.T) {
		s := strings.Repeat("a", 80) + " " + strings.Repeat("b", 40)
		got := Truncate(s, 100)
		if got != strings.Repeat("a", 80)+" …" {
			t.Errorf("Truncate() = %q, want the word boundary", got)
		}
	})

	t.Run("hard cut when no boundary is close enough", func(t *testing.T) {
		s := strings.Repeat("a", 200)
		got := Truncate(s, 100)
		if got != strings.Repeat("a", 100)+"…" {
			t.Errorf("Truncate() = %q, want a hard cut", got)
		}
	})

	t.Run("result stays within the limit plus the ellipsis", func(t *testing.T) {
		for _, limit := range []int{10, 50, 100, 3500} {
			got := []rune(Truncate(strings.Repeat("слово ", 2000), limit))
			if len(got) > limit+2 {
				t.Errorf("limit %d: got %d runes, want at most %d", limit, len(got), limit+2)
			}
		}
	})
}

func TestStripRawFlag(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantRest string
		wantRaw  bool
	}{
		{name: "leading flag", in: "-s разбери это", wantRest: "разбери это", wantRaw: true},
		{name: "flag alone", in: "-s", wantRest: "", wantRaw: true},
		{name: "extra spaces around the flag", in: "  -s   вопрос  ", wantRest: "вопрос", wantRaw: true},
		{name: "newline after the flag", in: "-s\nвопрос", wantRest: "вопрос", wantRaw: true},
		{name: "no flag", in: "обычный вопрос", wantRest: "обычный вопрос", wantRaw: false},
		// A word that merely starts with the flag is a word.
		{name: "word starting with the flag", in: "-sql это что", wantRest: "-sql это что", wantRaw: false},
		{name: "flag in the middle", in: "что значит -s в grep", wantRest: "что значит -s в grep", wantRaw: false},
		{name: "empty", in: "", wantRest: "", wantRaw: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, raw := StripRawFlag(tt.in)
			if rest != tt.wantRest || raw != tt.wantRaw {
				t.Errorf("StripRawFlag(%q) = (%q, %t), want (%q, %t)", tt.in, rest, raw, tt.wantRest, tt.wantRaw)
			}
		})
	}
}

package mdtext

import (
	"strings"
	"testing"
)

func TestConvertMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"bold italic code", "**bold** and *italic* and `code`"},
		{"heading", "## Heading\n\ntext below"},
		{"unordered list", "- item 1\n- item 2\n- item 3"},
		{"ordered list", "1. first\n2. second\n3. third"},
		{"blockquote", "> blockquote text\n> continued"},
		{"code block with lang", "```python\nprint('hello')\n```"},
		{"link", "[link](https://example.com)"},
		{"strike spoiler", "Text with ~~strike~~ and ||spoiler||"},
		{"tg-emoji tag", `Text with <tg-emoji emoji-id="123">😀</tg-emoji> tag`},
		{"table with separator", "| Col1 | Col2 |\n|------|------|\n| a | b |\n| c | d |"},
		{"ampersand", "Line with & ampersand and < less-than"},
		{"mixed formatting", "## Title\n\nSome **bold** text with `code`.\n\n- List **item**\n- Another _item_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertMarkdown(tt.in)
			t.Logf("\nIN:  %q\nOUT: %q", tt.in, result)
		})
	}
}

func TestTgEmojiPreservation(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"tg-emoji in text", `Hello **world** 😀 and <tg-emoji emoji-id="5372954454653933911">😀</tg-emoji> done`},
		{"multiple tg-emoji", `<tg-emoji emoji-id="123">😀</tg-emoji> text <tg-emoji emoji-id="456">🎉</tg-emoji>`},
		{"tg-emoji with bold", `**bold** <tg-emoji emoji-id="123">😎</tg-emoji> more **bold**`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertMarkdown(tt.in)
			t.Logf("\nIN:  %s\nOUT: %s", tt.in, result)
			if !strings.Contains(result, "<tg-emoji") {
				t.Errorf("tg-emoji tag was lost in conversion: %s", result)
			}
		})
	}
}

func TestLinkWithAmpersand(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"simple link", "Text [link](https://example.com) end"},
		{"link with query", "Text [link](https://example.com?q=1&r=2) end"},
		{"superscript link", "Text.\n\n[¹](https://t.me/s/channel/123) [²](https://example.com)"},
		{"link with special chars", "[link](https://example.com/path?a=1&b=2&c=3)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertMarkdown(tt.in)
			t.Logf("\nIN:  %s\nOUT: %s", tt.in, result)
		})
	}
}

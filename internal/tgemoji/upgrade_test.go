package tgemoji

import (
	"strings"
	"testing"
)

func TestUpgradeEmojiWrapsSomeEmoji(t *testing.T) {
	text := "Привет! 😀 Как дела? 😂"
	result := UpgradeEmoji(text, 1.0) // 100% probability

	// With p=1.0, every eligible emoji should be wrapped
	if !strings.Contains(result, `<tg-emoji`) {
		t.Errorf("expected <tg-emoji> tags in result, got: %s", result)
	}
	if !strings.Contains(result, "5372954454653933911") {
		t.Errorf("expected custom_emoji_id for 😀 in result, got: %s", result)
	}
}

func TestUpgradeEmojiPreservesText(t *testing.T) {
	text := "Hello world!"
	result := UpgradeEmoji(text, 1.0)
	if result != text {
		t.Errorf("text without emoji should be unchanged, got: %s", result)
	}
}

func TestUpgradeEmojiZeroProbability(t *testing.T) {
	text := "Привет! 😀"
	result := UpgradeEmoji(text, 0)
	// With p=0, no emoji should be wrapped
	if strings.Contains(result, "<tg-emoji") {
		t.Errorf("with p=0 no emoji should be wrapped, got: %s", result)
	}
}

func TestUpgradeEmojiKeepsFallback(t *testing.T) {
	text := "😀"
	result := UpgradeEmoji(text, 1.0)
	// The original emoji should still be present as fallback inside the tag
	if !strings.Contains(result, "😀") {
		t.Errorf("original emoji should be kept as fallback, got: %s", result)
	}
}

func TestUpgradeEmojiSkipsHtmlTags(t *testing.T) {
	text := `<b>bold</b> 😀`
	result := UpgradeEmoji(text, 1.0)
	// The <b> tag should be preserved as-is
	if !strings.Contains(result, "<b>bold</b>") {
		t.Errorf("HTML tags should be preserved, got: %s", result)
	}
	// But the emoji should be upgraded
	if !strings.Contains(result, "<tg-emoji") {
		t.Errorf("emoji outside tags should be upgraded, got: %s", result)
	}
}

func TestUpgradeEmojiSkipsExistingTgEmoji(t *testing.T) {
	text := `<tg-emoji emoji-id="123">😀</tg-emoji> 😂`
	result := UpgradeEmoji(text, 1.0)
	// The existing tg-emoji tag should not be double-wrapped
	count := strings.Count(result, "<tg-emoji")
	if count != 2 { // one existing + one new
		t.Errorf("expected 2 tg-emoji tags (existing + new), got %d in: %s", count, result)
	}
}

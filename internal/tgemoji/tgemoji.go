// Package tgemoji renders Telegram's premium (custom) emoji.
//
// A custom emoji is written as <tg-emoji emoji-id="…">👍</tg-emoji> in HTML.
// The inner text is what non-premium clients — and any client that cannot load
// the sticker — show instead, so it must be a real emoji rather than a
// placeholder. Not every bot is allowed to send custom emoji, which is why
// Strip exists: the caller can retry a rejected message with the fallbacks in
// place of the markup.
package tgemoji

import (
	"html"
	"regexp"
	"strings"
)

// IDs of the custom emoji this bot uses. They come from a sticker pack the bot
// owner picked; another deployment would need its own.
const (
	IDSettings   = "5870982283724328568"
	IDProfile    = "5870994129244131212"
	IDPeople     = "5870772616305839506"
	IDUserOK     = "5891207662678317861"
	IDUserNo     = "5893192487324880883"
	IDChart      = "5870921681735781843"
	IDGrowth     = "5870930636742595124"
	IDLockClosed = "6037249452824072506"
	IDLockOpen   = "6037496202990194718"
	IDMegaphone  = "6039422865189638057"
	IDCheck      = "5870633910337015697"
	IDCross      = "5870657884844462243"
	IDTrash      = "5870875489362513438"
	IDInfo       = "6028435952299413210"
	IDBot        = "6030400221232501136"
	IDEye        = "6037397706505195857"
	IDClock      = "5983150113483134607"
	IDElapsed    = "5775896410780079073"
	IDBell       = "6039486778597970865"
	IDCode       = "5940433880585605708"
	IDParty      = "6041731551845159060"
	IDWrite      = "5870753782874246579"
	IDHouse      = "5873147866364514353"
	IDLink       = "5769289093221454192"
	IDCalendar   = "5890937706803894250"
	// IDPlaceholder is the "working on it" marker the bot posts before the
	// answer arrives. It lives here rather than in the environment because it
	// is part of how the bot looks, not part of how it is deployed.
	IDPlaceholder = "5289930378885214069"
)

// alt is the plain emoji each custom one falls back to. It is a table rather
// than a literal at every call site because the same icon appears in two
// places: the message body, as <tg-emoji> markup, and an inline-keyboard
// button, as icon_custom_emoji_id. Neither is guaranteed to render — a bot may
// not be allowed custom emoji at all — and both fall back to the same glyph.
var alt = map[string]string{
	IDSettings:    "⚙️",
	IDProfile:     "👤",
	IDPeople:      "👥",
	IDUserOK:      "👤",
	IDUserNo:      "🚫",
	IDChart:       "📊",
	IDGrowth:      "📈",
	IDLockClosed:  "🔒",
	IDLockOpen:    "🔓",
	IDMegaphone:   "📣",
	IDCheck:       "✅",
	IDCross:       "❌",
	IDTrash:       "🗑",
	IDInfo:        "ℹ️",
	IDBot:         "🤖",
	IDEye:         "👁",
	IDClock:       "⏰",
	IDElapsed:     "🕓",
	IDBell:        "🔔",
	IDCode:        "🔨",
	IDParty:       "🎉",
	IDWrite:       "✍️",
	IDHouse:       "🏘",
	IDLink:        "🔗",
	IDCalendar:    "📅",
	IDPlaceholder: "✍️",
}

// Alt is the plain emoji that stands for a custom one. It is what a rejected
// message is retried with, in the text and on the buttons alike, so a bot
// without premium emoji still shows the same panel.
func Alt(id string) string {
	if fallback, ok := alt[id]; ok {
		return fallback
	}
	return "•"
}

// Icon renders a custom emoji by id, taking its fallback from the table.
func Icon(id string) string { return Tag(id, Alt(id)) }

// PlaceholderAlt is what clients show when the custom placeholder cannot be
// rendered.
const PlaceholderAlt = "✍️"

// placeholderCount is how many copies of the marker are shown. Three reads as
// a progress indicator; one reads as a message the bot meant to send.
const placeholderCount = 3

// Placeholder renders the "working on it" marker.
func Placeholder() string {
	return strings.Repeat(Tag(IDPlaceholder, PlaceholderAlt), placeholderCount)
}

// PlaceholderPlain is the same marker for a bot that may not send custom emoji.
func PlaceholderPlain() string {
	return strings.Repeat(PlaceholderAlt, placeholderCount)
}

// Tag renders one custom emoji. Fallback is shown wherever the custom one
// cannot be.
func Tag(id, fallback string) string {
	if id == "" {
		return html.EscapeString(fallback)
	}
	return `<tg-emoji emoji-id="` + html.EscapeString(id) + `">` + html.EscapeString(fallback) + `</tg-emoji>`
}

// tagPattern matches a rendered custom emoji and captures its fallback.
var tagPattern = regexp.MustCompile(`<tg-emoji emoji-id="[^"]*">(.*?)</tg-emoji>`)

// Strip replaces every custom emoji with its fallback, turning a message the
// bot may not be allowed to send into one it certainly is.
func Strip(s string) string {
	return tagPattern.ReplaceAllString(s, "$1")
}

// Has reports whether s carries any custom emoji, so a caller can skip a
// pointless retry.
func Has(s string) bool {
	return strings.Contains(s, "<tg-emoji ")
}

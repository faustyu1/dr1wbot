package menu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"dr1wbot/internal/access"
	"dr1wbot/internal/keyring"
	"dr1wbot/internal/llm"
	"dr1wbot/internal/quota"
)

const adminID int64 = 111

// fakeSender records what the menu sent.
type fakeSender struct {
	sent    []telego.SendMessageParams
	edits   []telego.EditMessageTextParams
	answers int

	sendErr error
	editErr error
	// failOnce makes the first call fail, which is how Telegram refuses a bot
	// that may not send custom emoji.
	failOnce bool
	calls    int
}

func (f *fakeSender) SendMessage(_ context.Context, p *telego.SendMessageParams) (*telego.Message, error) {
	f.calls++
	f.sent = append(f.sent, *p)
	if f.failOnce && f.calls == 1 {
		return nil, errors.New("CUSTOM_EMOJI_INVALID")
	}
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &telego.Message{}, nil
}

func (f *fakeSender) EditMessageText(_ context.Context, p *telego.EditMessageTextParams) (*telego.Message, error) {
	f.calls++
	f.edits = append(f.edits, *p)
	if f.failOnce && f.calls == 1 {
		return nil, errors.New("CUSTOM_EMOJI_INVALID")
	}
	if f.editErr != nil {
		return nil, f.editErr
	}
	return &telego.Message{}, nil
}

func (f *fakeSender) AnswerCallbackQuery(context.Context, *telego.AnswerCallbackQueryParams) error {
	f.answers++
	return nil
}

// fakeModels reports fixed statistics.
type fakeModels struct{ stats llm.Stats }

func (f fakeModels) Stats() llm.Stats { return f.stats }

// fakeRoster is the whitelist.
type fakeRoster struct {
	admins  map[int64]bool
	entries []access.Entry
}

func (r fakeRoster) IsAdmin(id int64) bool { return r.admins[id] }
func (r fakeRoster) List() []access.Entry  { return r.entries }

func defaultStats() llm.Stats {
	return llm.Stats{
		Keys: keyring.Stats{Total: 3, Ready: 2, Parked: 1, NextReady: time.Now().Add(30 * time.Second)},
		Models: []llm.ModelStat{
			{Name: "openai/gpt-5.6-terra-pro", Answers: 7},
			{Name: "openai/gpt-5.6-terra", Answers: 2},
		},
		Requests: 10,
		QuotaOut: 1,
		Failures: 1,
	}
}

func newTestHandler(t *testing.T, sender Sender, ration Rationer) *Handler {
	t.Helper()
	return New(Options{
		Sender:      sender,
		Models:      fakeModels{stats: defaultStats()},
		Quota:       ration,
		Access:      fakeRoster{admins: map[int64]bool{adminID: true}},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername: "dr1wbot",
		StartedAt:   time.Now().Add(-90 * time.Minute),
	})
}

func privateMessage(from int64, text string) telego.Message {
	return telego.Message{
		Chat: telego.Chat{ID: from, Type: telego.ChatTypePrivate},
		From: &telego.User{ID: from},
		Text: text,
	}
}

func TestAdminGetsThePanel(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	if _, err := h.HandleMessage(context.Background(), privateMessage(adminID, "/admin")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}

	got := sender.sent[0]
	if got.ReplyMarkup == nil {
		t.Fatal("panel came with no buttons")
	}
	if !strings.Contains(got.Text, "Панель") {
		t.Errorf("text = %q, want the panel", got.Text)
	}
	// Key health is the one number an operator opens the panel for.
	if !strings.Contains(got.Text, "2</b> из 3") {
		t.Errorf("text = %q, want the key counts", got.Text)
	}
	if got.ParseMode != telego.ModeHTML {
		t.Errorf("parse mode = %q, want HTML so custom emoji render", got.ParseMode)
	}
}

func TestNonAdminGetsTheGreetingInsteadOfThePanel(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	if _, err := h.HandleMessage(context.Background(), privateMessage(999, "/admin")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	got := sender.sent[0]
	if strings.Contains(got.Text, "Панель") {
		t.Errorf("text = %q, want a stranger not to see the panel", got.Text)
	}
	// Refusing by name would confirm the panel exists.
	if strings.Contains(strings.ToLower(got.Text), "админ") && !strings.Contains(got.Text, "Привет") {
		t.Errorf("text = %q, want the plain greeting", got.Text)
	}
	if got.ReplyMarkup != nil {
		t.Error("greeting came with panel buttons for a non-admin")
	}
}

func TestStartShowsTheRemainingAllowance(t *testing.T) {
	sender := &fakeSender{}
	ration, err := quota.New(quota.Options{Limit: 30})
	if err != nil {
		t.Fatal(err)
	}
	ration.Take(999)
	h := newTestHandler(t, sender, ration)

	if _, err := h.HandleMessage(context.Background(), privateMessage(999, "/start")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	got := sender.sent[0].Text
	if !strings.Contains(got, "29") || !strings.Contains(got, "30") {
		t.Errorf("text = %q, want 29 of 30 left after one request", got)
	}
}

func TestStartSaysPrivateWhenPublicAccessIsOff(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	if _, err := h.HandleMessage(context.Background(), privateMessage(999, "/start")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if got := sender.sent[0].Text; !strings.Contains(got, "приватный") {
		t.Errorf("text = %q, want it to say the bot is private", got)
	}
}

func TestButtonSwapsTheScreenInPlace(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	query := telego.CallbackQuery{
		ID:      "cb-1",
		From:    telego.User{ID: adminID},
		Data:    callbackPrefix + screenLimits,
		Message: &telego.Message{Chat: telego.Chat{ID: adminID}, MessageID: 42},
	}
	if err := h.HandleCallback(context.Background(), query); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}

	if len(sender.edits) != 1 {
		t.Fatalf("edits = %d, want the menu edited in place rather than a new message", len(sender.edits))
	}
	if sender.edits[0].MessageID != 42 {
		t.Errorf("edited message %d, want 42", sender.edits[0].MessageID)
	}
	if !strings.Contains(sender.edits[0].Text, "openai/gpt-5.6-terra-pro") {
		t.Errorf("text = %q, want the model list on the limits screen", sender.edits[0].Text)
	}
	// Telegram spins the button until the query is answered.
	if sender.answers != 1 {
		t.Errorf("callback answered %d times, want exactly 1", sender.answers)
	}
}

func TestButtonsAreAdminOnly(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	query := telego.CallbackQuery{
		ID:      "cb-1",
		From:    telego.User{ID: 999},
		Data:    callbackPrefix + screenStats,
		Message: &telego.Message{Chat: telego.Chat{ID: 999}, MessageID: 42},
	}
	if err := h.HandleCallback(context.Background(), query); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}

	if len(sender.edits) != 0 {
		t.Errorf("edits = %+v, want a stranger's press to change nothing", sender.edits)
	}
	// The spinner still has to stop, or their client hangs on the button.
	if sender.answers != 1 {
		t.Errorf("callback answered %d times, want it answered anyway", sender.answers)
	}
}

func TestForeignCallbackDataIsIgnored(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	query := telego.CallbackQuery{ID: "cb-1", From: telego.User{ID: adminID}, Data: "something:else"}
	if err := h.HandleCallback(context.Background(), query); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}
	if sender.answers != 0 || len(sender.edits) != 0 {
		t.Error("a callback that is not ours was handled anyway")
	}
}

func TestUnknownScreenFallsBackToMain(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	query := telego.CallbackQuery{
		ID:      "cb-1",
		From:    telego.User{ID: adminID},
		Data:    callbackPrefix + "gone",
		Message: &telego.Message{Chat: telego.Chat{ID: adminID}, MessageID: 42},
	}
	if err := h.HandleCallback(context.Background(), query); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}
	// An old menu message from a previous version must still do something sane.
	if !strings.Contains(sender.edits[0].Text, "Панель") {
		t.Errorf("text = %q, want the main screen", sender.edits[0].Text)
	}
}

func TestCustomEmojiRejectionFallsBackToPlain(t *testing.T) {
	sender := &fakeSender{failOnce: true}
	h := newTestHandler(t, sender, nil)

	if _, err := h.HandleMessage(context.Background(), privateMessage(adminID, "/admin")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	if len(sender.sent) != 2 {
		t.Fatalf("sent %d messages, want the rejected one retried", len(sender.sent))
	}
	if strings.Contains(sender.sent[1].Text, "tg-emoji") {
		t.Errorf("retry = %q, want the custom emoji stripped", sender.sent[1].Text)
	}
	// The fallback characters must survive, or the retry loses its icons.
	if !strings.Contains(sender.sent[1].Text, "⚙️") {
		t.Errorf("retry = %q, want the plain emoji left behind", sender.sent[1].Text)
	}
}

func TestGroupMessagesAreNotThePanel(t *testing.T) {
	sender := &fakeSender{}
	h := newTestHandler(t, sender, nil)

	msg := privateMessage(adminID, "/admin")
	msg.Chat.Type = telego.ChatTypeGroup
	if _, err := h.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	// The panel lists whitelist ids and key health; a group is the wrong place
	// for it, and guest mode means the bot is not even a member.
	if len(sender.sent) != 0 {
		t.Errorf("sent %+v, want the panel to stay in private chats", sender.sent)
	}
}

func TestEveryScreenRenders(t *testing.T) {
	sender := &fakeSender{}
	ration, err := quota.New(quota.Options{Limit: 30})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, sender, ration)

	for _, name := range []string{screenMain, screenLimits, screenStats, screenPeople, screenSettings, screenHelp} {
		t.Run(name, func(t *testing.T) {
			text, keyboard := h.screen(name)
			if text == "" {
				t.Error("screen rendered nothing")
			}
			if keyboard == nil || len(keyboard.InlineKeyboard) == 0 {
				t.Error("screen has no buttons, leaving the operator stuck")
			}
			if strings.Count(text, "<b>") != strings.Count(text, "</b>") {
				t.Errorf("unbalanced bold tags in %q", text)
			}
		})
	}
}

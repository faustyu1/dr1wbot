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
	"dr1wbot/internal/settings"
	"dr1wbot/internal/tgemoji"
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
			{Name: "gemini-3.6-flash", Answers: 7},
			{Name: "gemini-3.5-flash", Answers: 2},
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
	if !strings.Contains(sender.edits[0].Text, "gemini-3.6-flash") {
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

// fakeTuner is an in-memory settings store.
type fakeTuner struct {
	values  settings.Values
	saveErr error
}

func (f *fakeTuner) Get() settings.Values { return f.values }

func (f *fakeTuner) Update(mutate func(*settings.Values)) (settings.Values, error) {
	if f.saveErr != nil {
		return f.values, f.saveErr
	}
	mutate(&f.values)
	return f.values, nil
}

// tunedHandler is newTestHandler with a settings store attached.
func tunedHandler(t *testing.T, sender Sender, tuner Tuner) (*Handler, *[]settings.Values) {
	t.Helper()
	var applied []settings.Values
	h := New(Options{
		Sender:      sender,
		Models:      fakeModels{stats: defaultStats()},
		Access:      fakeRoster{admins: map[int64]bool{adminID: true}},
		Settings:    tuner,
		Apply:       func(v settings.Values) { applied = append(applied, v) },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername: "dr1wbot",
		StartedAt:   time.Now(),
	})
	return h, &applied
}

// press builds the callback query one button press produces.
func press(from int64, data string) telego.CallbackQuery {
	return telego.CallbackQuery{
		ID:      "cb-1",
		From:    telego.User{ID: from},
		Data:    callbackPrefix + data,
		Message: &telego.Message{Chat: telego.Chat{ID: from, Type: telego.ChatTypePrivate}, MessageID: 42},
	}
}

func TestATypedValueIsApplied(t *testing.T) {
	sender := &fakeSender{}
	tuner := &fakeTuner{values: settings.Values{PublicDailyLimit: 30, BanFor: time.Hour}}
	h, applied := tunedHandler(t, sender, tuner)

	// The panel asks for a value, and the next ordinary message answers: an
	// inline keyboard has no text field, so this is the only way to type one.
	if err := h.HandleCallback(context.Background(), press(adminID, askPrefix+knobLimit)); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}
	handled, err := h.HandleMessage(context.Background(), privateMessage(adminID, "42"))
	if err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if !handled {
		t.Fatal("HandleMessage() handled = false, want the value claimed rather than answered as a question")
	}
	if tuner.values.PublicDailyLimit != 42 {
		t.Errorf("PublicDailyLimit = %d, want the typed 42", tuner.values.PublicDailyLimit)
	}
	if len(*applied) != 1 {
		t.Errorf("applied = %+v, want the change pushed into the running components", *applied)
	}

	// The panel message the question came from is redrawn, so the new number is
	// visible where it was changed.
	if len(sender.edits) == 0 || sender.edits[len(sender.edits)-1].MessageID != 42 {
		t.Errorf("edits = %+v, want the panel screen refreshed", sender.edits)
	}
}

func TestATypedDurationUnderstandsHowPeopleWriteIt(t *testing.T) {
	tests := []struct {
		typed string
		want  time.Duration
	}{
		{"2ч", 2 * time.Hour},
		{"90", 90 * time.Minute},
		{"7д", 7 * 24 * time.Hour},
		{"1д12ч", 36 * time.Hour},
		{"30m", 30 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.typed, func(t *testing.T) {
			tuner := &fakeTuner{values: settings.Values{BanFor: time.Hour}}
			h, _ := tunedHandler(t, &fakeSender{}, tuner)

			_ = h.HandleCallback(context.Background(), press(adminID, askPrefix+knobBan))
			if _, err := h.HandleMessage(context.Background(), privateMessage(adminID, tt.typed)); err != nil {
				t.Fatalf("HandleMessage() error = %v", err)
			}
			if tuner.values.BanFor != tt.want {
				t.Errorf("BanFor = %s, want %s", tuner.values.BanFor, tt.want)
			}
		})
	}
}

func TestABadTypedValueIsRejectedAndAskedAgain(t *testing.T) {
	sender := &fakeSender{}
	tuner := &fakeTuner{values: settings.Values{PublicDailyLimit: 30}}
	h, _ := tunedHandler(t, sender, tuner)

	_ = h.HandleCallback(context.Background(), press(adminID, askPrefix+knobLimit))
	if _, err := h.HandleMessage(context.Background(), privateMessage(adminID, "много")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if tuner.values.PublicDailyLimit != 30 {
		t.Errorf("PublicDailyLimit = %d, want it left alone", tuner.values.PublicDailyLimit)
	}

	// The question stays open: a typo should not mean walking back into the menu.
	if _, err := h.HandleMessage(context.Background(), privateMessage(adminID, "45")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if tuner.values.PublicDailyLimit != 45 {
		t.Errorf("PublicDailyLimit = %d, want the corrected value", tuner.values.PublicDailyLimit)
	}
}

func TestACommandCancelsAPendingQuestion(t *testing.T) {
	tuner := &fakeTuner{values: settings.Values{PublicDailyLimit: 30}}
	h, _ := tunedHandler(t, &fakeSender{}, tuner)

	_ = h.HandleCallback(context.Background(), press(adminID, askPrefix+knobLimit))
	if _, err := h.HandleMessage(context.Background(), privateMessage(adminID, "/admin")); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if tuner.values.PublicDailyLimit != 30 {
		t.Errorf("PublicDailyLimit = %d, want a command to cancel rather than be parsed", tuner.values.PublicDailyLimit)
	}
	// And an ordinary message afterwards is a question for the model again.
	handled, err := h.HandleMessage(context.Background(), privateMessage(adminID, "привет"))
	if err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if handled {
		t.Error("HandleMessage() handled = true, want an ordinary message left to the model")
	}
}

func TestAStaleQuestionIsNotAnAnswer(t *testing.T) {
	tuner := &fakeTuner{values: settings.Values{PublicDailyLimit: 30}}
	h, _ := tunedHandler(t, &fakeSender{}, tuner)

	_ = h.HandleCallback(context.Background(), press(adminID, askPrefix+knobLimit))
	// A question asked yesterday must not swallow today's first message.
	h.now = func() time.Time { return time.Now().Add(askTTL + time.Minute) }

	handled, err := h.HandleMessage(context.Background(), privateMessage(adminID, "42"))
	if err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}
	if handled {
		t.Error("HandleMessage() handled = true, want a stale question dropped")
	}
	if tuner.values.PublicDailyLimit != 30 {
		t.Errorf("PublicDailyLimit = %d, want it left alone", tuner.values.PublicDailyLimit)
	}
}

func TestATappedKnobCyclesItsPresets(t *testing.T) {
	tuner := &fakeTuner{values: settings.Values{PublicDailyLimit: 30}}
	h, applied := tunedHandler(t, &fakeSender{}, tuner)

	if err := h.HandleCallback(context.Background(), press(adminID, editPrefix+knobLimit)); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}
	if tuner.values.PublicDailyLimit != 50 {
		t.Errorf("PublicDailyLimit = %d, want the next preset after 30", tuner.values.PublicDailyLimit)
	}
	if len(*applied) != 1 {
		t.Errorf("applied = %+v, want the change pushed out", *applied)
	}
}

func TestSearchAndStreamingAreSwitches(t *testing.T) {
	tuner := &fakeTuner{values: settings.Values{SearchEnabled: false, StreamEnabled: false}}
	h, _ := tunedHandler(t, &fakeSender{}, tuner)

	_ = h.HandleCallback(context.Background(), press(adminID, editPrefix+knobSearch))
	_ = h.HandleCallback(context.Background(), press(adminID, editPrefix+knobStream))

	if !tuner.values.SearchEnabled || !tuner.values.StreamEnabled {
		t.Errorf("values = %+v, want both switched on", tuner.values)
	}
}

// fakeBans is the ban half of the quota store.
type fakeBans struct {
	list      []quota.Ban
	pardoned  []int64
	unnamed   []string
	publicOn  bool
	stats     quota.Stats
	pardonRet bool
}

func (f *fakeBans) Enabled() bool         { return f.publicOn }
func (f *fakeBans) Limit() int            { return 30 }
func (f *fakeBans) Peek(int64) (int, int) { return 1, 30 }
func (f *fakeBans) Stats() quota.Stats    { return f.stats }
func (f *fakeBans) Bans(int) []quota.Ban  { return f.list }
func (f *fakeBans) Pardon(id int64)       { f.pardoned = append(f.pardoned, id) }
func (f *fakeBans) PardonName(name string) bool {
	f.unnamed = append(f.unnamed, name)
	return f.pardonRet
}

func TestBansScreenListsAndUnbans(t *testing.T) {
	sender := &fakeSender{}
	bans := &fakeBans{publicOn: true, list: []quota.Ban{
		{ID: 555, Username: "spammer", Forever: true, Manual: true},
		{Username: "ghost", Forever: true, Manual: true},
	}}
	h := newTestHandler(t, sender, bans)

	if err := h.HandleCallback(context.Background(), press(adminID, screenBans)); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}
	screen := sender.edits[len(sender.edits)-1].Text
	for _, want := range []string{"555", "spammer", "ghost", "/ban"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen = %q, want it to mention %q", screen, want)
		}
	}

	if err := h.HandleCallback(context.Background(), press(adminID, pardonPrefix+"555")); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}
	if len(bans.pardoned) != 1 || bans.pardoned[0] != 555 {
		t.Errorf("pardoned = %v, want [555]", bans.pardoned)
	}

	// A ban on a name nobody has spoken under has no id to pardon, so the name
	// itself travels in the callback data.
	if err := h.HandleCallback(context.Background(), press(adminID, unnamePrefix+"ghost")); err != nil {
		t.Fatalf("HandleCallback() error = %v", err)
	}
	if len(bans.unnamed) != 1 || bans.unnamed[0] != "ghost" {
		t.Errorf("unnamed = %v, want [ghost]", bans.unnamed)
	}
}

func TestButtonLabelsCarryNoMarkup(t *testing.T) {
	sender := &fakeSender{}
	tuner := &fakeTuner{values: settings.Values{PublicDailyLimit: 30, Burst: 5, BurstWindow: time.Minute, BanFor: time.Hour}}
	h, _ := tunedHandler(t, sender, tuner)

	for _, screen := range []string{screenMain, screenSettings, screenManual} {
		if err := h.HandleCallback(context.Background(), press(adminID, screen)); err != nil {
			t.Fatalf("HandleCallback(%s) error = %v", screen, err)
		}
		keyboard := sender.edits[len(sender.edits)-1].ReplyMarkup
		if keyboard == nil {
			t.Fatalf("screen %s has no keyboard", screen)
		}
		for _, row := range keyboard.InlineKeyboard {
			for _, b := range row {
				// Telegram allows no entities on a keyboard, so a custom emoji
				// in a label would be shown as raw HTML.
				if tgemoji.Has(b.Text) || strings.Contains(b.Text, "<") {
					t.Errorf("button %q carries markup, which Telegram renders literally", b.Text)
				}
			}
		}
	}
}

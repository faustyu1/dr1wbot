// Package menu is the bot's private-chat interface: /start for everybody and
// /admin for whoever runs it.
//
// This is a second, separate way in. Everywhere else the bot lives in guest
// mode, where it answers a summon once and never sees the chat again; here it
// is an ordinary bot in an ordinary private chat, so it can send messages,
// attach buttons and edit them in place as the operator walks the menu.
//
// Screens are rebuilt from scratch on every button press rather than kept in
// any per-user state. The numbers they show are live, the bot has no memory of
// where a user was, and a menu message left open for a day still works.
package menu

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"

	"dr1wbot/internal/access"
	"dr1wbot/internal/llm"
	"dr1wbot/internal/quota"
	"dr1wbot/internal/settings"
	"dr1wbot/internal/tgemoji"
)

// Sender is the slice of the Telegram API this package needs.
type Sender interface {
	SendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error)
	EditMessageText(ctx context.Context, params *telego.EditMessageTextParams) (*telego.Message, error)
	AnswerCallbackQuery(ctx context.Context, params *telego.AnswerCallbackQueryParams) error
}

// Reporter is the model client's statistics.
type Reporter interface {
	Stats() llm.Stats
}

// Rationer is the public daily allowance and the ban list.
type Rationer interface {
	Enabled() bool
	Limit() int
	Peek(userID int64) (used, limit int)
	Stats() quota.Stats
	// Bans is what the panel can act on. Placing a ban stays with /ban, which
	// takes an id or a @username; a keyboard has nowhere to type either into.
	Bans(max int) []quota.Ban
	Pardon(userID int64)
	PardonName(username string) bool
}

// Roster is the whitelist, read-only from here: changing it stays with the
// /add and /del commands, which work in any chat and leave a trace.
type Roster interface {
	IsAdmin(userID int64) bool
	List() []access.Entry
}

// Tuner is the settings the panel may change.
type Tuner interface {
	Get() settings.Values
	Update(mutate func(*settings.Values)) (settings.Values, error)
}

// Prober checks the keys against the live API.
type Prober interface {
	Probe(ctx context.Context) []llm.KeyProbe
}

// Handler serves private-chat messages and button presses.
type Handler struct {
	sender Sender
	models Reporter
	ration Rationer
	roster Roster
	tuner  Tuner
	prober Prober
	apply  func(settings.Values)
	log    *slog.Logger

	botUsername string
	imagesOn    bool
	startedAt   time.Time

	// asked remembers which admin is in the middle of typing a value, and for
	// which knob. An inline keyboard has no text field, so "type it yourself"
	// has to be a short conversation: the panel asks, the next message answers.
	askedMu sync.Mutex
	asked   map[int64]question
	now     func() time.Time
}

// question is one pending "send me a value" prompt.
type question struct {
	knob      string
	chatID    int64
	messageID int // the panel message to redraw once the value arrives
	at        time.Time
}

// Options configures a Handler.
type Options struct {
	Sender   Sender
	Models   Reporter
	Quota    Rationer
	Access   Roster
	Settings Tuner
	Prober   Prober
	// Apply pushes a changed setting into the components that enforce it. It
	// runs on every successful edit, so nothing has to be restarted for a new
	// value to take effect.
	Apply  func(settings.Values)
	Logger *slog.Logger

	BotUsername string
	// ImagesOn is shown on the settings screen; drawing is off by default
	// because Google's image models have no free tier.
	ImagesOn bool
	// StartedAt is when the process came up, which is the window every counter
	// on the stats screen covers.
	StartedAt time.Time
}

// New builds a Handler.
func New(opts Options) *Handler {
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now()
	}
	return &Handler{
		asked:       make(map[int64]question),
		now:         time.Now,
		sender:      opts.Sender,
		models:      opts.Models,
		ration:      opts.Quota,
		roster:      opts.Access,
		tuner:       opts.Settings,
		prober:      opts.Prober,
		apply:       opts.Apply,
		log:         opts.Logger,
		botUsername: opts.BotUsername,
		imagesOn:    opts.ImagesOn,
		startedAt:   opts.StartedAt,
	}
}

// Screen names double as callback data. They are short because Telegram caps
// callback data at 64 bytes.
const (
	screenMain     = "m"
	screenLimits   = "lim"
	screenStats    = "st"
	screenPeople   = "ppl"
	screenSettings = "cfg"
	screenHelp     = "help"
	screenCheck    = "chk"
	screenBans     = "ban"
	screenManual   = "man"
)

// Action prefixes. An action does something and then redraws a screen, rather
// than only navigating.
const (
	pardonPrefix = "par:"
	// unnamePrefix lifts a ban placed on a @username that has never been seen,
	// which has no id to pardon.
	unnamePrefix = "unn:"
	// askPrefix starts the type-it-yourself flow for one knob.
	askPrefix = "ask:"
	// killPublic is the stop switch: one press closes the bot to the public
	// without cycling the limit down to zero one step at a time.
	killPublic = "kill"
)

// bansShown bounds the ban list. Telegram caps a keyboard's size, and a panel
// with sixty buttons is not a panel.
const bansShown = 8

// askTTL is how long the panel waits for a typed value before deciding the
// admin moved on. Without it, a question asked yesterday would swallow today's
// first message as an answer.
const askTTL = 5 * time.Minute

// editPrefix marks a button that changes a setting rather than opening a
// screen. The knob's name follows.
const editPrefix = "set:"

// Knob names, one per editable setting.
const (
	knobLimit  = "lim"
	knobBurst  = "brst"
	knobWindow = "win"
	knobBan    = "ban"
	knobTokens = "tok"
	knobRunes  = "runes"
	knobRaw    = "raw"
	knobGlobal = "glob"
	knobNewAcc = "new"
	knobCache  = "cache"
	knobSearch = "srch"
	knobStream = "strm"
)

// Choices a button walks through. A tap is the fast path — the values here are
// the ones that are actually sensible — and anything else is typed by hand on
// the manual screen, because a preset list is a guess about what somebody will
// want and this one is only a guess.
var (
	limitChoices  = []int{0, 10, 30, 50, 100, 300}
	burstChoices  = []int{3, 5, 10, 20}
	windowChoices = []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute}
	banChoices    = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
	tokenChoices  = []int{512, 1024, 2048, 4096}
	promptChoices = []int{500, 1000, 2000, 4000}
	globalChoices = []int{0, 200, 500, 1000, 2000, 5000}
	cacheChoices  = []time.Duration{0, 5 * time.Minute, 10 * time.Minute, time.Hour}
	// newAccountChoices are Telegram id boundaries. Ids are handed out in
	// ascending order, so a boundary is a rough "registered after about then".
	newAccountChoices = []int64{0, 6_000_000_000, 7_000_000_000, 8_000_000_000}
)

// knob is one editable setting: how its button reads, what a tap does, and how
// a typed value is understood. Keeping the three together is what lets the
// manual screen exist at all — otherwise every knob would need its parser
// written twice and they would drift.
type knob struct {
	name string
	// icon is the premium emoji, drawn both beside the line in the message body
	// and on the button itself through icon_custom_emoji_id. The label carries
	// no emoji of its own, or the same glyph would appear twice.
	icon  string
	title string // what the manual prompt calls it
	hint  string // an example of a value it accepts
	// label renders the button, current value included.
	label func(settings.Values) string
	// cycle advances to the next preset.
	cycle func(*settings.Values)
	// parse reads a typed value. Nil marks a switch, which has nothing to type.
	parse func(string) (func(*settings.Values), error)
}

// knobs are in the order the settings screen shows them.
var knobs = []knob{{
	name: knobLimit, icon: tgemoji.IDChart, title: "Лимит на человека в сутки",
	hint: "число запросов, 0 — выключить публику",
	label: func(v settings.Values) string {
		return "Лимит: " + limitLabel(v.PublicDailyLimit)
	},
	cycle: func(v *settings.Values) {
		v.PublicDailyLimit = settings.Cycle(limitChoices, v.PublicDailyLimit)
		if v.PublicDailyLimit > 0 {
			v.PublicOpenLimit = v.PublicDailyLimit
		}
	},
	parse: number(0, 100000, func(v *settings.Values, n int) {
		v.PublicDailyLimit = n
		if n > 0 {
			v.PublicOpenLimit = n
		}
	}),
}, {
	name: knobGlobal, icon: tgemoji.IDGrowth, title: "Потолок на весь бот в сутки",
	hint: "число запросов, 0 — без потолка",
	label: func(v settings.Values) string {
		return "Всего: " + limitLabel(v.GlobalDailyLimit)
	},
	cycle: func(v *settings.Values) { v.GlobalDailyLimit = settings.Cycle(globalChoices, v.GlobalDailyLimit) },
	parse: number(0, 1000000, func(v *settings.Values, n int) { v.GlobalDailyLimit = n }),
}, {
	name: knobBurst, icon: tgemoji.IDElapsed, title: "Всплеск: сколько запросов подряд ещё по-человечески",
	hint: "число, например 5",
	label: func(v settings.Values) string {
		return "Всплеск: " + strconv.Itoa(v.Burst)
	},
	cycle: func(v *settings.Values) { v.Burst = settings.Cycle(burstChoices, v.Burst) },
	parse: number(1, 1000, func(v *settings.Values, n int) { v.Burst = n }),
}, {
	name: knobWindow, icon: tgemoji.IDClock, title: "Окно всплеска",
	hint: "срок, например 1м или 30с",
	label: func(v settings.Values) string {
		return "Окно: " + shortDuration(v.BurstWindow)
	},
	cycle: func(v *settings.Values) { v.BurstWindow = settings.Cycle(windowChoices, v.BurstWindow) },
	parse: span(time.Second, 24*time.Hour, func(v *settings.Values, d time.Duration) { v.BurstWindow = d }),
}, {
	name: knobBan, icon: tgemoji.IDCross, title: "Автобан за всплеск",
	hint: "срок, например 15м, 2ч, 7д",
	label: func(v settings.Values) string {
		return "Автобан: " + shortDuration(v.BanFor)
	},
	cycle: func(v *settings.Values) { v.BanFor = settings.Cycle(banChoices, v.BanFor) },
	parse: span(time.Minute, 365*24*time.Hour, func(v *settings.Values, d time.Duration) { v.BanFor = d }),
}, {
	name: knobTokens, icon: tgemoji.IDWrite, title: "Токенов на ответ публике",
	hint: "число, например 1024",
	label: func(v settings.Values) string {
		return "Токенов: " + strconv.Itoa(v.PublicMaxTokens)
	},
	cycle: func(v *settings.Values) { v.PublicMaxTokens = settings.Cycle(tokenChoices, v.PublicMaxTokens) },
	parse: number(64, 32000, func(v *settings.Values, n int) { v.PublicMaxTokens = n }),
}, {
	name: knobRunes, icon: tgemoji.IDCode, title: "Символов в вопросе от публики",
	hint: "число, например 2000",
	label: func(v settings.Values) string {
		return "Символов: " + strconv.Itoa(v.PublicMaxRunes)
	},
	cycle: func(v *settings.Values) { v.PublicMaxRunes = settings.Cycle(promptChoices, v.PublicMaxRunes) },
	parse: number(50, 100000, func(v *settings.Values, n int) { v.PublicMaxRunes = n }),
}, {
	name: knobNewAcc, icon: tgemoji.IDUserNo, title: "Порог свежего аккаунта",
	hint: "Telegram id, например 7000000000; 0 — все равны",
	label: func(v settings.Values) string {
		return "Новые: " + thresholdLabel(v.NewAccountThreshold)
	},
	cycle: func(v *settings.Values) {
		v.NewAccountThreshold = settings.Cycle(newAccountChoices, v.NewAccountThreshold)
	},
	parse: bigNumber(0, 100_000_000_000, func(v *settings.Values, n int64) { v.NewAccountThreshold = n }),
}, {
	name: knobCache, icon: tgemoji.IDClock, title: "Кэш одинаковых вопросов",
	hint: "срок, например 10м; 0 — выключить",
	label: func(v settings.Values) string {
		return "Кэш: " + cacheLabel(v.CacheTTL)
	},
	cycle: func(v *settings.Values) { v.CacheTTL = settings.Cycle(cacheChoices, v.CacheTTL) },
	parse: span(0, 24*time.Hour, func(v *settings.Values, d time.Duration) { v.CacheTTL = d }),
}, {
	name: knobSearch, icon: tgemoji.IDLink, title: "Поиск в интернете",
	label: func(v settings.Values) string {
		return "Поиск: " + onOffShort(v.SearchEnabled)
	},
	cycle: func(v *settings.Values) { v.SearchEnabled = !v.SearchEnabled },
}, {
	name: knobStream, icon: tgemoji.IDWrite, title: "Ответ по мере написания (личка)",
	label: func(v settings.Values) string {
		return "Стриминг: " + onOffShort(v.StreamEnabled)
	},
	cycle: func(v *settings.Values) { v.StreamEnabled = !v.StreamEnabled },
}, {
	name: knobRaw, icon: tgemoji.IDSettings, title: "Флаг -s для админов",
	label: func(v settings.Values) string {
		return "Флаг -s: " + onOffShort(v.RawFlagEnabled)
	},
	cycle: func(v *settings.Values) { v.RawFlagEnabled = !v.RawFlagEnabled },
}}

// findKnob looks one up by its callback name.
func findKnob(name string) (knob, bool) {
	for _, k := range knobs {
		if k.name == name {
			return k, true
		}
	}
	return knob{}, false
}

// number builds a parser for a plain integer setting.
func number(low, high int, set func(*settings.Values, int)) func(string) (func(*settings.Values), error) {
	return func(raw string) (func(*settings.Values), error) {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("«%s» — это не число", raw)
		}
		if n < low || n > high {
			return nil, fmt.Errorf("нужно число от %d до %d", low, high)
		}
		return func(v *settings.Values) { set(v, n) }, nil
	}
}

// bigNumber is number for a Telegram id, which does not fit the usual range.
func bigNumber(low, high int64, set func(*settings.Values, int64)) func(string) (func(*settings.Values), error) {
	return func(raw string) (func(*settings.Values), error) {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("«%s» — это не число", raw)
		}
		if n < low || n > high {
			return nil, fmt.Errorf("нужно число от %d до %d", low, high)
		}
		return func(v *settings.Values) { set(v, n) }, nil
	}
}

// span builds a parser for a duration setting. It takes what a person types —
// "15м", "2ч", "7д" — as well as Go's own syntax.
func span(low, high time.Duration, set func(*settings.Values, time.Duration)) func(string) (func(*settings.Values), error) {
	return func(raw string) (func(*settings.Values), error) {
		d, err := parseSpan(raw)
		if err != nil {
			return nil, err
		}
		if d < low || d > high {
			return nil, fmt.Errorf("нужен срок от %s до %s", shortDuration(low), shortDuration(high))
		}
		return func(v *settings.Values) { set(v, d) }, nil
	}
}

// spanUnits maps every spelling of a time unit somebody might type.
var spanUnits = map[string]time.Duration{
	"s": time.Second, "sec": time.Second, "с": time.Second, "сек": time.Second,
	"m": time.Minute, "min": time.Minute, "м": time.Minute, "мин": time.Minute,
	"h": time.Hour, "hour": time.Hour, "ч": time.Hour, "час": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "д": 24 * time.Hour, "дн": 24 * time.Hour, "день": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "н": 7 * 24 * time.Hour, "нед": 7 * 24 * time.Hour,
}

// parseSpan reads a duration in the form a person writes it. A bare number is
// minutes, which is the unit somebody leaves off.
func parseSpan(raw string) (time.Duration, error) {
	text := strings.ToLower(strings.Join(strings.Fields(raw), ""))
	if text == "" {
		return 0, fmt.Errorf("пусто — нужен срок, например 15м")
	}
	if text == "0" || text == "выкл" || text == "off" {
		return 0, nil
	}

	var total time.Duration
	for rest := text; rest != ""; {
		digits := 0
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("«%s» — непонятный срок", raw)
		}
		n, err := strconv.Atoi(rest[:digits])
		if err != nil {
			return 0, fmt.Errorf("«%s» — непонятный срок", raw)
		}

		rest = rest[digits:]
		letters := 0
		for letters < len(rest) && (rest[letters] < '0' || rest[letters] > '9') {
			letters++
		}
		unit := time.Minute
		if letters > 0 {
			known, ok := spanUnits[rest[:letters]]
			if !ok {
				return 0, fmt.Errorf("«%s» — непонятная единица времени", rest[:letters])
			}
			unit = known
		}
		rest = rest[letters:]
		total += time.Duration(n) * unit
	}
	return total, nil
}

// callbackPrefix namespaces our buttons, so an unrelated future keyboard
// cannot be mistaken for a menu press.
const callbackPrefix = "menu:"

// sourceURL is where this program's source lives. AGPL-3.0 asks that users
// interacting with the program over a network be told, and the help screen is
// where that belongs. A fork should point this at its own repository.
const sourceURL = "https://github.com/faustyu1/dr1wbot"

// HandleMessage serves one private-chat message. It reports whether the
// message was a menu command: anything else is an ordinary question, and the
// caller answers it the same way it answers a summon. Greeting somebody who
// asked a question would be the most annoying possible reply.
func (h *Handler) HandleMessage(ctx context.Context, msg telego.Message) (handled bool, err error) {
	if msg.Chat.Type != telego.ChatTypePrivate || msg.From == nil {
		return false, nil
	}
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)
	command, _, _ := strings.Cut(text, " ")
	command, _, _ = strings.Cut(command, "@") // "/admin@dr1wbot" in a group-style client

	// A value the panel asked for is answered by an ordinary message, so it has
	// to be claimed before anything else looks at the text. A command cancels
	// the question instead: somebody who types /admin has moved on.
	if pending, waiting := h.takeQuestion(userID, strings.HasPrefix(command, "/")); waiting {
		return true, h.answerQuestion(ctx, userID, msg.Chat.ID, pending, text)
	}

	switch command {
	case "/start", "/help":
		var keyboard *telego.InlineKeyboardMarkup
		if h.roster.IsAdmin(userID) {
			keyboard = keys(row(button(tgemoji.IDSettings, "Панель", screenMain)))
		}
		return true, h.send(ctx, msg.Chat.ID, h.greeting(userID), keyboard)
	case "/admin":
		if !h.roster.IsAdmin(userID) {
			// Saying "not an admin" would confirm the menu exists; the greeting
			// says nothing either way.
			return true, h.send(ctx, msg.Chat.ID, h.greeting(userID), nil)
		}
		text, keyboard := h.screen(screenMain)
		return true, h.send(ctx, msg.Chat.ID, text, keyboard)
	default:
		return false, nil
	}
}

// HandleCallback serves one button press.
func (h *Handler) HandleCallback(ctx context.Context, query telego.CallbackQuery) error {
	name, found := strings.CutPrefix(query.Data, callbackPrefix)
	if !found {
		return nil
	}
	// Telegram spins the button until the query is answered, so this happens
	// before any work and regardless of how the work goes.
	defer func() {
		_ = h.sender.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	}()

	if !h.roster.IsAdmin(query.From.ID) {
		return nil
	}
	if query.Message == nil {
		return nil
	}

	// An action does something and then redraws the screen it lives on, so the
	// operator sees the result where they pressed.
	switch {
	case strings.HasPrefix(name, editPrefix):
		knob := strings.TrimPrefix(name, editPrefix)
		if err := h.turn(knob); err != nil {
			h.log.Error("could not save a setting", "knob", knob, "err", err)
		}
		name = screenSettings

	case name == killPublic:
		if err := h.killSwitch(); err != nil {
			h.log.Error("could not flip the public switch", "err", err)
		}
		name = screenSettings

	case strings.HasPrefix(name, pardonPrefix):
		if id, ok := parseID(strings.TrimPrefix(name, pardonPrefix)); ok && h.ration != nil {
			h.ration.Pardon(id)
			h.log.Info("pardoned by hand", "user_id", id, "by", query.From.ID)
		}
		name = screenBans

	case strings.HasPrefix(name, unnamePrefix):
		if username := strings.TrimPrefix(name, unnamePrefix); username != "" && h.ration != nil {
			h.ration.PardonName(username)
			h.log.Info("pardoned by hand", "username", username, "by", query.From.ID)
		}
		name = screenBans

	case strings.HasPrefix(name, askPrefix):
		// The panel asks, and the next message answers: the value lands in this
		// same screen, which is why the message id is remembered with it.
		knobName := strings.TrimPrefix(name, askPrefix)
		if k, ok := findKnob(knobName); ok && k.parse != nil {
			h.ask(query.From.ID, k.name, query.Message.GetChat().ID, query.Message.GetMessageID())
			text, keyboard := h.promptScreen(k)
			return h.edit(ctx, query.Message.GetChat().ID, query.Message.GetMessageID(), text, keyboard)
		}
		name = screenManual
	}

	text, keyboard := h.screenContext(ctx, name)
	return h.edit(ctx, query.Message.GetChat().ID, query.Message.GetMessageID(), text, keyboard)
}

// killSwitch closes the bot to the public in one press, or reopens it at the
// limit it had before. Cycling the limit down to zero step by step is exactly
// what nobody wants to be doing while watching abuse happen.
func (h *Handler) killSwitch() error {
	if h.tuner == nil {
		return nil
	}

	updated, err := h.tuner.Update(func(v *settings.Values) {
		if v.PublicDailyLimit > 0 {
			v.PublicOpenLimit = v.PublicDailyLimit // remember it for the way back
			v.PublicDailyLimit = 0
			return
		}
		if v.PublicOpenLimit > 0 {
			v.PublicDailyLimit = v.PublicOpenLimit
			return
		}
		v.PublicDailyLimit = limitChoices[len(limitChoices)-1]
	})
	if err != nil {
		return err
	}
	if h.apply != nil {
		h.apply(updated)
	}
	h.log.Warn("public access switched", "limit", updated.PublicDailyLimit)
	return nil
}

// parseID reads a user id out of callback data.
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// turn advances one setting to its next value and pushes it into the running
// components.
func (h *Handler) turn(name string) error {
	k, ok := findKnob(name)
	if !ok || h.tuner == nil {
		return nil
	}
	return h.change(k.cycle)
}

// change applies a mutation to the settings and pushes the result into whatever
// enforces it, so nothing has to be restarted for a new value to take effect.
func (h *Handler) change(mutate func(*settings.Values)) error {
	updated, err := h.tuner.Update(mutate)
	if err != nil {
		return err
	}
	if h.apply != nil {
		h.apply(updated)
	}
	return nil
}

// ask records that this admin is expected to type a value next.
func (h *Handler) ask(userID int64, knobName string, chatID int64, messageID int) {
	h.askedMu.Lock()
	defer h.askedMu.Unlock()
	h.asked[userID] = question{knob: knobName, chatID: chatID, messageID: messageID, at: h.now()}
}

// takeQuestion claims the pending question for this admin, if any is still
// live. A command cancels it: somebody who typed /admin is not answering.
func (h *Handler) takeQuestion(userID int64, isCommand bool) (question, bool) {
	h.askedMu.Lock()
	defer h.askedMu.Unlock()

	pending, waiting := h.asked[userID]
	if !waiting {
		return question{}, false
	}
	delete(h.asked, userID)
	if isCommand || h.now().Sub(pending.at) > askTTL {
		return question{}, false
	}
	return pending, true
}

// answerQuestion applies a typed value and redraws the settings screen where
// the question was asked, so the new number is visible in place.
func (h *Handler) answerQuestion(ctx context.Context, userID, chatID int64, pending question, text string) error {
	k, ok := findKnob(pending.knob)
	if !ok || k.parse == nil || h.tuner == nil {
		return nil
	}

	mutate, err := k.parse(text)
	if err != nil {
		// The question is already consumed, so the prompt is re-armed: a typo
		// should not mean walking back into the menu.
		h.ask(userID, k.name, pending.chatID, pending.messageID)
		return h.send(ctx, chatID, fmt.Sprintf("%s <b>%s</b>\n\n%s\n\nПришли значение ещё раз — или нажми любую кнопку в панели, чтобы отменить.",
			tgemoji.Icon(tgemoji.IDCross), escape(k.title), escape(err.Error())), nil)
	}

	if err := h.change(mutate); err != nil {
		h.log.Error("could not save a typed setting", "knob", k.name, "err", err)
		return h.send(ctx, chatID, fmt.Sprintf("%s Не получилось сохранить: <code>%s</code>",
			tgemoji.Icon(tgemoji.IDCross), escape(err.Error())), nil)
	}
	h.log.Info("setting typed in", "knob", k.name, "by", userID)

	if err := h.send(ctx, chatID, fmt.Sprintf("%s <b>%s</b> — теперь <b>%s</b>.",
		tgemoji.Icon(tgemoji.IDCheck), escape(k.title), escape(valueOf(k, h.tuner.Get()))), nil); err != nil {
		return err
	}

	// The panel message the question came from still shows the old number.
	screen, keyboard := h.settingsScreen()
	return h.edit(ctx, pending.chatID, pending.messageID, screen, keyboard)
}

// valueOf reads a knob's current value out of its button label, which is where
// it is already rendered the way a person reads it.
func valueOf(k knob, v settings.Values) string {
	label := k.label(v)
	if _, value, found := strings.Cut(label, ": "); found {
		return value
	}
	return label
}

// promptScreen is what the panel shows while it waits for a typed value.
func (h *Handler) promptScreen(k knob) (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>\n\n", tgemoji.Icon(tgemoji.IDWrite), escape(k.title))
	if h.tuner != nil {
		fmt.Fprintf(&b, "Сейчас: <b>%s</b>\n\n", escape(valueOf(k, h.tuner.Get())))
	}
	fmt.Fprintf(&b, "Пришли новое значение обычным сообщением — %s.\n\n", escape(k.hint))
	fmt.Fprintf(&b, "%s <i>Жду %s. Любая кнопка ниже отменяет.</i>",
		tgemoji.Icon(tgemoji.IDInfo), shortDuration(askTTL))

	return b.String(), keys(
		row(button("", "◁ Отмена", screenManual)),
	)
}

// greeting is what a non-admin sees. It says how to use the bot and how much of
// it they get, because both questions arrive constantly otherwise.
func (h *Handler) greeting(userID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Привет!</b>\n\n", tgemoji.Tag(tgemoji.IDBot, "🤖"))
	fmt.Fprintf(&b, "Я отвечаю в любом чате Telegram — позови меня по имени:\n\n")
	fmt.Fprintf(&b, "<blockquote><code>@%s объясни, что такое CAP-теорема</code></blockquote>\n\n", h.botUsername)
	fmt.Fprintf(&b, "%s Отвечу на вопрос, разберу картинку, продолжу разговор, если ответишь мне реплаем.\n\n",
		tgemoji.Tag(tgemoji.IDWrite, "✍️"))

	switch {
	case h.roster.IsAdmin(userID):
		fmt.Fprintf(&b, "%s Ты админ: /admin открывает панель.", tgemoji.Tag(tgemoji.IDSettings, "⚙️"))
	case h.ration != nil && h.ration.Enabled():
		used, limit := h.ration.Peek(userID)
		fmt.Fprintf(&b, "%s Сегодня осталось <b>%d</b> из %d запросов. Счётчик обнуляется в полночь UTC.",
			tgemoji.Tag(tgemoji.IDChart, "📊"), max(limit-used, 0), limit)
	default:
		fmt.Fprintf(&b, "%s Бот приватный: отвечаю только тем, кто есть в списке доступа.",
			tgemoji.Tag(tgemoji.IDLockClosed, "🔒"))
	}
	return b.String()
}

// screenContext renders a page that may need to call out to the network.
func (h *Handler) screenContext(ctx context.Context, name string) (string, *telego.InlineKeyboardMarkup) {
	if name == screenCheck {
		return h.checkScreen(ctx)
	}
	return h.screen(name)
}

// bansScreen lists who is sidelined right now, with a button to let each of
// them back in. Only lifting a ban lives here: placing one takes an id or a
// @username, and an inline keyboard has nowhere to type either — that is what
// /ban is for.
func (h *Handler) bansScreen() (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Баны</b>\n\n", tgemoji.Icon(tgemoji.IDUserNo))

	if h.ration == nil {
		fmt.Fprintf(&b, "%s Учёт запросов не настроен — банить нечем.", tgemoji.Icon(tgemoji.IDInfo))
		return b.String(), keys(row(backButton()))
	}

	list := h.ration.Bans(bansShown)
	if len(list) == 0 {
		fmt.Fprintf(&b, "%s Забаненных нет.\n\n", tgemoji.Icon(tgemoji.IDCheck))
		fmt.Fprintf(&b, "%s Забанить: <code>/ban 123456789</code> или <code>/ban @username 2ч</code>. "+
			"Без срока — навсегда.", tgemoji.Icon(tgemoji.IDInfo))
		return b.String(), keys(row(backButton()))
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(list)+1)
	for _, ban := range list {
		when := "навсегда"
		if !ban.Forever {
			when = "ещё " + shortDuration(time.Until(ban.Until))
		}
		why := "автобан"
		if ban.Manual {
			why = "вручную"
		}

		switch {
		case ban.ID != 0:
			who := fmt.Sprintf("<code>%d</code>", ban.ID)
			if ban.Username != "" {
				who += fmt.Sprintf(" @%s", escape(ban.Username))
			}
			fmt.Fprintf(&b, "%s %s — <b>%s</b>, %s", tgemoji.Icon(tgemoji.IDUserNo), who, when, why)
			if ban.Warns > 0 {
				fmt.Fprintf(&b, ", варнов %d", ban.Warns)
			}
			fmt.Fprintf(&b, "\n")
			rows = append(rows, row(styled(
				action(tgemoji.IDCheck, fmt.Sprintf("Разбанить %d", ban.ID), pardonPrefix+idText(ban.ID)),
				styleSuccess)))
		default:
			// A ban placed on a name nobody has spoken under yet: there is no id
			// to pardon, so the name itself travels in the callback data.
			fmt.Fprintf(&b, "%s @%s — <b>%s</b>, ждёт первого сообщения\n",
				tgemoji.Icon(tgemoji.IDUserNo), escape(ban.Username), when)
			rows = append(rows, row(styled(
				action(tgemoji.IDCheck, "Разбанить @"+ban.Username, unnamePrefix+ban.Username),
				styleSuccess)))
		}
	}

	fmt.Fprintf(&b, "\n%s <i>Забанить: <code>/ban id</code> или <code>/ban @username срок</code>. "+
		"Без срока — навсегда. Забаненный не получает ответа нигде, даже если он есть в списке доступа.</i>",
		tgemoji.Icon(tgemoji.IDInfo))

	rows = append(rows, row(backButton()))
	return b.String(), keys(rows...)
}

func idText(id int64) string { return strconv.FormatInt(id, 10) }

// screen renders one menu page. An unknown name falls back to the main screen,
// which is what an old message with a stale button should do.
func (h *Handler) screen(name string) (string, *telego.InlineKeyboardMarkup) {
	switch name {
	case screenLimits:
		return h.limitsScreen()
	case screenStats:
		return h.statsScreen()
	case screenPeople:
		return h.peopleScreen()
	case screenSettings:
		return h.settingsScreen()
	case screenHelp:
		return h.helpScreen()
	case screenBans:
		return h.bansScreen()
	case screenManual:
		return h.manualScreen()
	default:
		return h.mainScreen()
	}
}

func (h *Handler) mainScreen() (string, *telego.InlineKeyboardMarkup) {
	stats := h.models.Stats()

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Панель управления</b>\n\n", tgemoji.Tag(tgemoji.IDSettings, "⚙️"))
	fmt.Fprintf(&b, "%s Ключи: <b>%d</b> из %d свободны\n",
		health(stats.Keys.Ready), stats.Keys.Ready, stats.Keys.Total)
	fmt.Fprintf(&b, "%s Ответов за сессию: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDGrowth, "📈"), answered(stats))
	fmt.Fprintf(&b, "%s Аптайм: <b>%s</b>", tgemoji.Tag(tgemoji.IDClock, "⏰"), shortDuration(time.Since(h.startedAt)))

	return b.String(), keys(
		row(button(tgemoji.IDChart, "Лимиты", screenLimits),
			button(tgemoji.IDGrowth, "Статистика", screenStats)),
		row(button(tgemoji.IDPeople, "Доступ", screenPeople),
			button(tgemoji.IDSettings, "Настройки", screenSettings)),
		row(button(tgemoji.IDUserNo, "Баны", screenBans),
			button(tgemoji.IDInfo, "Справка", screenHelp)),
	)
}

func (h *Handler) limitsScreen() (string, *telego.InlineKeyboardMarkup) {
	stats := h.models.Stats()

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Лимиты и ключи</b>\n\n", tgemoji.Tag(tgemoji.IDChart, "📊"))

	fmt.Fprintf(&b, "<b>Ключи Google AI Studio</b>\n")
	fmt.Fprintf(&b, "%s Свободны: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDLockOpen, "🔓"), stats.Keys.Ready)
	fmt.Fprintf(&b, "%s Остывают: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDLockClosed, "🔒"), stats.Keys.Parked)
	if !stats.Keys.NextReady.IsZero() {
		fmt.Fprintf(&b, "%s Ближайший вернётся через <b>%s</b>\n",
			tgemoji.Tag(tgemoji.IDElapsed, "🕓"), shortDuration(time.Until(stats.Keys.NextReady)))
	}

	fmt.Fprintf(&b, "\n<b>Модели</b> (в порядке фолбэка)\n")
	for i, m := range stats.Models {
		mark := tgemoji.Tag(tgemoji.IDCheck, "✅")
		if i > 0 && m.Answers > 0 {
			// Answers on a fallback model mean the one above it ran dry.
			mark = tgemoji.Tag(tgemoji.IDBell, "🔔")
		}
		fmt.Fprintf(&b, "%s <code>%s</code> — <b>%d</b>\n", mark, m.Name, m.Answers)
	}

	if stats.QuotaOut > 0 {
		fmt.Fprintf(&b, "\n%s Упёрлись в лимиты <b>%d</b> раз: ни один ключ не ответил ни на одной модели.\n",
			tgemoji.Tag(tgemoji.IDCross, "❌"), stats.QuotaOut)
	}

	fmt.Fprintf(&b, "\n%s <i>Остаток квоты Google через API не отдаёт — эндпоинта для этого нет. "+
		"Здесь только то, что бот увидел сам с момента запуска. «Проверить ключи» дёргает список "+
		"моделей каждым ключом: это бесплатно и показывает, какие ключи вообще живы.</i>",
		tgemoji.Tag(tgemoji.IDInfo, "ℹ️"))

	return b.String(), keys(
		row(button(tgemoji.IDEye, "Проверить ключи", screenCheck)),
		row(backButton()),
	)
}

// checkScreen asks every key what it can reach. This is the only live question
// the API will answer: there is no quota endpoint, so "which keys work" is as
// close to "what are my limits" as it gets.
func (h *Handler) checkScreen(ctx context.Context) (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Проверка ключей</b>\n\n", tgemoji.Tag(tgemoji.IDEye, "👁"))

	if h.prober == nil {
		fmt.Fprintf(&b, "Проверка недоступна.")
		return b.String(), keys(row(backButton()))
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	alive := 0
	for _, p := range h.prober.Probe(probeCtx) {
		if p.Err == nil {
			alive++
			fmt.Fprintf(&b, "%s Ключ %d — <b>%d моделей</b>\n",
				tgemoji.Tag(tgemoji.IDCheck, "✅"), p.Index+1, p.Models)
			continue
		}
		fmt.Fprintf(&b, "%s Ключ %d — <code>%s</code>\n",
			tgemoji.Tag(tgemoji.IDCross, "❌"), p.Index+1, escape(p.Err.Error()))
	}

	fmt.Fprintf(&b, "\n%s Живых ключей: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDLockOpen, "🔓"), alive)
	fmt.Fprintf(&b, "\n%s <i>Сам список моделей квоту не тратит. Ключ, который отвечает здесь, "+
		"всё ещё может упереться в лимит на генерации — это разные счётчики.</i>",
		tgemoji.Tag(tgemoji.IDInfo, "ℹ️"))

	return b.String(), keys(
		row(button(tgemoji.IDEye, "Ещё раз", screenCheck)),
		row(backButton()),
	)
}

// probeTimeout bounds the whole key check. It is generous because it is one
// request per key, and stingy enough that a hung provider does not leave the
// operator staring at a spinner.
const probeTimeout = 20 * time.Second

func (h *Handler) statsScreen() (string, *telego.InlineKeyboardMarkup) {
	stats := h.models.Stats()

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Статистика</b>\n\n", tgemoji.Tag(tgemoji.IDGrowth, "📈"))
	fmt.Fprintf(&b, "%s Запросов к модели: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDWrite, "✍️"), stats.Requests)
	fmt.Fprintf(&b, "%s Ответов: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDCheck, "✅"), answered(stats))
	fmt.Fprintf(&b, "%s Неудач: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDCross, "❌"), stats.Failures)
	fmt.Fprintf(&b, "%s Поисков в интернете: <b>%d</b>\n", tgemoji.Icon(tgemoji.IDLink), stats.Searches)
	fmt.Fprintf(&b, "%s Аптайм: <b>%s</b>\n", tgemoji.Tag(tgemoji.IDClock, "⏰"), shortDuration(time.Since(h.startedAt)))

	if h.ration != nil && h.ration.Enabled() {
		q := h.ration.Stats()
		fmt.Fprintf(&b, "\n%s <b>Публичный доступ сегодня</b>\n", tgemoji.Tag(tgemoji.IDMegaphone, "📣"))
		fmt.Fprintf(&b, "%s Людей: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDPeople, "👥"), q.Users)
		fmt.Fprintf(&b, "%s Запросов: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDChart, "📊"), q.Requests)
		fmt.Fprintf(&b, "%s Обнуление через <b>%s</b>", tgemoji.Tag(tgemoji.IDCalendar, "📅"), shortDuration(q.ResetsIn))
	}

	return b.String(), keys(row(backButton()))
}

func (h *Handler) peopleScreen() (string, *telego.InlineKeyboardMarkup) {
	entries := h.roster.List()

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Доступ</b>\n\n", tgemoji.Tag(tgemoji.IDPeople, "👥"))

	if len(entries) == 0 {
		fmt.Fprintf(&b, "%s Список пуст — значит, отвечаю всем подряд.\n", tgemoji.Tag(tgemoji.IDLockOpen, "🔓"))
	}

	var admins, users, chats []string
	for _, e := range entries {
		line := fmt.Sprintf("<code>%d</code>", e.ID)
		if e.Static {
			line += " <i>· из .env</i>"
		}
		switch {
		case e.Admin:
			admins = append(admins, line)
		case e.IsChat:
			chats = append(chats, line)
		default:
			users = append(users, line)
		}
	}

	section(&b, tgemoji.Tag(tgemoji.IDProfile, "👤"), "Админы", admins)
	section(&b, tgemoji.Tag(tgemoji.IDUserOK, "👤"), "Люди", users)
	section(&b, tgemoji.Tag(tgemoji.IDHouse, "🏘"), "Чаты", chats)

	fmt.Fprintf(&b, "\n%s Менять список — командами <code>/add id</code> и <code>/del id</code> "+
		"в любом чате. Записи из <code>.env</code> так не убрать.", tgemoji.Tag(tgemoji.IDWrite, "✍️"))

	return b.String(), keys(row(backButton()))
}

func (h *Handler) settingsScreen() (string, *telego.InlineKeyboardMarkup) {
	stats := h.models.Stats()

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Настройки</b>\n\n", tgemoji.Icon(tgemoji.IDSettings))

	models := make([]string, 0, len(stats.Models))
	for _, m := range stats.Models {
		models = append(models, m.Name)
	}
	fmt.Fprintf(&b, "%s Модели: <code>%s</code>\n", tgemoji.Icon(tgemoji.IDCode), strings.Join(models, ", "))
	fmt.Fprintf(&b, "%s Ключей в пуле: <b>%d</b>\n", tgemoji.Icon(tgemoji.IDLockOpen), stats.Keys.Total)

	fmt.Fprintf(&b, "%s Картинки: %s\n", tgemoji.Icon(tgemoji.IDEye), onOff(h.imagesOn))
	fmt.Fprintf(&b, "<i>Это из <code>.env</code>: меняется только перезапуском.</i>\n")

	if h.tuner == nil {
		return b.String(), keys(row(backButton()))
	}
	v := h.tuner.Get()

	fmt.Fprintf(&b, "\n%s <b>Публичный доступ</b>\n", tgemoji.Icon(tgemoji.IDMegaphone))
	if v.PublicDailyLimit > 0 {
		fmt.Fprintf(&b, "%s Лимит: <b>%d запросов в сутки</b> на человека\n",
			tgemoji.Icon(tgemoji.IDChart), v.PublicDailyLimit)
	} else {
		fmt.Fprintf(&b, "%s Выключен — отвечаю только по списку\n", tgemoji.Icon(tgemoji.IDLockClosed))
	}
	fmt.Fprintf(&b, "%s Всплеск: <b>%d за %s</b>, дальше бан\n",
		tgemoji.Icon(tgemoji.IDElapsed), v.Burst, shortDuration(v.BurstWindow))
	fmt.Fprintf(&b, "%s Автобан: <b>%s</b>\n", tgemoji.Icon(tgemoji.IDCross), shortDuration(v.BanFor))
	if v.GlobalDailyLimit > 0 {
		fmt.Fprintf(&b, "%s Потолок на весь бот: <b>%d запросов в сутки</b>\n",
			tgemoji.Icon(tgemoji.IDGrowth), v.GlobalDailyLimit)
	} else {
		fmt.Fprintf(&b, "%s Потолок на весь бот: <b>без ограничения</b>\n", tgemoji.Icon(tgemoji.IDGrowth))
	}
	if v.NewAccountThreshold > 0 {
		fmt.Fprintf(&b, "%s Свежим аккаунтам (id ≥ %d) — половина лимита\n",
			tgemoji.Icon(tgemoji.IDUserNo), v.NewAccountThreshold)
	}
	fmt.Fprintf(&b, "%s Ответ публике: до <b>%d токенов</b>\n", tgemoji.Icon(tgemoji.IDWrite), v.PublicMaxTokens)
	fmt.Fprintf(&b, "%s Вопрос публики: до <b>%d символов</b>\n", tgemoji.Icon(tgemoji.IDCode), v.PublicMaxRunes)

	fmt.Fprintf(&b, "\n%s <b>Ответы</b>\n", tgemoji.Icon(tgemoji.IDWrite))
	fmt.Fprintf(&b, "%s Поиск в интернете: %s\n", tgemoji.Icon(tgemoji.IDLink), onOff(v.SearchEnabled))
	fmt.Fprintf(&b, "%s Стриминг ответа: %s\n", tgemoji.Icon(tgemoji.IDWrite), onOff(v.StreamEnabled))
	if v.CacheTTL > 0 {
		fmt.Fprintf(&b, "%s Кэш одинаковых вопросов: <b>%s</b>\n",
			tgemoji.Icon(tgemoji.IDClock), shortDuration(v.CacheTTL))
	} else {
		fmt.Fprintf(&b, "%s Кэш одинаковых вопросов: <b>выключен</b>\n", tgemoji.Icon(tgemoji.IDClock))
	}
	fmt.Fprintf(&b, "%s Флаг <code>-s</code> для админов: %s\n", tgemoji.Icon(tgemoji.IDSettings), onOff(v.RawFlagEnabled))

	fmt.Fprintf(&b, "\n%s <i>Кнопка перебирает готовые значения по кругу. "+
		"Нужно своё число или срок — «Ввести вручную». Изменения применяются сразу "+
		"и переживают перезапуск: с этого момента <code>.env</code> для них уже не читается.</i>",
		tgemoji.Icon(tgemoji.IDInfo))

	rows := [][]telego.InlineKeyboardButton{row(killButton(v.PublicDailyLimit))}
	rows = append(rows, pairs(v)...)
	rows = append(rows,
		row(button(tgemoji.IDWrite, "Ввести вручную", screenManual)),
		row(backButton()))
	return b.String(), keys(rows...)
}

// pairs lays the knob buttons out two to a row, in table order.
func pairs(v settings.Values) [][]telego.InlineKeyboardButton {
	rows := make([][]telego.InlineKeyboardButton, 0, (len(knobs)+1)/2)
	for i := 0; i < len(knobs); i += 2 {
		if i+1 == len(knobs) {
			rows = append(rows, row(edit(knobs[i].icon, knobs[i].label(v), knobs[i].name)))
			continue
		}
		rows = append(rows, row(
			edit(knobs[i].icon, knobs[i].label(v), knobs[i].name),
			edit(knobs[i+1].icon, knobs[i+1].label(v), knobs[i+1].name)))
	}
	return rows
}

// manualScreen offers the knobs that take a typed value. It exists because a
// preset list is somebody else's idea of what is sensible: 40 requests a day
// and a three-day ban are perfectly reasonable numbers that no cycle contains.
func (h *Handler) manualScreen() (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Свои значения</b>\n\n", tgemoji.Icon(tgemoji.IDWrite))

	if h.tuner == nil {
		fmt.Fprintf(&b, "Настройки недоступны.")
		return b.String(), keys(row(backButton()))
	}
	v := h.tuner.Get()

	fmt.Fprintf(&b, "Выбери, что менять — потом пришли значение обычным сообщением.\n\n")

	rows := make([][]telego.InlineKeyboardButton, 0, len(knobs)+2)
	for _, k := range knobs {
		if k.parse == nil {
			continue // a switch has nothing to type
		}
		fmt.Fprintf(&b, "%s <b>%s</b> — сейчас <b>%s</b>\n", tgemoji.Icon(k.icon), escape(k.title), escape(valueOf(k, v)))
		rows = append(rows, row(action(k.icon, k.label(v), askPrefix+k.name)))
	}

	fmt.Fprintf(&b, "\n%s <i>Сроки пишутся как <code>30с</code>, <code>15м</code>, <code>2ч</code>, "+
		"<code>7д</code>; голое число — минуты.</i>", tgemoji.Icon(tgemoji.IDInfo))

	rows = append(rows, row(button("", "◁ К настройкам", screenSettings)), row(backButton()))
	return b.String(), keys(rows...)
}

func limitLabel(n int) string {
	if n == 0 {
		return "выкл"
	}
	return strconv.Itoa(n)
}

// killButton is the stop switch, named after what pressing it does rather than
// after the current state — a button that says "включено" is ambiguous about
// whether that is a description or a promise. It is the one button that is
// painted: closing the bot to the public is the press an operator hunts for
// while watching abuse happen.
func killButton(limit int) telego.InlineKeyboardButton {
	if limit > 0 {
		return styled(action(tgemoji.IDLockClosed, "Выключить публику", killPublic), styleDanger)
	}
	return styled(action(tgemoji.IDMegaphone, "Включить публику", killPublic), styleSuccess)
}

func thresholdLabel(n int64) string {
	if n == 0 {
		return "все равны"
	}
	return fmt.Sprintf("≥%d млрд", n/1_000_000_000)
}

func cacheLabel(d time.Duration) string {
	if d <= 0 {
		return "выкл"
	}
	return shortDuration(d)
}

func (h *Handler) helpScreen() (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Справка</b>\n\n", tgemoji.Tag(tgemoji.IDInfo, "ℹ️"))
	fmt.Fprintf(&b, "<b>Команды</b> — работают в любом чате, где бота позвали:\n")
	fmt.Fprintf(&b, "<code>/add id</code> — выдать доступ\n")
	fmt.Fprintf(&b, "<code>/del id</code> — забрать\n")
	fmt.Fprintf(&b, "<code>/list</code> — показать список\n")
	fmt.Fprintf(&b, "<code>/ban id|@username [срок]</code> — забанить; без срока навсегда\n")
	fmt.Fprintf(&b, "<code>/unban id|@username</code> — разбанить\n")
	fmt.Fprintf(&b, "<code>/bans</code> — кто забанен\n\n")
	fmt.Fprintf(&b, "%s Срок: <code>30м</code>, <code>2ч</code>, <code>7д</code>, <code>1нед</code>. "+
		"Голое число — минуты.\n\n", tgemoji.Icon(tgemoji.IDElapsed))
	fmt.Fprintf(&b, "%s Свежие данные бот берёт поиском в интернете сам, когда вопрос того требует. "+
		"Выключается в настройках.\n\n", tgemoji.Icon(tgemoji.IDLink))
	fmt.Fprintf(&b, "<b>Флаг <code>-s</code></b> — первым словом в вопросе снимает домашний стиль ответа:\n")
	fmt.Fprintf(&b, "<blockquote><code>@%s -s распиши подробно, ничего не сокращай</code></blockquote>\n\n",
		h.botUsername)
	fmt.Fprintf(&b, "%s Положительный id — человек, отрицательный — группа или канал.\n\n",
		tgemoji.Tag(tgemoji.IDInfo, "ℹ️"))

	// AGPL asks that a program offered over a network tell its users where the
	// source is. This is that notice, and it is also just useful.
	fmt.Fprintf(&b, "%s Исходники: %s\nЛицензия AGPL-3.0 — пользоваться и продавать можно, "+
		"свой форк обязан остаться открытым.",
		tgemoji.Tag(tgemoji.IDCode, "🔨"), sourceURL)

	return b.String(), keys(row(backButton()))
}

// send posts a menu message, retrying without custom emoji if Telegram refuses
// them: not every bot is allowed to send them, and a plain menu beats none.
func (h *Handler) send(ctx context.Context, chatID int64, text string, keyboard *telego.InlineKeyboardMarkup) error {
	params := &telego.SendMessageParams{
		ChatID:             telego.ChatID{ID: chatID},
		Text:               text,
		ParseMode:          telego.ModeHTML,
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	}
	// ReplyMarkup is an interface: assigning a nil *InlineKeyboardMarkup to it
	// would produce a non-nil interface holding nil, and Telegram would be sent
	// a keyboard field it cannot read.
	if keyboard != nil {
		params.ReplyMarkup = keyboard
	}
	_, err := h.sender.SendMessage(ctx, params)
	if err == nil || !fancy(text, keyboard) {
		return err
	}

	h.log.Warn("menu rejected with custom emoji, retrying plain", "err", err)
	params.Text = tgemoji.Strip(text)
	if keyboard != nil {
		params.ReplyMarkup = plainKeyboard(keyboard)
	}
	_, plainErr := h.sender.SendMessage(ctx, params)
	return plainErr
}

// fancy reports whether a screen asks for anything premium — custom emoji in
// the text, an icon on a button — and is therefore worth a second, plain
// attempt when Telegram refuses it.
func fancy(text string, keyboard *telego.InlineKeyboardMarkup) bool {
	return tgemoji.Has(text) || hasIcons(keyboard)
}

// edit swaps a menu message for another screen, with the same fallback.
func (h *Handler) edit(ctx context.Context, chatID int64, messageID int, text string, keyboard *telego.InlineKeyboardMarkup) error {
	params := &telego.EditMessageTextParams{
		ChatID:             telego.ChatID{ID: chatID},
		MessageID:          messageID,
		Text:               text,
		ParseMode:          telego.ModeHTML,
		ReplyMarkup:        keyboard, // always a real keyboard on a menu screen
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	}
	_, err := h.sender.EditMessageText(ctx, params)
	if err == nil || !fancy(text, keyboard) {
		return err
	}

	h.log.Warn("menu edit rejected with custom emoji, retrying plain", "err", err)
	params.Text = tgemoji.Strip(text)
	params.ReplyMarkup = plainKeyboard(keyboard)
	_, plainErr := h.sender.EditMessageText(ctx, params)
	return plainErr
}

// section appends a titled list, or nothing when the list is empty.
func section(b *strings.Builder, icon, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s <b>%s</b>\n", icon, title)
	for _, line := range lines {
		fmt.Fprintf(b, "%s\n", line)
	}
}

// health picks the icon for the key pool: a pool with nothing free is the one
// state an operator needs to spot at a glance.
func health(ready int) string {
	if ready == 0 {
		return tgemoji.Tag(tgemoji.IDLockClosed, "🔒")
	}
	return tgemoji.Tag(tgemoji.IDLockOpen, "🔓")
}

func answered(s llm.Stats) int {
	total := 0
	for _, m := range s.Models {
		total += m.Answers
	}
	return total
}

func onOff(on bool) string {
	if on {
		return "<b>включены</b>"
	}
	return "<b>выключены</b>"
}

func onOffShort(on bool) string {
	if on {
		return "вкл"
	}
	return "выкл"
}

// escape makes provider text safe to drop into an HTML message. Error bodies
// come from Google and can contain anything.
func escape(s string) string {
	return html.EscapeString(s)
}

// shortDuration renders a duration the way a person would say it.
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч %d мин", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d д %d ч", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// Button styles Telegram renders itself. Unlike the icon, these carry no
// premium condition: an omitted style is simply the app's default.
const (
	styleDanger  = "danger"  // red: closes something, cuts somebody off
	styleSuccess = "success" // green: lets somebody back in
)

// button builds one menu button.
//
// The icon is a real premium emoji: inline-keyboard buttons carry
// icon_custom_emoji_id, which Telegram draws before the label. It is not an
// entity inside the text — the label itself stays plain — and it works in two
// cases: for bots that bought a username on Fragment, and in messages the bot
// sends directly to a private, group or supergroup chat when the bot's owner
// has Premium. The panel is the second case. When neither holds, Telegram
// refuses the whole request, which is what plainKeyboard is for.
func button(icon, label, screen string) telego.InlineKeyboardButton {
	return telego.InlineKeyboardButton{
		Text:              label,
		IconCustomEmojiID: icon,
		CallbackData:      callbackPrefix + screen,
	}
}

// action builds a button that does something to somebody instead of opening a
// screen. The target id travels in the callback data, which is what makes a ban
// button possible without a text field.
func action(icon, label, data string) telego.InlineKeyboardButton {
	return button(icon, label, data)
}

// styled paints a button, for the two presses worth marking: the one that cuts
// people off and the one that lets somebody back in.
func styled(b telego.InlineKeyboardButton, style string) telego.InlineKeyboardButton {
	b.Style = style
	return b
}

// edit builds a button that changes a setting instead of opening a screen.
func edit(icon, label, knob string) telego.InlineKeyboardButton {
	return button(icon, label, editPrefix+knob)
}

func backButton() telego.InlineKeyboardButton {
	return button("", "◁ Назад", screenMain)
}

// plainKeyboard is the same keyboard for a bot that may not use premium icons:
// each icon is folded into its label as the plain emoji it falls back to, so
// nothing is lost but the rendering.
func plainKeyboard(keyboard *telego.InlineKeyboardMarkup) *telego.InlineKeyboardMarkup {
	if keyboard == nil {
		return nil
	}
	rows := make([][]telego.InlineKeyboardButton, 0, len(keyboard.InlineKeyboard))
	for _, source := range keyboard.InlineKeyboard {
		row := make([]telego.InlineKeyboardButton, 0, len(source))
		for _, b := range source {
			if b.IconCustomEmojiID != "" {
				b.Text = tgemoji.Alt(b.IconCustomEmojiID) + " " + b.Text
				b.IconCustomEmojiID = ""
			}
			row = append(row, b)
		}
		rows = append(rows, row)
	}
	return &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// hasIcons reports whether any button asks for a premium icon, which is what
// makes a rejected message worth retrying.
func hasIcons(keyboard *telego.InlineKeyboardMarkup) bool {
	if keyboard == nil {
		return false
	}
	for _, row := range keyboard.InlineKeyboard {
		for _, b := range row {
			if b.IconCustomEmojiID != "" {
				return true
			}
		}
	}
	return false
}

func row(buttons ...telego.InlineKeyboardButton) []telego.InlineKeyboardButton {
	return buttons
}

func keys(rows ...[]telego.InlineKeyboardButton) *telego.InlineKeyboardMarkup {
	return &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

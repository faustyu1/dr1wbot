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

// Rationer is the public daily allowance.
type Rationer interface {
	Enabled() bool
	Limit() int
	Peek(userID int64) (used, limit int)
	Stats() quota.Stats
	Callers(max int) []quota.Caller
	Ban(userID int64, d time.Duration)
	Pardon(userID int64)
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
	screenCallers  = "who"
)

// Action prefixes. An action does something and then redraws a screen, rather
// than only navigating.
const (
	banPrefix    = "ban:"
	pardonPrefix = "par:"
	// killPublic is the stop switch: one press closes the bot to the public
	// without cycling the limit down to zero one step at a time.
	killPublic = "kill"
)

// callersShown bounds the ban list. Telegram caps a keyboard's size, and a
// panel with sixty buttons is not a panel.
const callersShown = 8

// editPrefix marks a button that changes a setting rather than opening a
// screen. The knob's name follows.
const editPrefix = "set:"

// Knob names, one per editable setting.
const (
	knobLimit  = "lim"
	knobBurst  = "brst"
	knobBan    = "ban"
	knobTokens = "tok"
	knobRunes  = "runes"
	knobRaw    = "raw"
	knobGlobal = "glob"
	knobNewAcc = "new"
	knobCache  = "cache"
)

// Choices a button walks through. Inline keyboards have no text input, so every
// number is edited by cycling a short list of values that are actually sensible
// rather than by typing an arbitrary one.
var (
	limitChoices  = []int{0, 10, 30, 50, 100, 300}
	burstChoices  = []int{3, 5, 10, 20}
	banChoices    = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
	tokenChoices  = []int{512, 1024, 2048, 4096}
	promptChoices = []int{500, 1000, 2000, 4000}
	globalChoices = []int{0, 200, 500, 1000, 2000, 5000}
	cacheChoices  = []time.Duration{0, 5 * time.Minute, 10 * time.Minute, time.Hour}
	// newAccountChoices are Telegram id boundaries. Ids are handed out in
	// ascending order, so a boundary is a rough "registered after about then".
	newAccountChoices = []int64{0, 6_000_000_000, 7_000_000_000, 8_000_000_000}
)

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
	command, _, _ := strings.Cut(strings.TrimSpace(msg.Text), " ")
	command, _, _ = strings.Cut(command, "@") // "/admin@dr1wbot" in a group-style client

	switch command {
	case "/start", "/help":
		var keyboard *telego.InlineKeyboardMarkup
		if h.roster.IsAdmin(userID) {
			keyboard = keys(row(button("⚙️ Панель", screenMain)))
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

	case strings.HasPrefix(name, banPrefix):
		if id, ok := parseID(strings.TrimPrefix(name, banPrefix)); ok && h.ration != nil {
			h.ration.Ban(id, 0)
			h.log.Info("banned by hand", "user_id", id, "by", query.From.ID)
		}
		name = screenCallers

	case strings.HasPrefix(name, pardonPrefix):
		if id, ok := parseID(strings.TrimPrefix(name, pardonPrefix)); ok && h.ration != nil {
			h.ration.Pardon(id)
			h.log.Info("pardoned by hand", "user_id", id, "by", query.From.ID)
		}
		name = screenCallers
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
func (h *Handler) turn(knob string) error {
	if h.tuner == nil {
		return nil
	}

	updated, err := h.tuner.Update(func(v *settings.Values) {
		switch knob {
		case knobLimit:
			v.PublicDailyLimit = settings.Cycle(limitChoices, v.PublicDailyLimit)
			if v.PublicDailyLimit > 0 {
				v.PublicOpenLimit = v.PublicDailyLimit
			}
		case knobGlobal:
			v.GlobalDailyLimit = settings.Cycle(globalChoices, v.GlobalDailyLimit)
		case knobNewAcc:
			v.NewAccountThreshold = settings.Cycle(newAccountChoices, v.NewAccountThreshold)
		case knobCache:
			v.CacheTTL = settings.Cycle(cacheChoices, v.CacheTTL)
		case knobBurst:
			v.Burst = settings.Cycle(burstChoices, v.Burst)
		case knobBan:
			v.BanFor = settings.Cycle(banChoices, v.BanFor)
		case knobTokens:
			v.PublicMaxTokens = settings.Cycle(tokenChoices, v.PublicMaxTokens)
		case knobRunes:
			v.PublicMaxRunes = settings.Cycle(promptChoices, v.PublicMaxRunes)
		case knobRaw:
			v.RawFlagEnabled = !v.RawFlagEnabled
		}
	})
	if err != nil {
		return err
	}
	if h.apply != nil {
		h.apply(updated)
	}
	h.log.Info("setting changed", "knob", knob)
	return nil
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

// callersScreen lists today's public users with a button each. This is the only
// way to offer a ban button at all: an inline keyboard has nowhere to type an
// id into, so every actionable person has to be one the bot already knows.
func (h *Handler) callersScreen() (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Кто сегодня писал</b>\n\n", tgemoji.Tag(tgemoji.IDPeople, "👥"))

	if h.ration == nil || !h.ration.Enabled() {
		fmt.Fprintf(&b, "%s Публичный доступ выключен — списка нет.", tgemoji.Tag(tgemoji.IDLockClosed, "🔒"))
		return b.String(), keys(row(backButton()))
	}

	callers := h.ration.Callers(callersShown)
	if len(callers) == 0 {
		fmt.Fprintf(&b, "%s Сегодня никого.", tgemoji.Tag(tgemoji.IDInfo, "ℹ️"))
		return b.String(), keys(row(backButton()))
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(callers)+1)
	for _, c := range callers {
		switch {
		case c.Forever:
			fmt.Fprintf(&b, "%s <code>%d</code> — <b>бан навсегда</b>, варнов %d\n",
				tgemoji.Tag(tgemoji.IDUserNo, "🚫"), c.ID, c.Warns)
			rows = append(rows, row(action(fmt.Sprintf("✅ Разбанить %d", c.ID), pardonPrefix+idText(c.ID))))
		case !c.Until.IsZero():
			fmt.Fprintf(&b, "%s <code>%d</code> — бан ещё <b>%s</b>, варнов %d\n",
				tgemoji.Tag(tgemoji.IDUserNo, "🚫"), c.ID, shortDuration(time.Until(c.Until)), c.Warns)
			rows = append(rows, row(action(fmt.Sprintf("✅ Разбанить %d", c.ID), pardonPrefix+idText(c.ID))))
		default:
			fmt.Fprintf(&b, "%s <code>%d</code> — %d из %d",
				tgemoji.Tag(tgemoji.IDUserOK, "👤"), c.ID, c.Used, c.Limit)
			if c.Warns > 0 {
				fmt.Fprintf(&b, ", варнов %d", c.Warns)
			}
			fmt.Fprintf(&b, "\n")
			rows = append(rows, row(action(fmt.Sprintf("🚫 Забанить %d", c.ID), banPrefix+idText(c.ID))))
		}
	}

	fmt.Fprintf(&b, "\n%s <i>Забаненные сверху. Автоматический бан выдаётся за скорость; "+
		"кнопка — для тех, кто мешает в человеческом темпе.</i>", tgemoji.Tag(tgemoji.IDInfo, "ℹ️"))

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
	case screenCallers:
		return h.callersScreen()
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
		row(button("📊 Лимиты", screenLimits), button("📈 Статистика", screenStats)),
		row(button("👥 Доступ", screenPeople), button("⚙️ Настройки", screenSettings)),
		row(button("🚫 Кто писал", screenCallers), button("ℹ️ Справка", screenHelp)),
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
		row(button("👁 Проверить ключи", screenCheck)),
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
		row(button("🔄 Ещё раз", screenCheck)),
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
	fmt.Fprintf(&b, "%s <b>Настройки</b>\n\n", tgemoji.Tag(tgemoji.IDSettings, "⚙️"))

	models := make([]string, 0, len(stats.Models))
	for _, m := range stats.Models {
		models = append(models, m.Name)
	}
	fmt.Fprintf(&b, "%s Модели: <code>%s</code>\n", tgemoji.Tag(tgemoji.IDCode, "🔨"), strings.Join(models, ", "))
	fmt.Fprintf(&b, "%s Ключей в пуле: <b>%d</b>\n", tgemoji.Tag(tgemoji.IDLockOpen, "🔓"), stats.Keys.Total)

	fmt.Fprintf(&b, "%s Картинки: %s\n", tgemoji.Tag(tgemoji.IDEye, "👁"), onOff(h.imagesOn))
	fmt.Fprintf(&b, "<i>Это из <code>.env</code>: меняется только перезапуском.</i>\n")

	if h.tuner == nil {
		return b.String(), keys(row(backButton()))
	}
	v := h.tuner.Get()

	fmt.Fprintf(&b, "\n%s <b>Публичный доступ</b>\n", tgemoji.Tag(tgemoji.IDMegaphone, "📣"))
	if v.PublicDailyLimit > 0 {
		fmt.Fprintf(&b, "%s Лимит: <b>%d запросов в сутки</b> на человека\n",
			tgemoji.Tag(tgemoji.IDChart, "📊"), v.PublicDailyLimit)
	} else {
		fmt.Fprintf(&b, "%s Выключен — отвечаю только по списку\n", tgemoji.Tag(tgemoji.IDLockClosed, "🔒"))
	}
	fmt.Fprintf(&b, "%s Всплеск: <b>%d за %s</b>, дальше бан\n",
		tgemoji.Tag(tgemoji.IDElapsed, "🕓"), v.Burst, shortDuration(v.BurstWindow))
	fmt.Fprintf(&b, "%s Первый бан: <b>%s</b>, каждый следующий вдвое дольше\n",
		tgemoji.Tag(tgemoji.IDCross, "❌"), shortDuration(v.BanFor))
	if v.GlobalDailyLimit > 0 {
		fmt.Fprintf(&b, "%s Потолок на весь бот: <b>%d запросов в сутки</b>\n",
			tgemoji.Tag(tgemoji.IDGrowth, "📈"), v.GlobalDailyLimit)
	} else {
		fmt.Fprintf(&b, "%s Потолок на весь бот: <b>без ограничения</b>\n",
			tgemoji.Tag(tgemoji.IDGrowth, "📈"))
	}
	if v.NewAccountThreshold > 0 {
		fmt.Fprintf(&b, "%s Свежим аккаунтам (id ≥ %d) — половина лимита\n",
			tgemoji.Tag(tgemoji.IDUserNo, "👤"), v.NewAccountThreshold)
	}
	fmt.Fprintf(&b, "%s Ответ публике: до <b>%d токенов</b>\n",
		tgemoji.Tag(tgemoji.IDWrite, "✍️"), v.PublicMaxTokens)
	fmt.Fprintf(&b, "%s Вопрос публики: до <b>%d символов</b>\n",
		tgemoji.Tag(tgemoji.IDCode, "🔨"), v.PublicMaxRunes)

	fmt.Fprintf(&b, "\n%s <b>Прочее</b>\n", tgemoji.Tag(tgemoji.IDSettings, "⚙️"))
	if v.CacheTTL > 0 {
		fmt.Fprintf(&b, "%s Кэш одинаковых вопросов: <b>%s</b>\n",
			tgemoji.Tag(tgemoji.IDClock, "⏰"), shortDuration(v.CacheTTL))
	} else {
		fmt.Fprintf(&b, "%s Кэш одинаковых вопросов: <b>выключен</b>\n",
			tgemoji.Tag(tgemoji.IDClock, "⏰"))
	}
	fmt.Fprintf(&b, "%s Флаг <code>-s</code> для админов: %s\n",
		tgemoji.Tag(tgemoji.IDSettings, "⚙️"), onOff(v.RawFlagEnabled))

	fmt.Fprintf(&b, "\n%s <i>Кнопки перебирают значения по кругу. Изменения применяются "+
		"сразу и переживают перезапуск — с этого момента <code>.env</code> для них "+
		"уже не читается.</i>", tgemoji.Tag(tgemoji.IDInfo, "ℹ️"))

	return b.String(), keys(
		row(action(killLabel(v.PublicDailyLimit), killPublic)),
		row(edit(fmt.Sprintf("📊 Лимит: %s", limitLabel(v.PublicDailyLimit)), knobLimit),
			edit(fmt.Sprintf("📈 Всего: %s", limitLabel(v.GlobalDailyLimit)), knobGlobal)),
		row(edit(fmt.Sprintf("🕓 Всплеск: %d", v.Burst), knobBurst),
			edit(fmt.Sprintf("❌ Бан: %s", shortDuration(v.BanFor)), knobBan)),
		row(edit(fmt.Sprintf("✍️ Токенов: %d", v.PublicMaxTokens), knobTokens),
			edit(fmt.Sprintf("🔨 Символов: %d", v.PublicMaxRunes), knobRunes)),
		row(edit(fmt.Sprintf("👤 Новые: %s", thresholdLabel(v.NewAccountThreshold)), knobNewAcc),
			edit(fmt.Sprintf("⏰ Кэш: %s", cacheLabel(v.CacheTTL)), knobCache)),
		row(edit(fmt.Sprintf("⚙️ Флаг -s: %s", onOffShort(v.RawFlagEnabled)), knobRaw)),
		row(backButton()),
	)
}

func limitLabel(n int) string {
	if n == 0 {
		return "выкл"
	}
	return strconv.Itoa(n)
}

// killLabel names the stop switch after what pressing it does, not after what
// the current state is — a button that says "включено" is ambiguous about
// whether that is a description or a promise.
func killLabel(limit int) string {
	if limit > 0 {
		return "🛑 Выключить публику"
	}
	return "📣 Включить публику"
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
	fmt.Fprintf(&b, "<code>/list</code> — показать список\n\n")
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
	if err == nil || !tgemoji.Has(text) {
		return err
	}

	h.log.Warn("menu rejected with custom emoji, retrying plain", "err", err)
	params.Text = tgemoji.Strip(text)
	_, plainErr := h.sender.SendMessage(ctx, params)
	return plainErr
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
	if err == nil || !tgemoji.Has(text) {
		return err
	}

	h.log.Warn("menu edit rejected with custom emoji, retrying plain", "err", err)
	params.Text = tgemoji.Strip(text)
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

// button builds one menu button. Button labels are plain text — Telegram allows
// no entities there, so custom emoji stay in the message body.
func button(label, screen string) telego.InlineKeyboardButton {
	return telego.InlineKeyboardButton{Text: label, CallbackData: callbackPrefix + screen}
}

// action builds a button that does something to somebody instead of opening a
// screen. The target id travels in the callback data, which is what makes a ban
// button possible without a text field.
func action(label, data string) telego.InlineKeyboardButton {
	return button(label, data)
}

// edit builds a button that changes a setting instead of opening a screen.
func edit(label, knob string) telego.InlineKeyboardButton {
	return button(label, editPrefix+knob)
}

func backButton() telego.InlineKeyboardButton {
	return button("◁ Назад", screenMain)
}

func row(buttons ...telego.InlineKeyboardButton) []telego.InlineKeyboardButton {
	return buttons
}

func keys(rows ...[]telego.InlineKeyboardButton) *telego.InlineKeyboardMarkup {
	return &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

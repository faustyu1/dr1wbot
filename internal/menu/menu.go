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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"

	"dr1wbot/internal/access"
	"dr1wbot/internal/config"
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

// SessionRestorer loads an archived conversation back as the active one.
// Used by /start deep links (restore_<hash>). Archive saves and clears
// the current conversation, returning a hash for a restore link.
type SessionRestorer interface {
	Restore(userID int64, hash string) error
}

// Tuner is the settings the panel may change.
type Tuner interface {
	Get() settings.Values
	Update(mutate func(*settings.Values)) (settings.Values, error)
	UserPreset(userID int64) string
	SetUserPreset(userID int64, name string) (settings.Values, error)
}

// Prober checks the keys against the live API.
type Prober interface {
	Probe(ctx context.Context) []llm.KeyProbe
}

// ModelPicker can list, test, and switch the active LLM models at runtime.
type ModelPicker interface {
	ListModels(ctx context.Context) ([]string, error)
	TestModel(ctx context.Context, modelID string) error
	ActiveModels() []string
	SetModels(models []string)
}

// PromptSwitcher can report the active system-prompt preset and replace it at
// runtime, so the panel can switch between presets without a restart.
type PromptSwitcher interface {
	SetSystemPrompt(prompt string)
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

	picker ModelPicker

	// switcher can change the system prompt at runtime. Nil hides the preset screen.
	switcher PromptSwitcher

	// restorer loads saved sessions. Nil disables restore deep links.
	restorer SessionRestorer

	// testMu guards testResults cache from concurrent probes.
	testMu      sync.Mutex
	testResults map[string]bool // model → alive
	testWhen    time.Time
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
	// because image generation is billed separately.
	ImagesOn bool
	// Picker lets the panel switch models at runtime. Nil disables the screen.
	Picker ModelPicker
	// Switcher lets the panel switch system-prompt presets at runtime. Nil
	// hides the preset selector.
	Switcher PromptSwitcher

	// StartedAt is when the process came up, which is the window every counter
	// on the stats screen covers.
	StartedAt time.Time

	// Restorer enables /start=restore_<hash> deep links. Nil ignores them.
	Restorer SessionRestorer
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
		picker:      opts.Picker,
		switcher:    opts.Switcher,
		restorer:    opts.Restorer,
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
	screenModels   = "mdl" // model selector screen
	screenPresets  = "pmp" // system-prompt preset selector screen
)

// Action prefixes. An action does something and then redraws a screen, rather
// than only navigating.
const (
	banPrefix    = "ban:"
	pardonPrefix = "par:"
	// killPublic is the stop switch: one press closes the bot to the public
	// without cycling the limit down to zero one step at a time.
	killPublic = "kill"
	// selModelPrefix marks a model-selection button. The model id follows.
	selModelPrefix = "mdl:"
	// testModels triggers a live probe of all models.
	testModels = "tmdl"
	// selPresetPrefix marks a preset-selection button. The preset name follows.
	selPresetPrefix = "psp:"
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
		return true, h.handleStart(ctx, msg)
	case "/admin":
		if !h.roster.IsAdmin(userID) {
			// Saying "not an admin" would confirm the menu exists; the greeting
			// says nothing either way.
			return true, h.send(ctx, msg.Chat.ID, h.greeting(userID), nil)
		}
		text, keyboard := h.screen(screenMain, userID)
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

	case strings.HasPrefix(name, selModelPrefix):
		modelID := strings.TrimPrefix(name, selModelPrefix)
		if h.picker != nil && modelID != "" {
			active := h.picker.ActiveModels()
			if slices.Contains(active, modelID) {
				// Remove from active list.
				next := make([]string, 0, len(active))
				for _, m := range active {
					if m != modelID {
						next = append(next, m)
					}
				}
				if len(next) == 0 {
					next = active // never empty the list
				}
				h.picker.SetModels(next)
			} else {
				// Add to front of the list (primary model).
				h.picker.SetModels(append([]string{modelID}, active...))
			}
			h.log.Info("model selection changed", "model", modelID, "active", h.picker.ActiveModels(), "by", query.From.ID)
		}
		name = screenModels

	case name == testModels:
		h.testAllModels(ctx)
		name = screenModels

	case strings.HasPrefix(name, selPresetPrefix):
		presetName := strings.TrimPrefix(name, selPresetPrefix)
		if h.tuner != nil {
			preset := config.PresetByName(presetName)
			if h.switcher != nil && preset.Name == "default" {
				// default = clear the per-user override; falls back to global.
				h.tuner.SetUserPreset(query.From.ID, "")
			} else {
				h.tuner.SetUserPreset(query.From.ID, preset.Name)
			}
			h.log.Info("system prompt preset changed", "preset", preset.Name, "by", query.From.ID)
		}
		name = screenPresets

	}

	text, keyboard := h.screenContext(ctx, name, query.From.ID)
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
	fmt.Fprintf(&b, "<b>Привет!</b>\n\n")
	if h.imagesOn {
		fmt.Fprintf(&b, "Отвечу на вопрос, разберу картинку или продолжу разговор.\n\n")
	} else {
		fmt.Fprintf(&b, "Отвечу на вопрос или продолжу разговор.\n\n")
	}
	fmt.Fprintf(&b, "В личке пишу как собеседник — с памятью. Кнопка «Новый чат» внизу сбрасывает контекст. /sessions — сохранённые диалоги.\n\n")

	switch {
	case h.roster.IsAdmin(userID):
		fmt.Fprintf(&b, "Ты админ — кнопка «панель» ниже.")
	case h.ration != nil && h.ration.Enabled():
		used, limit := h.ration.Peek(userID)
		fmt.Fprintf(&b, "Сегодня осталось <b>%d</b> из %d запросов. Счётчик обнуляется в полночь UTC.",
			max(limit-used, 0), limit)
	default:
		fmt.Fprintf(&b, "Бот приватный: отвечаю только тем, кто есть в списке доступа.")
	}
	return b.String()
}

// handleStart processes /start (and /help), including deep links of the form
// /start restore_<hash> that restore an archived conversation.
func (h *Handler) handleStart(ctx context.Context, msg telego.Message) error {
	userID := msg.From.ID

	// Parse deep link payload: "/start restore_abc123…"
	parts := strings.Fields(msg.Text)
	if len(parts) >= 2 {
		if hash, ok := strings.CutPrefix(parts[1], "restore_"); ok && h.restorer != nil {
			if err := h.restorer.Restore(userID, hash); err != nil {
				h.log.Warn("restore failed", "err", err, "user_id", userID)
				return h.send(ctx, msg.Chat.ID,
					"Не удалось восстановить сессию. Возможно, она была удалена.", nil)
			}
			var kb *telego.InlineKeyboardMarkup
			if h.roster.IsAdmin(userID) {
				kb = keys(row(button("панель", screenMain)))
			}
			return h.send(ctx, msg.Chat.ID,
				"<b>Сессия восстановлена.</b>\n\nКонтекст загружен — продолжайте писать.",
				kb)
		}
	}

	// Regular /start — show greeting. Admins get a panel button;
	// everyone else just gets text. Session management is handled by
	// the reply-keyboard "Новый чат" button and /sessions command.
	var keyboard *telego.InlineKeyboardMarkup
	if h.roster.IsAdmin(userID) {
		keyboard = keys(row(button("панель", screenMain)))
	}
	return h.send(ctx, msg.Chat.ID, h.greeting(userID), keyboard)
}

// screenContext renders a page that may need to call out to the network.
func (h *Handler) screenContext(ctx context.Context, name string, userID int64) (string, *telego.InlineKeyboardMarkup) {
	if name == screenCheck {
		return h.checkScreen(ctx)
	}
	return h.screen(name, userID)
}

// callersScreen lists today's public users with a button each. This is the only
// way to offer a ban button at all: an inline keyboard has nowhere to type an
// id into, so every actionable person has to be one the bot already knows.
func (h *Handler) callersScreen() (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Кто сегодня писал</b>\n\n")

	if h.ration == nil {
		fmt.Fprintf(&b, "Статистика недоступна.")
		return b.String(), keys(row(backButton()))
	}

	callers := h.ration.Callers(callersShown)
	if len(callers) == 0 {
		fmt.Fprintf(&b, "Сегодня никого.")
		return b.String(), keys(row(backButton()))
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(callers)+1)
	for _, c := range callers {
		switch {
		case c.Forever:
			fmt.Fprintf(&b, "<code>%d</code> — <b>бан навсегда</b>, варнов %d\n",
				c.ID, c.Warns)
			rows = append(rows, row(action(fmt.Sprintf("Разбанить %d", c.ID), pardonPrefix+idText(c.ID))))
		case !c.Until.IsZero():
			fmt.Fprintf(&b, "<code>%d</code> — бан ещё <b>%s</b>, варнов %d\n",
				c.ID, shortDuration(time.Until(c.Until)), c.Warns)
			rows = append(rows, row(action(fmt.Sprintf("Разбанить %d", c.ID), pardonPrefix+idText(c.ID))))
		default:
			fmt.Fprintf(&b, "<code>%d</code> — %d из %d",
				c.ID, c.Used, c.Limit)
			if c.Warns > 0 {
				fmt.Fprintf(&b, ", варнов %d", c.Warns)
			}
			fmt.Fprintf(&b, "\n")
			rows = append(rows, row(action(fmt.Sprintf("Забанить %d", c.ID), banPrefix+idText(c.ID))))
		}
	}

	fmt.Fprintf(&b, "\n<i>Забаненные сверху. Автоматический бан выдаётся за скорость; "+
		"кнопка — для тех, кто мешает в человеческом темпе.</i>")

	rows = append(rows, row(backButton()))
	return b.String(), keys(rows...)
}

func idText(id int64) string { return strconv.FormatInt(id, 10) }

// screen renders one menu page. An unknown name falls back to the main screen,
// which is what an old message with a stale button should do.
func (h *Handler) screen(name string, userID int64) (string, *telego.InlineKeyboardMarkup) {
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
	case screenModels:
		return h.modelsScreen()
	case screenPresets:
		return h.presetsScreen(userID)
	default:
		return h.mainScreen()
	}
}

func (h *Handler) mainScreen() (string, *telego.InlineKeyboardMarkup) {
	stats := h.models.Stats()

	var b strings.Builder
	fmt.Fprintf(&b, "<b>Панель управления</b>\n\n")
	fmt.Fprintf(&b, "Ключи: <b>%d</b> из %d свободны\n",
		stats.Keys.Ready, stats.Keys.Total)
	fmt.Fprintf(&b, "Ответов за сессию: <b>%d</b>\n", answered(stats))
	fmt.Fprintf(&b, "Аптайм: <b>%s</b>", shortDuration(time.Since(h.startedAt)))

	return b.String(), keys(
		row(button("лимиты", screenLimits), button("статистика", screenStats)),
		row(button("доступ", screenPeople), button("настройки", screenSettings)),
		row(button("модели", screenModels), button("промпт", screenPresets)),
		row(button("кто писал", screenCallers), button("справка", screenHelp)),
	)
}

func (h *Handler) limitsScreen() (string, *telego.InlineKeyboardMarkup) {
	stats := h.models.Stats()

	var b strings.Builder
	fmt.Fprintf(&b, "<b>Лимиты и ключи</b>\n\n")

	fmt.Fprintf(&b, "<b>Ключи API</b>\n")
	fmt.Fprintf(&b, "Свободны: <b>%d</b>\n", stats.Keys.Ready)
	fmt.Fprintf(&b, "Остывают: <b>%d</b>\n", stats.Keys.Parked)
	if !stats.Keys.NextReady.IsZero() {
		fmt.Fprintf(&b, "Ближайший вернётся через <b>%s</b>\n",
			shortDuration(time.Until(stats.Keys.NextReady)))
	}

	fmt.Fprintf(&b, "\n<b>Модели</b> (в порядке фолбэка)\n")
	for _, m := range stats.Models {
		fmt.Fprintf(&b, "<code>%s</code> — <b>%d</b>\n", m.Name, m.Answers)
	}

	if stats.QuotaOut > 0 {
		fmt.Fprintf(&b, "\nУпёрлись в лимиты <b>%d</b> раз: ни один ключ не ответил ни на одной модели.\n",
			stats.QuotaOut)
	}

	fmt.Fprintf(&b, "\n<i>Остаток квоты через API не отдаёт — эндпоинта для этого нет. "+
		"Здесь только то, что бот увидел сам с момента запуска. «Проверить ключи» дёргает список "+
		"моделей каждым ключом: это бесплатно и показывает, какие ключи вообще живы.</i>")

	return b.String(), keys(
		row(button("проверить ключи", screenCheck)),
		row(backButton()),
	)
}

// checkScreen asks every key what it can reach. This is the only live question
// the API will answer: there is no quota endpoint, so "which keys work" is as
// close to "what are my limits" as it gets.
func (h *Handler) checkScreen(ctx context.Context) (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Проверка ключей</b>\n\n")

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
			fmt.Fprintf(&b, "Ключ %d — <b>%d моделей</b>\n",
				p.Index+1, p.Models)
			continue
		}
		fmt.Fprintf(&b, "Ключ %d — <code>%s</code>\n",
			p.Index+1, escape(p.Err.Error()))
	}

	fmt.Fprintf(&b, "\nЖивых ключей: <b>%d</b>\n", alive)
	fmt.Fprintf(&b, "\n<i>Сам список моделей квоту не тратит. Ключ, который отвечает здесь, "+
		"всё ещё может упереться в лимит на генерации — это разные счётчики.</i>")

	return b.String(), keys(
		row(button("ещё раз", screenCheck)),
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
	fmt.Fprintf(&b, "<b>Статистика</b>\n\n")
	fmt.Fprintf(&b, "Запросов к модели: <b>%d</b>\n", stats.Requests)
	fmt.Fprintf(&b, "Ответов: <b>%d</b>\n", answered(stats))
	fmt.Fprintf(&b, "Неудач: <b>%d</b>\n", stats.Failures)
	fmt.Fprintf(&b, "Аптайм: <b>%s</b>\n", shortDuration(time.Since(h.startedAt)))

	if h.ration != nil && h.ration.Enabled() {
		q := h.ration.Stats()
		fmt.Fprintf(&b, "\n<b>Публичный доступ сегодня</b>\n")
		fmt.Fprintf(&b, "Людей: <b>%d</b>\n", q.Users)
		fmt.Fprintf(&b, "Запросов: <b>%d</b>\n", q.Requests)
		fmt.Fprintf(&b, "Обнуление через <b>%s</b>", shortDuration(q.ResetsIn))
	}

	return b.String(), keys(row(backButton()))
}

func (h *Handler) peopleScreen() (string, *telego.InlineKeyboardMarkup) {
	entries := h.roster.List()

	var b strings.Builder
	fmt.Fprintf(&b, "<b>Доступ</b>\n\n")

	if len(entries) == 0 {
		fmt.Fprintf(&b, "Список пуст — значит, отвечаю всем подряд.\n")
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

	section(&b, "", "Админы", admins)
	section(&b, "", "Люди", users)
	section(&b, "", "Чаты", chats)

	fmt.Fprintf(&b, "\nМенять список — командами <code>/add id</code> и <code>/del id</code> "+
		"в любом чате. Записи из <code>.env</code> так не убрать.")

	return b.String(), keys(row(backButton()))
}

func (h *Handler) settingsScreen() (string, *telego.InlineKeyboardMarkup) {
	stats := h.models.Stats()

	var b strings.Builder
	fmt.Fprintf(&b, "<b>Настройки</b>\n\n")

	models := make([]string, 0, len(stats.Models))
	for _, m := range stats.Models {
		models = append(models, m.Name)
	}
	fmt.Fprintf(&b, "Модели: <code>%s</code>\n", strings.Join(models, ", "))
	fmt.Fprintf(&b, "Ключей в пуле: <b>%d</b>\n", stats.Keys.Total)

	fmt.Fprintf(&b, "Картинки: %s\n", onOff(h.imagesOn))
	fmt.Fprintf(&b, "<i>Это из <code>.env</code>: меняется только перезапуском.</i>\n")

	if h.tuner == nil {
		return b.String(), keys(row(backButton()))
	}
	v := h.tuner.Get()

	fmt.Fprintf(&b, "\n<b>Публичный доступ</b>\n")
	if v.PublicDailyLimit > 0 {
		fmt.Fprintf(&b, "Лимит: <b>%d запросов в сутки</b> на человека\n",
			v.PublicDailyLimit)
	} else {
		fmt.Fprintf(&b, "Выключен — отвечаю только по списку\n")
	}
	fmt.Fprintf(&b, "Всплеск: <b>%d за %s</b>, дальше бан\n",
		v.Burst, shortDuration(v.BurstWindow))
	fmt.Fprintf(&b, "Первый бан: <b>%s</b>, каждый следующий вдвое дольше\n",
		shortDuration(v.BanFor))
	if v.GlobalDailyLimit > 0 {
		fmt.Fprintf(&b, "Потолок на весь бот: <b>%d запросов в сутки</b>\n",
			v.GlobalDailyLimit)
	} else {
		fmt.Fprintf(&b, "Потолок на весь бот: <b>без ограничения</b>\n")
	}
	if v.NewAccountThreshold > 0 {
		fmt.Fprintf(&b, "Свежим аккаунтам (id ≥ %d) — половина лимита\n",
			v.NewAccountThreshold)
	}
	fmt.Fprintf(&b, "Ответ: до <b>%d токенов</b>\n",
		v.PublicMaxTokens)
	fmt.Fprintf(&b, "Вопрос: до <b>%d символов</b> (для публики)\n",
		v.PublicMaxRunes)

	fmt.Fprintf(&b, "\n<b>Прочее</b>\n")
	if v.CacheTTL > 0 {
		fmt.Fprintf(&b, "Кэш одинаковых вопросов: <b>%s</b>\n",
			shortDuration(v.CacheTTL))
	} else {
		fmt.Fprintf(&b, "Кэш одинаковых вопросов: <b>выключен</b>\n")
	}
	fmt.Fprintf(&b, "Флаг <code>-s</code> для админов: %s\n",
		onOff(v.RawFlagEnabled))

	fmt.Fprintf(&b, "\n<i>Кнопки перебирают значения по кругу. Изменения применяются "+
		"сразу и переживают перезапуск — с этого момента <code>.env</code> для них "+
		"уже не читается.</i>")

	return b.String(), keys(
		row(button("модели", screenModels),
			button("промпт", screenPresets)),
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

// modelsScreen shows available LLM models from the API and lets the admin
// toggle which ones are active. Active models form the fallback chain:
// the first one is tried first, then the next, etc.
func (h *Handler) modelsScreen() (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Модели</b>\n\n")

	if h.picker == nil {
		fmt.Fprintf(&b, "Селектор моделей недоступен.")
		return b.String(), keys(row(backButton()))
	}

	active := h.picker.ActiveModels()
	activeSet := make(map[string]bool, len(active))
	for _, m := range active {
		activeSet[m] = true
	}

	// Fetch the model list from the API.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	available, err := h.picker.ListModels(ctx)
	if err != nil {
		fmt.Fprintf(&b, "Не удалось получить список: <code>%s</code>\n",
			html.EscapeString(err.Error()))
		return b.String(), keys(row(backButton()))
	}

	if len(available) == 0 {
		fmt.Fprintf(&b, "Модели не найдены.")
		return b.String(), keys(row(backButton()))
	}

	slices.Sort(available)

	// Show cached test results if available.
	h.testMu.Lock()
	results := h.testResults
	age := time.Since(h.testWhen)
	h.testMu.Unlock()

	fmt.Fprintf(&b, "Всего: <b>%d</b>", len(available))
	if results != nil && age < 5*time.Minute {
		alive := 0
		for _, ok := range results {
			if ok {
				alive++
			}
		}
		fmt.Fprintf(&b, " | Живых: <b>%d</b>", alive)
	}
	fmt.Fprintf(&b, "\nАктивно в боте: <b>%d</b>\n", len(active))

	// Show active model order.
	for i, m := range active {
		fmt.Fprintf(&b, "%d. <code>%s</code>\n", i+1, m)
	}
	fmt.Fprintf(&b, "\n")

	// Build buttons: one per model showing its toggle state and test status.
	rows := make([][]telego.InlineKeyboardButton, 0, len(available)+3)
	for _, m := range available {
		label := m
		if activeSet[m] {
			order := 1
			for i, am := range active {
				if am == m {
					order = i + 1
					break
				}
			}
			label = fmt.Sprintf("[%d] %s", order, m)
		} else {
			label = "    " + m
		}
		// Mark dead models.
		if results != nil {
			if ok, found := results[m]; found && !ok {
				label = "(мёртв) " + label
			}
		}
		rows = append(rows, row(action(label, selModelPrefix+m)))
	}

	rows = append(rows, row(button("проверить все", testModels)))
	rows = append(rows, row(backButton()))

	return b.String(), keys(rows...)
}

// presetsScreen shows the system-prompt preset selector. The operator picks one
// of the fixed presets defined in config; the change applies immediately.
func (h *Handler) presetsScreen(userID int64) (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Промпт</b>\n\n")

	presets := config.SystemPresets
	active := h.tuner.UserPreset(userID)
	if active == "" {
		active = "default"
	}

	for _, p := range presets {
		mark := "○"
		if p.Name == active {
			mark = "●"
		}
		fmt.Fprintf(&b, "%s <b>%s</b> — %s\n", mark, p.Name, p.Desc)
	}

	fmt.Fprintf(&b, "\n<i>Переключается мгновенно. Пресет «default» — стандартное поведение.</i>")

	rows := make([][]telego.InlineKeyboardButton, 0, len(presets)+1)
	for _, p := range presets {
		var label string
		if p.Name == active {
			label = p.Desc + " ●"
		} else {
			label = p.Desc
		}
		rows = append(rows, row(action(label, selPresetPrefix+p.Name)))
	}
	rows = append(rows, row(backButton()))

	return b.String(), keys(rows...)
}

// testAllModels probes every available model with a minimal request and caches
// the results so the screen can show ✅/❌ without re-testing on every render.
func (h *Handler) testAllModels(ctx context.Context) {
	if h.picker == nil {
		return
	}

	listCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	available, err := h.picker.ListModels(listCtx)
	cancel()
	if err != nil || len(available) == 0 {
		return
	}

	slices.Sort(available)

	// Test each model with a generous timeout. Models are tested sequentially
	// to avoid hammering the API with 20+ concurrent requests.
	results := make(map[string]bool, len(available))
	for _, m := range available {
		testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := h.picker.TestModel(testCtx, m)
		cancel()
		results[m] = (err == nil)
	}

	// Only keep models that actually responded.
	live := make([]string, 0, len(results))
	for m, ok := range results {
		if ok {
			live = append(live, m)
		}
	}

	h.testMu.Lock()
	h.testResults = results
	h.testWhen = time.Now()
	h.testMu.Unlock()

	// Auto-activate the live models, sorted, preserving any current order
	// for models that were already active.
	currentActive := h.picker.ActiveModels()
	activeSet := make(map[string]bool, len(currentActive))
	for _, m := range currentActive {
		activeSet[m] = true
	}

	// New active list: keep currently active ones that are still alive,
	// then append newly discovered live models.
	newActive := make([]string, 0, len(live))
	for _, m := range currentActive {
		if results[m] { // still alive
			newActive = append(newActive, m)
		}
	}
	for _, m := range available {
		if results[m] && !activeSet[m] {
			newActive = append(newActive, m)
		}
	}
	if len(newActive) > 0 {
		h.picker.SetModels(newActive)
	}

	h.log.Info("model probe complete",
		"total", len(available),
		"alive", len(live),
		"active", h.picker.ActiveModels())
}

func (h *Handler) helpScreen() (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Справка</b>\n\n")
	fmt.Fprintf(&b, "<b>В личке с ботом:</b>\n")
	fmt.Fprintf(&b, "Кнопка «Новый чат» внизу — сбросить контекст, старый диалог сохранится\n")
	fmt.Fprintf(&b, "<code>/sessions</code> — список сохранённых сессий\n")
	fmt.Fprintf(&b, "<code>/clearcache</code> — сбросить кэш одинаковых вопросов\n\n")
	fmt.Fprintf(&b, "<b>Команды доступа</b> — работают везде, где бота позвали:\n")
	fmt.Fprintf(&b, "<code>/add id</code> — выдать доступ\n")
	fmt.Fprintf(&b, "<code>/del id</code> — забрать\n")
	fmt.Fprintf(&b, "<code>/list</code> — показать список\n\n")
	fmt.Fprintf(&b, "<b>Флаг <code>-s</code></b> — первым словом в вопросе снимает домашний стиль:\n")
	fmt.Fprintf(&b, "<blockquote><code>@%s -s распиши подробно</code></blockquote>\n\n",
		h.botUsername)
	fmt.Fprintf(&b, "Положительный id — человек, отрицательный — группа или канал.\n")

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
	if keyboard != nil {
		params.ReplyMarkup = keyboard
	} else {
		// No inline buttons — show the persistent "Новый чат" reply keyboard.
		params.ReplyMarkup = &telego.ReplyKeyboardMarkup{
			Keyboard:       [][]telego.KeyboardButton{{telego.KeyboardButton{Text: "Новый чат"}}},
			ResizeKeyboard: true,
		}
	}
	_, err := h.sender.SendMessage(ctx, params)
	if err == nil {
		return nil
	}
	// Retry on custom-emoji rejection.  Telegram sometimes returns
	// CUSTOM_EMOJI_INVALID even when our markup has been stripped, so we
	// check both the error string and the content.
	if !tgemoji.Has(text) && !strings.Contains(err.Error(), "CUSTOM_EMOJI_INVALID") {
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
	if err == nil {
		return nil
	}
	if !tgemoji.Has(text) && !strings.Contains(err.Error(), "CUSTOM_EMOJI_INVALID") {
		return err
	}

	h.log.Warn("menu edit rejected with custom emoji, retrying plain", "err", err)
	params.Text = tgemoji.Strip(text)
	_, plainErr := h.sender.EditMessageText(ctx, params)
	return plainErr
}

// section appends a titled list, or nothing when the list is empty.
func section(b *strings.Builder, _ string, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "\n<b>%s</b>\n", title)
	for _, line := range lines {
		fmt.Fprintf(b, "%s\n", line)
	}
}

// health picks the icon for the key pool: a pool with nothing free is the one
// state an operator needs to spot at a glance.
func health(ready int) string {
	if ready == 0 {
		return ""
	}
	return ""
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
// come from the model and can contain anything.
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
	return button("назад", screenMain)
}

func row(buttons ...telego.InlineKeyboardButton) []telego.InlineKeyboardButton {
	return buttons
}

func keys(rows ...[]telego.InlineKeyboardButton) *telego.InlineKeyboardMarkup {
	return &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

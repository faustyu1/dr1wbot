// Package reply orchestrates a single guest-mode answer.
//
// The shape of the flow is dictated by two facts about guest mode: the bot may
// answer a guest query exactly once, and that answer is an inline message whose
// id we get back. So we answer immediately with a placeholder — well inside
// whatever deadline Telegram puts on a guest query — and then edit that inline
// message with the result once it arrives.
package reply

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"

	"dr1wbot/internal/alert"
	"dr1wbot/internal/answers"
	"dr1wbot/internal/imagegen"
	"dr1wbot/internal/intent"
	"dr1wbot/internal/llm"
	"dr1wbot/internal/mdtext"
	"dr1wbot/internal/memory"
	"dr1wbot/internal/quota"
	"dr1wbot/internal/tgemoji"
)

// Sender is the slice of the Telegram API this package needs. *telego.Bot
// satisfies it; tests supply a fake.
type Sender interface {
	AnswerGuestQuery(ctx context.Context, params *telego.AnswerGuestQueryParams) (*telego.SentGuestMessage, error)
	EditMessageText(ctx context.Context, params *telego.EditMessageTextParams) (*telego.Message, error)
	SendPhoto(ctx context.Context, params *telego.SendPhotoParams) (*telego.Message, error)
	EditMessageMedia(ctx context.Context, params *telego.EditMessageMediaParams) (*telego.Message, error)
	// SendMessage serves the private chat, which is not guest mode: there is
	// no query to answer once, so a reply is simply sent.
	SendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error)
}

// Gatekeeper decides who may summon the bot and who runs it.
type Gatekeeper interface {
	Allowed(userID, chatID int64) bool
	IsAdmin(userID int64) bool
}

// Downloader fetches an attachment by file_id.
type Downloader interface {
	Download(ctx context.Context, fileID string) ([]byte, error)
}

// Remembrancer keeps the short conversation history guest mode does not give us.
type Remembrancer interface {
	History(chatID int64) []memory.Turn
	Remember(chatID int64, question, answer string)
	Forget(chatID int64)
}

// Rationer meters people who are not on the whitelist. A nil one keeps the bot
// private: everybody outside the list is ignored, as before.
type Rationer interface {
	Enabled() bool
	Judge(userID int64) (verdict quota.Verdict, used, limit int)
}

// Cache serves repeated questions without going to the model.
type Cache interface {
	Enabled() bool
	Get(key string) (string, bool)
	Put(key, answer string)
}

// Notifier tells the admins about things they would otherwise only find by
// opening the panel.
type Notifier interface {
	Notify(ctx context.Context, kind, text string)
}

// Commander handles admin commands. Handle reports false when the text is an
// ordinary question that should go to the model instead.
type Commander interface {
	Handle(text string, callerID int64) (reply string, handled bool)
}

// User-facing strings. Kept together so the bot's voice stays consistent.
const (
	msgEmptyPrompt   = "Позови меня с вопросом — или ответь мной на сообщение, которое нужно разобрать."
	promptForPicture = "Разбери, что на изображении."
	msgEmptyDrawing  = "Опиши, что нарисовать. Например: `нарисуй кота в скафандре`."
	msgBusy          = "⚠️ Сейчас слишком много запросов. Попробуй ещё раз через минуту."
	msgTimeout       = "⚠️ Модель не ответила вовремя. Попробуй ещё раз."
	msgFailed        = "⚠️ Не получилось получить ответ от модели."
	msgRateLimited   = "⚠️ Лимиты исчерпаны на всех ключах и моделях. " +
		"Снять лимит — добавить ещё ключ в OPENAI_API_KEYS или включить биллинг."
	// msgCommandExpired is what an admin command's answer decays into. Guest
	// answers are inline messages, and the Bot API cannot delete those — only
	// edit them — so the closest thing to cleaning up after a command is
	// shrinking its output to a single character.
	msgCommandExpired = "✓"
	msgImagesOff      = "⚠️ Генерация картинок выключена."
	// msgDrawingIsPrivate keeps the paid path off the public allowance: a
	// picture costs real money, a question costs quota.
	msgDrawingIsPrivate = "⚠️ Рисовать могу только тем, кто есть в списке доступа."
	msgTooLong          = "⚠️ Слишком длинный вопрос. Сократи — так я отвечу быстрее и точнее."
	msgAlreadyBusy      = "⚠️ Я ещё думаю над твоим прошлым вопросом. Дождись ответа."
	msgBotExhausted     = "⚠️ На сегодня бот исчерпал общий дневной лимит. Возвращайся после полуночи UTC."
	msgImageFailed      = "⚠️ Не получилось нарисовать. Модель могла отказаться от такого запроса."
	msgImageTimeout     = "⚠️ Картинка рисовалась слишком долго. Попробуй ещё раз."
	msgImageRejected    = "⚠️ Картинка сгенерировалась, но Telegram её не принял."
	msgNoCredits        = "⚠️ Нечем оплатить картинку: генерация картинок платная. " +
		"Нужен ключ от проекта с включённым биллингом."
)

// msgQuotaSpent tells a public user their day is over. The number is in the
// text because "you hit the limit" without it is the most annoying possible
// version of this message.
func msgQuotaSpent(limit int) string {
	return fmt.Sprintf("⚠️ На сегодня всё: %d запросов в сутки на человека. "+
		"Счётчик обнуляется в полночь UTC.", limit)
}

// captionLimit is Telegram's cap on a photo caption.
const captionLimit = 1024

// Handler answers guest queries.
type Handler struct {
	sender      Sender
	model       llm.Completer
	images      imagegen.Generator // nil disables picture generation
	files       Downloader         // nil disables reading attachments
	recall      Remembrancer       // nil disables conversation history
	allow       Gatekeeper
	ration      Rationer // nil keeps the bot whitelist-only
	cache       Cache    // nil disables the repeat-question cache
	alerts      Notifier // nil disables admin alerts
	commands    Commander
	log         *slog.Logger
	sem         chan struct{}
	placeholder telego.InputRichMessage
	// after schedules the delayed cleanup. It is a field so tests can run it
	// without waiting.
	after func(time.Duration, func())

	// policy guards the knobs the admin panel can change while requests are in
	// flight.
	policy sync.RWMutex

	// gate guards the fairness bookkeeping: who is already being served, and
	// how many are waiting for a slot.
	gate     sync.Mutex
	inflight map[int64]struct{}
	waiting  int
	maxQueue int

	rawFlagOff      bool
	botID           int64
	botUsername     string
	storageChatID   int64
	maxReplyRunes   int
	timeout         time.Duration
	imageTimeout    time.Duration
	commandReplyTTL time.Duration
	rawSystemPrompt string
	publicMaxTokens int
	publicMaxRunes  int
}

// Options configures a Handler.
type Options struct {
	Sender   Sender
	Model    llm.Completer
	Images   imagegen.Generator
	Files    Downloader
	Memory   Remembrancer
	Access   Gatekeeper
	Quota    Rationer
	Cache    Cache
	Alerts   Notifier
	Commands Commander
	Logger   *slog.Logger

	// BotID identifies our own messages, so a reply to one reads as a follow-up.
	BotID       int64
	BotUsername string
	// StorageChatID is where generated pictures are uploaded to get a file_id.
	StorageChatID int64
	MaxConcurrent int
	MaxReplyRunes int
	Timeout       time.Duration
	ImageTimeout  time.Duration
	// CommandReplyTTL is how long the answer to /add, /del or /list stays in
	// full before it is shrunk to a marker. Zero leaves it standing.
	CommandReplyTTL time.Duration
	// RawSystemPrompt replaces the house style when an admin prefixes a
	// question with mdtext.RawFlag. Empty disables the flag entirely.
	RawSystemPrompt string
	// PublicMaxTokens caps what a non-whitelisted question may spend on the
	// answer. Zero leaves them on the same budget as everybody else.
	PublicMaxTokens int
	// PublicMaxRunes caps how long a non-whitelisted question may be. Input is
	// billed too, and a pasted book is not a chat question.
	PublicMaxRunes int
	// MaxQueue bounds how many summons may wait for a worker. Zero means an
	// unbounded line.
	MaxQueue int
}

// New builds a Handler.
func New(opts Options) *Handler {
	if opts.MaxConcurrent < 1 {
		opts.MaxConcurrent = 1
	}
	if opts.ImageTimeout <= 0 {
		opts.ImageTimeout = 2 * time.Minute
	}
	return &Handler{
		after:         func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		sender:        opts.Sender,
		model:         opts.Model,
		images:        opts.Images,
		files:         opts.Files,
		recall:        opts.Memory,
		allow:         opts.Access,
		ration:        opts.Quota,
		cache:         opts.Cache,
		alerts:        opts.Alerts,
		commands:      opts.Commands,
		log:           opts.Logger,
		sem:           make(chan struct{}, opts.MaxConcurrent),
		placeholder:   placeholderMessage(),
		botID:         opts.BotID,
		botUsername:   opts.BotUsername,
		storageChatID: opts.StorageChatID,
		maxReplyRunes: opts.MaxReplyRunes,
		timeout:       opts.Timeout,
		imageTimeout:  opts.ImageTimeout,

		commandReplyTTL: opts.CommandReplyTTL,
		rawSystemPrompt: opts.RawSystemPrompt,
		publicMaxTokens: opts.PublicMaxTokens,
		publicMaxRunes:  opts.PublicMaxRunes,
		maxQueue:        opts.MaxQueue,
		inflight:        make(map[int64]struct{}),
	}
}

// SetPublicPolicy replaces the public caps at runtime. Zero on either one
// restores "same as everybody else".
func (h *Handler) SetPublicPolicy(maxTokens, maxRunes int) {
	h.policy.Lock()
	defer h.policy.Unlock()
	h.publicMaxTokens = maxTokens
	h.publicMaxRunes = maxRunes
}

// SetRawFlagEnabled turns the admin "-s" flag on and off without a restart.
// Disabling it keeps the configured prompt so the switch is reversible.
func (h *Handler) SetRawFlagEnabled(on bool) {
	h.policy.Lock()
	defer h.policy.Unlock()
	h.rawFlagOff = !on
}

// publicCaps reads the caps under the lock.
func (h *Handler) publicCaps() (maxTokens, maxRunes int) {
	h.policy.RLock()
	defer h.policy.RUnlock()
	return h.publicMaxTokens, h.publicMaxRunes
}

// placeholderMessage builds the "working on it" message. A custom emoji has to
// go through HTML, because the rich-message Markdown dialect has no syntax for
// one.
func placeholderMessage() telego.InputRichMessage {
	return telego.InputRichMessage{HTML: tgemoji.Placeholder()}
}

// HandleGuestMessage serves one summon. It returns an error only when something
// went wrong that the operator should see in the logs; the user always gets
// either an answer or an explanation in the chat.
func (h *Handler) HandleGuestMessage(ctx context.Context, msg telego.Message) error {
	if msg.GuestQueryID == "" {
		return nil // not a guest query; nothing we can answer
	}

	var userID int64
	if msg.From != nil {
		userID = msg.From.ID
	}
	log := h.log.With("chat_id", msg.Chat.ID, "user_id", userID)

	privileged := h.allow.Allowed(userID, msg.Chat.ID)
	if !privileged && !h.publicOpen() {
		// Stay silent rather than announcing the whitelist to strangers.
		log.Debug("summon rejected: not whitelisted")
		return nil
	}

	question := strippedText(msg, h.botUsername)

	// Commands are answered from local state, so they skip the model entirely:
	// no spend, no placeholder, instant reply.
	if h.commands != nil {
		if answer, handled := h.commands.Handle(question, userID); handled {
			log.Info("command handled", "command", question)
			inlineID, err := h.answer(ctx, msg.GuestQueryID, markdown(answer))
			if err != nil {
				return err
			}
			h.expireCommandReply(ctx, log, inlineID)
			return nil
		}
	}

	question, raw := h.rawRequested(question, userID)

	if prompt, kind := intent.Detect(question); kind == intent.Image {
		if !privileged {
			_, err := h.answer(ctx, msg.GuestQueryID, markdown(msgDrawingIsPrivate))
			return err
		}
		return h.drawPicture(ctx, log, msg.GuestQueryID, prompt)
	}

	return h.answerQuestion(ctx, log, msg, question, raw, privileged)
}

// alert forwards news to the admins when a notifier is configured.
func (h *Handler) alert(ctx context.Context, kind, text string) {
	if h.alerts == nil {
		return
	}
	h.alerts.Notify(ctx, kind, text)
}

// publicOpen reports whether people outside the whitelist are served at all.
func (h *Handler) publicOpen() bool {
	return h.ration != nil && h.ration.Enabled()
}

// rawRequested pulls the raw flag off an admin's question. For everyone else
// the flag is left in the text: it then reads as part of the question, which is
// both harmless and the quietest way to say no.
func (h *Handler) rawRequested(question string, userID int64) (string, bool) {
	h.policy.RLock()
	off := h.rawFlagOff
	h.policy.RUnlock()

	if h.rawSystemPrompt == "" || off {
		return question, false
	}
	rest, raw := mdtext.StripRawFlag(question)
	if !raw || !h.allow.IsAdmin(userID) {
		return question, false
	}
	return rest, true
}

// HandleDirectMessage answers an ordinary private-chat message. The private
// chat is not guest mode: there is no query to answer once and no inline
// message to edit, so the reply is simply sent. Everything else — the
// whitelist, the allowance, the caps, the cache — is the same, because a
// private chat is not a way around any of it.
func (h *Handler) HandleDirectMessage(ctx context.Context, msg telego.Message) error {
	if msg.From == nil || strings.TrimSpace(msg.Text) == "" {
		return nil
	}
	userID := msg.From.ID
	log := h.log.With("chat_id", msg.Chat.ID, "user_id", userID, "direct", true)

	privileged := h.allow.Allowed(userID, msg.Chat.ID)
	if !privileged && !h.publicOpen() {
		log.Debug("direct message rejected: not whitelisted")
		return nil
	}

	question := strings.TrimSpace(msg.Text)
	if h.commands != nil {
		if answer, ok := h.commands.Handle(question, userID); ok {
			return h.sendPlain(ctx, msg.Chat.ID, answer)
		}
	}

	question, raw := h.rawRequested(question, userID)
	maxTokens, maxRunes := h.publicCaps()

	// Claim the in-flight slot before charging quota, so a rejected double-tap
	// costs nothing.
	done, free := h.claim(userID)
	if !free {
		return h.sendPlain(ctx, msg.Chat.ID, msgAlreadyBusy)
	}
	defer done()

	var budget int
	if !privileged {
		if maxRunes > 0 && len([]rune(question)) > maxRunes {
			return h.sendPlain(ctx, msg.Chat.ID, msgTooLong)
		}
		switch verdict, _, limit := h.ration.Judge(userID); verdict {
		case quota.Banned:
			log.Warn("ignoring a caller flagged for abuse")
			return nil
		case quota.Exhausted:
			return h.sendPlain(ctx, msg.Chat.ID, msgBotExhausted)
		case quota.Spent:
			return h.sendPlain(ctx, msg.Chat.ID, msgQuotaSpent(limit))
		}
		budget = maxTokens
	}

	// The same placeholder the guest path shows, for the same reason: an answer
	// takes seconds, and silence for seconds reads as a broken bot.
	messageID, err := h.sendPlaceholder(ctx, msg.Chat.ID)
	if err != nil {
		return err
	}

	release, ok := h.acquire(ctx)
	if !ok {
		return h.editChatText(ctx, msg.Chat.ID, messageID, msgBusy)
	}
	defer release()

	llmCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	system := ""
	if raw {
		system = h.rawSystemPrompt
	}

	// A private chat is a real conversation, so every message continues it —
	// there is no reply-to signal to wait for the way guest mode needs one.
	started := time.Now()
	answer, err := h.model.Complete(llmCtx, llm.Request{
		Prompt:    question,
		History:   h.history(msg.Chat.ID, true),
		System:    system,
		MaxTokens: budget,
	})
	if err != nil {
		log.Error("llm call failed", "err", err, "took", time.Since(started))
		text := msgFailed
		switch {
		case errors.Is(err, llm.ErrRateLimited):
			text = msgRateLimited
			h.alert(ctx, alert.KindNoKeys,
				"🔑 <b>Ключи кончились</b>\nЛимиты выбраны на всех ключах и всех моделях. "+
					"Подробности в /admin → Лимиты.")
		case errors.Is(err, context.DeadlineExceeded):
			text = msgTimeout
		}
		return h.editChatText(ctx, msg.Chat.ID, messageID, text)
	}
	log.Info("answered", "took", time.Since(started), "runes", len([]rune(answer)), "raw", raw)

	if h.recall != nil {
		h.recall.Remember(msg.Chat.ID, question, answer)
	}

	return h.editChatText(ctx, msg.Chat.ID, messageID, mdtext.Truncate(answer, h.maxReplyRunes))
}

// sendPlaceholder posts the "working on it" marker and returns its id, so the
// answer can replace it in place.
func (h *Handler) sendPlaceholder(ctx context.Context, chatID int64) (int, error) {
	sent, err := h.sender.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		Text:      tgemoji.Placeholder(),
		ParseMode: telego.ModeHTML,
	})
	if err == nil {
		return sent.MessageID, nil
	}

	// A bot that may not send custom emoji still needs a placeholder.
	h.log.Warn("custom placeholder rejected, falling back to a plain one", "err", err)
	sent, plainErr := h.sender.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID},
		Text:   tgemoji.PlaceholderPlain(),
	})
	if plainErr != nil {
		return 0, errors.Join(err, plainErr)
	}
	return sent.MessageID, nil
}

// editChatText replaces a private-chat message with text.
//
// It goes through a rich message, the same as the guest path: that dialect
// carries headings, tables and fenced code, which is what the model actually
// emits and what the legacy Markdown parse mode cannot render. A rejected edit
// is retried as unformatted text, so the content always arrives.
func (h *Handler) editChatText(parent context.Context, chatID int64, messageID int, text string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), editTimeout)
	defer cancel()

	_, err := h.sender.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:      telego.ChatID{ID: chatID},
		MessageID:   messageID,
		RichMessage: &telego.InputRichMessage{Markdown: text},
	})
	if err == nil {
		return nil
	}

	h.log.Warn("rich edit rejected in a private chat, retrying as plain text", "err", err)
	_, plainErr := h.sender.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:             telego.ChatID{ID: chatID},
		MessageID:          messageID,
		Text:               text,
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	})
	if plainErr != nil {
		return errors.Join(err, plainErr)
	}
	return nil
}

// sendPlain posts a short notice into a private chat — a refusal or a limit,
// never a model answer. Those are plain sentences by design, so they need no
// formatting and no placeholder to replace.
func (h *Handler) sendPlain(ctx context.Context, chatID int64, text string) error {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), editTimeout)
	defer cancel()

	_, err := h.sender.SendMessage(sendCtx, &telego.SendMessageParams{
		ChatID:             telego.ChatID{ID: chatID},
		Text:               text,
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	})
	return err
}

// expireCommandReply shrinks a command's answer after CommandReplyTTL. A
// whitelist dump is noise once its reader has seen it, and in a group it is
// noise everybody else keeps scrolling past.
//
// The edit runs on a context detached from the handler's, which is already gone
// by then. A shutdown in the meantime simply leaves the answer standing —
// nothing is lost, and there is nothing to clean up on the next start.
func (h *Handler) expireCommandReply(ctx context.Context, log *slog.Logger, inlineMessageID string) {
	if h.commandReplyTTL <= 0 || inlineMessageID == "" {
		return
	}
	detached := context.WithoutCancel(ctx)
	h.after(h.commandReplyTTL, func() {
		if err := h.editText(detached, inlineMessageID, msgCommandExpired); err != nil {
			log.Warn("could not shrink the command reply", "err", err)
		}
	})
}

// answerQuestion is the text path: placeholder, model, edit.
func (h *Handler) answerQuestion(ctx context.Context, log *slog.Logger, msg telego.Message, question string, raw, privileged bool) error {
	// Replying to one of our answers is the only signal that this is the same
	// conversation. A fresh summon is a fresh topic even in the same chat, so it
	// starts from nothing rather than inheriting whatever was discussed before.
	followUp := h.isOwnMessage(msg.ReplyToMessage)

	prompt := mdtext.BuildPrompt(
		question,
		strings.TrimSpace(messageText(msg.ReplyToMessage)),
		followUp,
	)

	var userID int64
	if msg.From != nil {
		userID = msg.From.ID
	}
	maxTokens, maxRunes := h.publicCaps()

	// budget is left at zero for privileged callers, which means "use the
	// configured maximum".
	var budget int

	// A picture is the most expensive thing that can be attached to a question:
	// one photo is worth a page of text in input tokens. Public callers get the
	// text path only.
	var attachments [][]byte
	if privileged {
		attachments = h.attachments(ctx, log, msg)
	}

	switch {
	case prompt == "" && len(attachments) > 0:
		// A bare picture is a complete request on its own.
		prompt = promptForPicture
	case prompt == "":
		_, err := h.answer(ctx, msg.GuestQueryID, markdown(msgEmptyPrompt))
		return err
	}

	// One question at a time per person, checked before the allowance so a
	// double-tap costs nothing — not a quota point, not a placeholder.
	done, free := h.claim(userID)
	if !free {
		log.Info("caller already has a question in flight")
		_, err := h.answer(ctx, msg.GuestQueryID, markdown(msgAlreadyBusy))
		return err
	}
	defer done()

	// The allowance is spent only once there is a real question to answer, so a
	// command, an empty summon or a rejected one costs a stranger nothing.
	if !privileged {
		// Length is checked before the allowance, so a rejected wall of text
		// costs the sender nothing but also buys them nothing.
		if maxRunes > 0 && len([]rune(prompt)) > maxRunes {
			log.Info("public question too long", "runes", len([]rune(prompt)), "max", maxRunes)
			_, err := h.answer(ctx, msg.GuestQueryID, markdown(msgTooLong))
			return err
		}

		switch verdict, used, limit := h.ration.Judge(userID); verdict {
		case quota.Banned:
			// Silence, deliberately. A reply is the feedback a script is
			// looking for, and sending one is a request we would be making to
			// Telegram on a bot's behalf.
			log.Warn("ignoring a caller flagged for abuse")
			h.alert(ctx, alert.KindBan, fmt.Sprintf(
				"🚫 <b>Бан за флуд</b>\nПользователь <code>%d</code> отправляет запросы быстрее человека "+
					"и отключён. Снять: <code>/unban %d</code>", userID, userID))
			return nil
		case quota.Exhausted:
			log.Warn("the bot's own daily budget is gone")
			h.alert(ctx, alert.KindExhausted,
				"🛑 <b>Дневной потолок бота исчерпан</b>\nПублика больше не обслуживается до полуночи UTC. "+
					"Поднять лимит или выключить публичный доступ — в /admin → Настройки.")
			_, err := h.answer(ctx, msg.GuestQueryID, markdown(msgBotExhausted))
			return err
		case quota.Spent:
			log.Info("public allowance spent", "limit", limit)
			_, err := h.answer(ctx, msg.GuestQueryID, markdown(msgQuotaSpent(limit)))
			return err
		default:
			log = log.With("public", true, "used", used, "limit", limit)
		}

		// Public answers are capped tighter than an admin's: output tokens are
		// the expensive half, and a stranger's question rarely needs an essay.
		budget = maxTokens
	}
	inlineID, err := h.answer(ctx, msg.GuestQueryID, h.placeholder)
	if err != nil {
		return err // no placeholder means no message to edit; give up
	}

	release, ok := h.acquire(ctx)
	if !ok {
		log.Warn("queue full, rejecting summon")
		return h.editText(ctx, inlineID, msgBusy)
	}
	defer release()

	llmCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	system := ""
	if raw {
		system = h.rawSystemPrompt
	}

	// Only a question that stands entirely on its own can be cached: a
	// follow-up depends on a conversation, and a picture makes two identical
	// texts two different questions.
	cacheable := h.cache != nil && h.cache.Enabled() && !followUp && len(attachments) == 0
	var cacheKey string
	if cacheable {
		cacheKey = answers.Key(system, prompt)
		if cached, hit := h.cache.Get(cacheKey); hit {
			log.Info("answered from cache", "runes", len([]rune(cached)))
			if h.recall != nil {
				h.recall.Forget(msg.Chat.ID)
				h.recall.Remember(msg.Chat.ID, attribute(msg, prompt), cached)
			}
			return h.editText(ctx, inlineID, mdtext.Truncate(cached, h.maxReplyRunes))
		}
	}

	started := time.Now()
	answer, err := h.model.Complete(llmCtx, llm.Request{
		Prompt:    prompt,
		Images:    attachments,
		History:   h.history(msg.Chat.ID, followUp),
		System:    system,
		MaxTokens: budget,
	})
	if err != nil {
		if errors.Is(err, llm.ErrRateLimited) {
			h.alert(ctx, alert.KindNoKeys,
				"🔑 <b>Ключи кончились</b>\nЛимиты выбраны на всех ключах и всех моделях — "+
					"бот отвечать не может. Подробности в /admin → Лимиты.")
		}
		log.Error("llm call failed", "err", err, "took", time.Since(started))
		text := msgFailed
		switch {
		case errors.Is(err, llm.ErrRateLimited):
			text = msgRateLimited
		case errors.Is(err, context.DeadlineExceeded):
			text = msgTimeout
		}
		return h.editText(ctx, inlineID, text)
	}
	log.Info("answered", "took", time.Since(started), "runes", len([]rune(answer)),
		"follow_up", followUp, "raw", raw, "pictures", len(attachments))
	if cacheable {
		h.cache.Put(cacheKey, answer)
	}
	if h.recall != nil {
		// Dropping the old thread here rather than before the call means a
		// failed answer leaves the previous conversation intact.
		if !followUp {
			h.recall.Forget(msg.Chat.ID)
		}
		h.recall.Remember(msg.Chat.ID, attribute(msg, prompt), answer)
	}

	return h.editText(ctx, inlineID, mdtext.Truncate(answer, h.maxReplyRunes))
}

// history returns what we remember of this conversation. A summon that is not a
// reply to us gets none: in a group chat the previous exchange is usually
// somebody else's, and dragging it in makes the answer talk about the wrong
// thing.
func (h *Handler) history(chatID int64, followUp bool) []llm.Turn {
	if h.recall == nil || !followUp {
		return nil
	}
	remembered := h.recall.History(chatID)
	turns := make([]llm.Turn, 0, len(remembered))
	for _, t := range remembered {
		turns = append(turns, llm.Turn{Role: t.Role, Content: t.Content})
	}
	return turns
}

// attribute prefixes a remembered question with who asked it. In a group the
// follow-up usually comes from someone other than the original asker, so
// without a name the model cannot tell one participant from another.
func attribute(msg telego.Message, prompt string) string {
	name := speaker(msg)
	if name == "" || msg.Chat.Type == telego.ChatTypePrivate {
		return prompt
	}
	return name + ": " + prompt
}

// speaker is the best display name we have for the sender.
func speaker(msg telego.Message) string {
	if msg.From == nil {
		return ""
	}
	switch {
	case msg.From.FirstName != "":
		return msg.From.FirstName
	case msg.From.Username != "":
		return "@" + msg.From.Username
	default:
		return ""
	}
}

// isOwnMessage reports whether we wrote the quoted message. Guest replies come
// back with guest_bot_caller_user set, so both that and a plain sender check
// are needed to recognise ourselves.
func (h *Handler) isOwnMessage(msg *telego.Message) bool {
	if msg == nil {
		return false
	}
	if msg.GuestBotCallerUser != nil {
		return true
	}
	return h.botID != 0 && msg.From != nil && msg.From.ID == h.botID
}

// attachments downloads the pictures that came with the summon, looking both at
// the summoning message and at whatever it replied to — those are the only two
// messages guest mode ever shows us. A picture that fails to download is
// skipped rather than fatal: answering about the text alone beats not
// answering.
func (h *Handler) attachments(ctx context.Context, log *slog.Logger, msg telego.Message) [][]byte {
	if h.files == nil {
		return nil
	}

	var out [][]byte
	for _, id := range photoFileIDs(&msg, msg.ReplyToMessage) {
		data, err := h.files.Download(ctx, id)
		if err != nil {
			log.Warn("could not download an attachment", "err", err)
			continue
		}
		out = append(out, data)
	}
	return out
}

// photoFileIDs picks the largest rendition of each attached photo.
func photoFileIDs(messages ...*telego.Message) []string {
	var ids []string
	for _, m := range messages {
		if m == nil || len(m.Photo) == 0 {
			continue
		}
		ids = append(ids, m.Photo[len(m.Photo)-1].FileID)
	}
	return ids
}

// drawPicture is the image path. Guest mode cannot upload bytes, so the picture
// is first sent to a storage chat to obtain a file_id, which can then be
// referenced when editing the inline message.
func (h *Handler) drawPicture(ctx context.Context, log *slog.Logger, guestQueryID, prompt string) error {
	if h.images == nil {
		_, err := h.answer(ctx, guestQueryID, markdown(msgImagesOff))
		return err
	}
	if prompt == "" {
		_, err := h.answer(ctx, guestQueryID, markdown(msgEmptyDrawing))
		return err
	}

	inlineID, err := h.answer(ctx, guestQueryID, h.placeholder)
	if err != nil {
		return err
	}

	release, ok := h.acquire(ctx)
	if !ok {
		log.Warn("queue full, rejecting drawing")
		return h.editText(ctx, inlineID, msgBusy)
	}
	defer release()

	drawCtx, cancel := context.WithTimeout(ctx, h.imageTimeout)
	defer cancel()

	started := time.Now()
	picture, err := h.images.Generate(drawCtx, prompt)
	if err != nil {
		log.Error("image generation failed", "err", err, "took", time.Since(started))
		text := msgImageFailed
		switch {
		case errors.Is(err, imagegen.ErrOutOfCredits):
			text = msgNoCredits
		case errors.Is(err, context.DeadlineExceeded):
			text = msgImageTimeout
		}
		return h.editText(ctx, inlineID, text)
	}

	fileID, err := h.park(ctx, picture, prompt)
	if err != nil {
		log.Error("uploading the picture failed", "err", err)
		return h.editText(ctx, inlineID, msgImageRejected)
	}
	log.Info("drew", "took", time.Since(started), "bytes", len(picture))

	return h.showPicture(ctx, inlineID, fileID, prompt)
}

// park uploads the picture to the storage chat. Telegram answers with a
// file_id, which is the only way to put those bytes into a chat the bot is not
// a member of.
func (h *Handler) park(parent context.Context, picture []byte, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), editTimeout)
	defer cancel()

	sent, err := h.sender.SendPhoto(ctx, &telego.SendPhotoParams{
		ChatID:              telego.ChatID{ID: h.storageChatID},
		Photo:               telego.InputFile{File: namedBytes{Reader: bytes.NewReader(picture), name: "picture.png"}},
		Caption:             truncateCaption(prompt),
		DisableNotification: true,
	})
	if err != nil {
		return "", err
	}
	if len(sent.Photo) == 0 {
		return "", errors.New("telegram accepted the upload but returned no photo sizes")
	}
	// The last size is the largest one Telegram kept.
	return sent.Photo[len(sent.Photo)-1].FileID, nil
}

// showPicture swaps the placeholder for the finished picture.
func (h *Handler) showPicture(parent context.Context, inlineMessageID, fileID, prompt string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), editTimeout)
	defer cancel()

	_, err := h.sender.EditMessageMedia(ctx, &telego.EditMessageMediaParams{
		InlineMessageID: inlineMessageID,
		Media: &telego.InputMediaPhoto{
			Type:    telego.MediaTypePhoto,
			Media:   telego.InputFile{FileID: fileID},
			Caption: truncateCaption(prompt),
		},
	})
	if err != nil {
		// Fall back to words so the placeholder never stays unresolved.
		h.log.Warn("showing the picture failed", "err", err)
		return h.editText(ctx, inlineMessageID, msgImageRejected)
	}
	return nil
}

// acquire takes a concurrency slot, waiting no longer than the model timeout.
// The placeholder is already in the chat, so queueing reads as "thinking"
// rather than as silence — but a placeholder that never resolves is worse than
// an honest "try again".
func (h *Handler) acquire(ctx context.Context) (release func(), ok bool) {
	if !h.enterQueue() {
		return nil, false
	}
	defer h.leaveQueue()

	queueCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, true
	case <-queueCtx.Done():
		return nil, false
	}
}

// enterQueue takes a place in the waiting line, or reports that the line is
// full. Without a bound, a flood would park an unbounded number of goroutines
// all holding a placeholder message that has to be resolved later.
func (h *Handler) enterQueue() bool {
	h.gate.Lock()
	defer h.gate.Unlock()

	if h.maxQueue > 0 && h.waiting >= h.maxQueue {
		return false
	}
	h.waiting++
	return true
}

func (h *Handler) leaveQueue() {
	h.gate.Lock()
	defer h.gate.Unlock()
	h.waiting--
}

// claim reserves the one slot a caller gets. Holding a request open is what
// makes a bot expensive: without this, one caller could fill every worker and
// everybody else would wait behind them.
func (h *Handler) claim(userID int64) (release func(), ok bool) {
	if userID == 0 {
		return func() {}, true // no id to key on; nothing to protect
	}

	h.gate.Lock()
	defer h.gate.Unlock()

	if _, busy := h.inflight[userID]; busy {
		return nil, false
	}
	h.inflight[userID] = struct{}{}

	return func() {
		h.gate.Lock()
		defer h.gate.Unlock()
		delete(h.inflight, userID)
	}, true
}

// answer sends the one reply guest mode allows and returns the id of the inline
// message it created, which is what makes later edits possible.
func (h *Handler) answer(ctx context.Context, guestQueryID string, content telego.InputRichMessage) (string, error) {
	sent, err := h.sender.AnswerGuestQuery(ctx, &telego.AnswerGuestQueryParams{
		GuestQueryID: guestQueryID,
		Result: &telego.InlineQueryResultArticle{
			Type:                telego.ResultTypeArticle,
			ID:                  resultID(),
			Title:               "Ответ",
			InputMessageContent: &telego.InputRichMessageContent{RichMessage: content},
		},
	})
	if err != nil {
		return "", err
	}
	return sent.InlineMessageID, nil
}

// editTimeout bounds the final edit. It runs on a context detached from the
// caller's: once a placeholder is in the chat we owe the user a resolved
// message even if the handler was cancelled meanwhile.
const editTimeout = 30 * time.Second

// editText replaces the placeholder with text. Rich Markdown is what makes the
// answer look native in Telegram, but a model can emit markup the renderer
// rejects, so a rejected edit is retried as unformatted text: the user always
// gets the content, at worst without the styling.
func (h *Handler) editText(parent context.Context, inlineMessageID, text string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), editTimeout)
	defer cancel()

	_, err := h.sender.EditMessageText(ctx, &telego.EditMessageTextParams{
		InlineMessageID: inlineMessageID,
		RichMessage:     &telego.InputRichMessage{Markdown: text},
	})
	if err == nil {
		return nil
	}

	h.log.Warn("rich edit rejected, retrying as plain text", "err", err)
	_, plainErr := h.sender.EditMessageText(ctx, &telego.EditMessageTextParams{
		InlineMessageID:    inlineMessageID,
		Text:               text,
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	})
	if plainErr != nil {
		return errors.Join(err, plainErr)
	}
	return nil
}

// markdown wraps text as a rich message.
func markdown(text string) telego.InputRichMessage {
	return telego.InputRichMessage{Markdown: text}
}

// truncateCaption keeps a caption within Telegram's limit.
func truncateCaption(s string) string {
	return mdtext.Truncate(s, captionLimit-1)
}

// strippedText returns the summoning message without the mention that
// triggered it. Guest mode gives us this message and, when the user replied to
// something, that message too — no history beyond it exists.
func strippedText(msg telego.Message, botUsername string) string {
	entities := make([]mdtext.Entity, 0, len(msg.Entities))
	for _, e := range msg.Entities {
		entities = append(entities, mdtext.Entity{Type: e.Type, Offset: e.Offset, Length: e.Length})
	}
	return mdtext.StripMention(messageText(&msg), entities, botUsername)
}

// messageText reads whichever field carries the text of a message.
func messageText(msg *telego.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Text != "" {
		return msg.Text
	}
	return msg.Caption
}

// namedBytes adapts a byte slice to the named reader telego wants for uploads.
type namedBytes struct {
	*bytes.Reader
	name string
}

func (n namedBytes) Name() string { return n.name }

// resultID returns a unique inline result id. Telegram allows 1-64 bytes.
func resultID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a fixed id is still valid
		// because guest results are not cached across queries.
		return "dr1wbot"
	}
	return hex.EncodeToString(b[:])
}

package reply

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/mymmrac/telego"

	"dr1wbot/internal/imagegen"
	"dr1wbot/internal/llm"
	"dr1wbot/internal/memory"
	"dr1wbot/internal/quota"
	"dr1wbot/internal/tgemoji"
)

// storageChatID is where the fake parks generated pictures.
const storageChatID int64 = -100999

const testInlineID = "inline-42"

// testDirectMessageID is what the fake returns for a private-chat send.
const testDirectMessageID = 7

// fakeSender records the Telegram calls a handler makes.
type fakeSender struct {
	mu sync.Mutex

	answers    []string // markdown passed to answerGuestQuery, in order
	answerHTML []string // html passed to answerGuestQuery, in order
	edits      []editCall

	photos []photoCall
	medias []mediaCall

	answerErr      error
	richEditErr    error // returned for edits that carry a rich message
	plainErr       error // returned for edits that carry plain text
	sendPhotoErr   error
	editMediaErr   error
	sendMessageErr error
	richSendErr    error // returned for a finished rich answer
	draftErr       error // returned for a streamed draft

	direct []directCall
	rich   []directCall
	drafts []draftCall
}

// draftCall is one streamed draft: either a "thinking" status or the partial
// answer, never both.
type draftCall struct {
	chatID   int64
	draftID  int
	thinking string
	text     string
}

// directCall is one private-chat message.
type directCall struct {
	chatID    int64
	text      string
	parseMode string
}

type photoCall struct {
	chatID  int64
	bytes   []byte
	caption string
}

type mediaCall struct {
	inlineMessageID string
	fileID          string
	caption         string
}

type editCall struct {
	inlineMessageID string
	chatID          int64
	messageID       int
	markdown        string // set when the edit used a rich message
	plain           string // set when the edit used plain text
}

func (f *fakeSender) AnswerGuestQuery(_ context.Context, p *telego.AnswerGuestQueryParams) (*telego.SentGuestMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	article, ok := p.Result.(*telego.InlineQueryResultArticle)
	if !ok {
		return nil, errors.New("result is not an article")
	}
	content, ok := article.InputMessageContent.(*telego.InputRichMessageContent)
	if !ok {
		return nil, errors.New("content is not a rich message")
	}
	rm := content.RichMessage
	f.answers = append(f.answers, rm.Markdown)
	f.answerHTML = append(f.answerHTML, rm.HTML)

	if f.answerErr != nil {
		return nil, f.answerErr
	}
	return &telego.SentGuestMessage{InlineMessageID: testInlineID}, nil
}

func (f *fakeSender) EditMessageText(_ context.Context, p *telego.EditMessageTextParams) (*telego.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	call := editCall{inlineMessageID: p.InlineMessageID, chatID: p.ChatID.ID, messageID: p.MessageID}
	if p.RichMessage != nil {
		call.markdown = p.RichMessage.Markdown
		f.edits = append(f.edits, call)
		if f.richEditErr != nil {
			return nil, f.richEditErr
		}
		return nil, nil
	}

	call.plain = p.Text
	f.edits = append(f.edits, call)
	if f.plainErr != nil {
		return nil, f.plainErr
	}
	return nil, nil
}

func (f *fakeSender) SendMessage(_ context.Context, p *telego.SendMessageParams) (*telego.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.direct = append(f.direct, directCall{chatID: p.ChatID.ID, text: p.Text, parseMode: p.ParseMode})
	if f.sendMessageErr != nil {
		return nil, f.sendMessageErr
	}
	return &telego.Message{MessageID: testDirectMessageID}, nil
}

func (f *fakeSender) SendRichMessage(_ context.Context, p *telego.SendRichMessageParams) (*telego.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.rich = append(f.rich, directCall{chatID: p.ChatID.ID, text: p.RichMessage.Markdown})
	if f.richSendErr != nil {
		return nil, f.richSendErr
	}
	return &telego.Message{MessageID: testDirectMessageID}, nil
}

func (f *fakeSender) SendRichMessageDraft(_ context.Context, p *telego.SendRichMessageDraftParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	call := draftCall{chatID: p.ChatID, draftID: p.DraftID}
	for _, block := range p.RichMessage.Blocks {
		switch b := block.(type) {
		case *telego.InputRichBlockThinking:
			call.thinking = plainText(b.Text)
		case *telego.InputRichBlockParagraph:
			call.text = plainText(b.Text)
		}
	}
	f.drafts = append(f.drafts, call)
	return f.draftErr
}

// plainText unwraps the only kind of rich text the drafts use.
func plainText(text telego.RichText) string {
	if plain, ok := text.(*telego.RichTextPlain); ok {
		return string(*plain)
	}
	return ""
}

func (f *fakeSender) SendPhoto(_ context.Context, p *telego.SendPhotoParams) (*telego.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var data []byte
	if p.Photo.File != nil {
		data, _ = io.ReadAll(p.Photo.File)
	}
	f.photos = append(f.photos, photoCall{chatID: p.ChatID.ID, bytes: data, caption: p.Caption})

	if f.sendPhotoErr != nil {
		return nil, f.sendPhotoErr
	}
	return &telego.Message{Photo: []telego.PhotoSize{{FileID: "parked-file-id"}}}, nil
}

func (f *fakeSender) EditMessageMedia(_ context.Context, p *telego.EditMessageMediaParams) (*telego.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	call := mediaCall{inlineMessageID: p.InlineMessageID}
	if photo, ok := p.Media.(*telego.InputMediaPhoto); ok {
		call.fileID = photo.Media.FileID
		call.caption = photo.Caption
	}
	f.medias = append(f.medias, call)

	if f.editMediaErr != nil {
		return nil, f.editMediaErr
	}
	return nil, nil
}

func (f *fakeSender) snapshot() ([]string, []editCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answers...), append([]editCall(nil), f.edits...)
}

func (f *fakeSender) directs() []directCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]directCall(nil), f.direct...)
}

func (f *fakeSender) richSends() []directCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]directCall(nil), f.rich...)
}

func (f *fakeSender) draftCalls() []draftCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]draftCall(nil), f.drafts...)
}

func (f *fakeSender) pictures() ([]photoCall, []mediaCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]photoCall(nil), f.photos...), append([]mediaCall(nil), f.medias...)
}

// fakeModel stands in for the LLM.
type fakeModel struct {
	mu sync.Mutex

	answer string
	err    error
	waitFn func(ctx context.Context) (string, error)

	prompts   []string
	histories [][]llm.Turn
	systems   []string
	budgets   []int
	images    []int
}

func (m *fakeModel) Complete(ctx context.Context, req llm.Request) (string, error) {
	m.mu.Lock()
	m.prompts = append(m.prompts, req.Prompt)
	m.histories = append(m.histories, req.History)
	m.systems = append(m.systems, req.System)
	m.budgets = append(m.budgets, req.MaxTokens)
	m.images = append(m.images, len(req.Images))
	waitFn := m.waitFn
	m.mu.Unlock()

	if waitFn != nil {
		return waitFn(ctx)
	}
	return m.answer, m.err
}

func (m *fakeModel) lastPrompt(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.prompts) == 0 {
		t.Fatal("model was never called")
	}
	return m.prompts[len(m.prompts)-1]
}

func (m *fakeModel) lastHistory(t *testing.T) []llm.Turn {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.histories) == 0 {
		t.Fatal("model was never called")
	}
	return m.histories[len(m.histories)-1]
}

func (m *fakeModel) lastSystem(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.systems) == 0 {
		t.Fatal("model was never called")
	}
	return m.systems[len(m.systems)-1]
}

func (m *fakeModel) lastMaxTokens(t *testing.T) int {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.budgets) == 0 {
		t.Fatal("model was never called")
	}
	return m.budgets[len(m.budgets)-1]
}

func (m *fakeModel) lastImageCount(t *testing.T) int {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.images) == 0 {
		t.Fatal("model was never called")
	}
	return m.images[len(m.images)-1]
}

// fakeFiles hands back the same bytes for any file_id.
type fakeFiles struct{ data []byte }

func (f *fakeFiles) Download(context.Context, string) ([]byte, error) { return f.data, nil }

func (m *fakeModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

// gate stands in for the access store.
type gate struct {
	allow func(userID, chatID int64) bool
	admin func(userID int64) bool
}

func (g gate) Allowed(userID, chatID int64) bool { return g.allow(userID, chatID) }

func (g gate) IsAdmin(userID int64) bool {
	if g.admin == nil {
		return false
	}
	return g.admin(userID)
}

var (
	allowAll = gate{allow: func(int64, int64) bool { return true }}
	denyAll  = gate{allow: func(int64, int64) bool { return false }}
	// adminGate lets user 111 — the one guestMessage summons as — run the
	// admin-only raw flag.
	adminGate = gate{
		allow: func(int64, int64) bool { return true },
		admin: func(id int64) bool { return id == 111 },
	}
)

// fakeCommands records what it was asked to handle.
type fakeCommands struct {
	mu      sync.Mutex
	reply   string
	handled bool

	seenText   []string
	seenCaller []int64
}

func (f *fakeCommands) Handle(text string, callerID int64) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seenText = append(f.seenText, text)
	f.seenCaller = append(f.seenCaller, callerID)
	return f.reply, f.handled
}

type handlerOpts struct {
	access          Gatekeeper
	commands        Commander
	images          imagegen.Generator
	memory          Remembrancer
	maxConcurrent   int
	maxReplyRunes   int
	timeout         time.Duration
	commandReplyTTL time.Duration
	rawSystemPrompt string
	ration          Rationer
	bans            Bouncer
	files           Downloader
	publicMaxTokens int
	publicMaxRunes  int
	streamEvery     time.Duration
}

func newHandler(sender Sender, model *fakeModel, o handlerOpts) *Handler {
	if o.access == nil {
		o.access = allowAll
	}
	if o.maxConcurrent == 0 {
		o.maxConcurrent = 4
	}
	if o.maxReplyRunes == 0 {
		o.maxReplyRunes = 3500
	}
	if o.timeout == 0 {
		o.timeout = time.Second
	}
	return New(Options{
		Sender:        sender,
		Model:         model,
		Access:        o.access,
		Commands:      o.commands,
		Images:        o.images,
		Memory:        o.memory,
		StorageChatID: storageChatID,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername:   "dr1wbot",
		MaxConcurrent: o.maxConcurrent,
		MaxReplyRunes: o.maxReplyRunes,
		Timeout:       o.timeout,

		CommandReplyTTL: o.commandReplyTTL,
		RawSystemPrompt: o.rawSystemPrompt,
		Quota:           o.ration,
		Bans:            o.bans,
		StreamEvery:     o.streamEvery,
		Files:           o.files,
		PublicMaxTokens: o.publicMaxTokens,
		PublicMaxRunes:  o.publicMaxRunes,
	})
}

// guestMessage builds the update a guest summon produces. The mention entity
// uses UTF-16 offsets, as Telegram does.
func guestMessage(text string) telego.Message {
	msg := telego.Message{
		GuestQueryID: "gq-1",
		Chat:         telego.Chat{ID: -100500},
		From:         &telego.User{ID: 111},
		Text:         text,
	}
	if i := strings.Index(text, "@dr1wbot"); i >= 0 {
		msg.Entities = []telego.MessageEntity{{
			Type:   "mention",
			Offset: len(utf16.Encode([]rune(text[:i]))),
			Length: len(utf16.Encode([]rune("@dr1wbot"))),
		}}
	}
	return msg
}

func TestAnswersWithPlaceholderThenEdit(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "# Ответ\n\nВот **таблица**."}
	h := newHandler(sender, model, handlerOpts{})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot объясни CAP")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	answers, edits := sender.snapshot()
	if len(answers) != 1 || answers[0] != "" {
		t.Fatalf("answers = %q, want the placeholder to be sent as HTML, not markdown", answers)
	}
	if len(edits) != 1 {
		t.Fatalf("edits = %+v, want exactly one", edits)
	}
	if edits[0].inlineMessageID != testInlineID {
		t.Errorf("edit targeted %q, want the id returned by answerGuestQuery (%q)", edits[0].inlineMessageID, testInlineID)
	}
	if edits[0].markdown != model.answer {
		t.Errorf("edit markdown = %q, want the model answer %q", edits[0].markdown, model.answer)
	}
	if edits[0].plain != "" {
		t.Error("edit used plain text, want rich markdown on the happy path")
	}
}

func TestPromptStripsMentionAndIncludesQuote(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{})

	msg := guestMessage("@dr1wbot почему?")
	msg.ReplyToMessage = &telego.Message{Text: "деплой упал с OOM"}

	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	prompt := model.lastPrompt(t)
	if strings.Contains(prompt, "@dr1wbot") {
		t.Errorf("prompt %q still contains the mention", prompt)
	}
	if !strings.Contains(prompt, "деплой упал с OOM") {
		t.Errorf("prompt %q is missing the replied-to message", prompt)
	}
	if !strings.Contains(prompt, "почему?") {
		t.Errorf("prompt %q is missing the question", prompt)
	}
}

// ownAnswer is a quoted message that came from us, which is what makes a summon
// a follow-up rather than a new question.
func ownAnswer(text string) *telego.Message {
	return &telego.Message{Text: text, GuestBotCallerUser: &telego.User{ID: 111}}
}

func TestFollowUpCarriesTheRememberedHistory(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "второй ответ"}
	store := memory.New(memory.Options{})
	h := newHandler(sender, model, handlerOpts{memory: store})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot что такое CAP")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	msg := guestMessage("@dr1wbot а подробнее?")
	msg.ReplyToMessage = ownAnswer("второй ответ")
	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	history := model.lastHistory(t)
	if len(history) == 0 {
		t.Fatal("history = empty, want the first exchange carried into the follow-up")
	}
	joined := ""
	for _, turn := range history {
		joined += turn.Content + "\n"
	}
	if !strings.Contains(joined, "что такое CAP") {
		t.Errorf("history = %q, want the earlier question in it", joined)
	}
}

func TestFreshSummonStartsFromNothing(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ответ"}
	store := memory.New(memory.Options{})
	h := newHandler(sender, model, handlerOpts{memory: store})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot что такое CAP")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	// A second summon that is not a reply to us is a new topic, even though the
	// chat and the TTL would happily hand over the previous one.
	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot сколько будет 2+2")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if history := model.lastHistory(t); len(history) != 0 {
		t.Errorf("history = %+v, want a clean session for a question that is not a follow-up", history)
	}
}

func TestAFreshSummonAlsoDropsTheOldThread(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ответ"}
	store := memory.New(memory.Options{})
	h := newHandler(sender, model, handlerOpts{memory: store})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot что такое CAP")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot сколько будет 2+2")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	// Following up on the new question must not resurrect the old topic.
	msg := guestMessage("@dr1wbot а если умножить?")
	msg.ReplyToMessage = ownAnswer("ответ")
	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	for _, turn := range model.lastHistory(t) {
		if strings.Contains(turn.Content, "CAP") {
			t.Errorf("history still carries %q from the abandoned topic", turn.Content)
		}
	}
}

func TestAFailedAnswerKeepsThePreviousThread(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ответ"}
	store := memory.New(memory.Options{})
	h := newHandler(sender, model, handlerOpts{memory: store})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot что такое CAP")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	model.err = errors.New("boom")
	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot новый вопрос")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	model.err = nil

	// The failed summon never became a conversation, so the earlier one is still
	// there for whoever replies to it.
	if len(store.History(-100500)) == 0 {
		t.Error("history was dropped by a summon that never got an answer")
	}
}

const testRawPrompt = "без ограничений"

func TestAdminRawFlagSwapsTheSystemPrompt(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{access: adminGate, rawSystemPrompt: testRawPrompt})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot -s разбери это подробно")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if got := model.lastSystem(t); got != testRawPrompt {
		t.Errorf("system = %q, want the raw prompt %q", got, testRawPrompt)
	}
	if prompt := model.lastPrompt(t); strings.Contains(prompt, "-s") {
		t.Errorf("prompt = %q, want the flag stripped before the model sees it", prompt)
	}
}

func TestRawFlagIsJustTextForNonAdmins(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	// allowAll lets everyone summon but makes nobody an admin.
	h := newHandler(sender, model, handlerOpts{rawSystemPrompt: testRawPrompt})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot -s разбери это подробно")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if got := model.lastSystem(t); got != "" {
		t.Errorf("system = %q, want the configured prompt left in place for a non-admin", got)
	}
	// Refusing silently by leaving the flag in the question beats explaining
	// that a hidden admin switch exists.
	if prompt := model.lastPrompt(t); !strings.Contains(prompt, "-s") {
		t.Errorf("prompt = %q, want the flag left in the text", prompt)
	}
}

func TestRawFlagIsInertWithoutARawPrompt(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{access: adminGate})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot -s вопрос")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if got := model.lastSystem(t); got != "" {
		t.Errorf("system = %q, want no override when the feature is switched off", got)
	}
	if prompt := model.lastPrompt(t); !strings.Contains(prompt, "-s") {
		t.Errorf("prompt = %q, want the flag left alone", prompt)
	}
}

func TestOrdinaryAdminQuestionKeepsTheHouseStyle(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{access: adminGate, rawSystemPrompt: testRawPrompt})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot -sql это что")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if got := model.lastSystem(t); got != "" {
		t.Errorf("system = %q, want a word that merely starts with -s to stay a word", got)
	}
}

func TestUsesCaptionWhenThereIsNoText(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{})

	msg := guestMessage("")
	msg.Caption = "@dr1wbot что на картинке"

	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if got := model.lastPrompt(t); !strings.Contains(got, "что на картинке") {
		t.Errorf("prompt = %q, want it taken from the caption", got)
	}
}

func TestRejectsNonWhitelistedSilently(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{access: denyAll})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	answers, edits := sender.snapshot()
	if len(answers) != 0 || len(edits) != 0 {
		t.Errorf("answers = %q, edits = %+v; a rejected summon must leave no trace in the chat", answers, edits)
	}
	if model.callCount() != 0 {
		t.Error("model was called for a non-whitelisted user, want no spend")
	}
}

func TestIgnoresMessageWithoutGuestQueryID(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{})

	msg := guestMessage("@dr1wbot привет")
	msg.GuestQueryID = ""

	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if answers, edits := sender.snapshot(); len(answers) != 0 || len(edits) != 0 {
		t.Errorf("answers = %q, edits = %+v, want no calls", answers, edits)
	}
}

func TestEmptyPromptAnswersHintWithoutCallingModel(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	answers, edits := sender.snapshot()
	if len(answers) != 1 || answers[0] != msgEmptyPrompt {
		t.Errorf("answers = %q, want the hint %q", answers, msgEmptyPrompt)
	}
	if len(edits) != 0 {
		t.Errorf("edits = %+v, want none: the hint is the final answer", edits)
	}
	if model.callCount() != 0 {
		t.Error("model was called for an empty prompt, want no spend")
	}
}

func TestModelErrorIsShownToTheUser(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{err: errors.New("502 bad gateway")}
	h := newHandler(sender, model, handlerOpts{})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 1 || edits[0].markdown != msgFailed {
		t.Errorf("edits = %+v, want a single edit with %q", edits, msgFailed)
	}
}

func TestModelTimeoutIsShownToTheUser(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{waitFn: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	h := newHandler(sender, model, handlerOpts{timeout: 20 * time.Millisecond})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 1 || edits[0].markdown != msgTimeout {
		t.Errorf("edits = %+v, want a single edit with %q", edits, msgTimeout)
	}
}

func TestRejectedRichEditFallsBackToPlainText(t *testing.T) {
	answer := "таблица с битой разметкой"
	sender := &fakeSender{richEditErr: errors.New("400: can't parse rich message")}
	model := &fakeModel{answer: answer}
	h := newHandler(sender, model, handlerOpts{})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot дай таблицу")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 2 {
		t.Fatalf("edits = %+v, want a rich attempt followed by a plain retry", edits)
	}
	if edits[0].markdown != answer {
		t.Errorf("first edit = %q, want the rich attempt with %q", edits[0].markdown, answer)
	}
	if edits[1].plain != answer {
		t.Errorf("retry = %+v, want plain text %q so the content still reaches the user", edits[1], answer)
	}
}

func TestBothEditsFailingIsReported(t *testing.T) {
	richErr := errors.New("rich rejected")
	plainErr := errors.New("message not found")
	sender := &fakeSender{richEditErr: richErr, plainErr: plainErr}
	h := newHandler(sender, &fakeModel{answer: "ок"}, handlerOpts{})

	err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет"))
	if err == nil {
		t.Fatal("HandleGuestMessage() error = nil, want both failures reported")
	}
	if !errors.Is(err, richErr) || !errors.Is(err, plainErr) {
		t.Errorf("error = %v, want it to wrap both %v and %v", err, richErr, plainErr)
	}
}

func TestFailedPlaceholderSkipsTheModel(t *testing.T) {
	sender := &fakeSender{answerErr: errors.New("guest query expired")}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err == nil {
		t.Fatal("HandleGuestMessage() error = nil, want the answerGuestQuery failure")
	}
	if model.callCount() != 0 {
		t.Error("model was called with no message to edit, want no spend")
	}
	if _, edits := sender.snapshot(); len(edits) != 0 {
		t.Errorf("edits = %+v, want none without an inline message id", edits)
	}
}

func TestAnswerIsTruncated(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: strings.Repeat("слово ", 500)}
	h := newHandler(sender, model, handlerOpts{maxReplyRunes: 100})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot много текста")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if got := len([]rune(edits[0].markdown)); got > 102 {
		t.Errorf("edit is %d runes, want it truncated to about 100", got)
	}
}

func TestQueueFullTellsTheUserToRetry(t *testing.T) {
	sender := &fakeSender{}
	release := make(chan struct{})
	// Hold the slot regardless of the context so the occupying summon cannot
	// free it by timing out; this test is about the queue, not about timeouts.
	model := &fakeModel{waitFn: func(context.Context) (string, error) {
		<-release
		return "ок", nil
	}}
	h := newHandler(sender, model, handlerOpts{maxConcurrent: 1, timeout: 50 * time.Millisecond})

	// Occupy the single slot.
	busy := make(chan struct{})
	go func() {
		defer close(busy)
		_ = h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot первый"))
	}()

	// Wait until the first summon is actually inside the model call.
	deadline := time.After(2 * time.Second)
	for model.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("first summon never reached the model")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// From a different person: the same one would be turned away by the
	// one-at-a-time rule before ever reaching the queue.
	second := guestMessage("@dr1wbot второй")
	second.From = &telego.User{ID: 222}
	if err := h.HandleGuestMessage(context.Background(), second); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	close(release)
	<-busy

	_, edits := sender.snapshot()
	var sawBusy bool
	for _, e := range edits {
		if e.markdown == msgBusy {
			sawBusy = true
		}
	}
	if !sawBusy {
		t.Errorf("edits = %+v, want one of them to be %q", edits, msgBusy)
	}
	if model.callCount() != 1 {
		t.Errorf("model was called %d times, want the queued summon rejected before spending", model.callCount())
	}
}

func TestCommandIsAnsweredWithoutCallingTheModel(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "не должно попасть в чат"}
	commands := &fakeCommands{reply: "✅ `222` добавлен.", handled: true}
	h := newHandler(sender, model, handlerOpts{commands: commands})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot /add 222")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if model.callCount() != 0 {
		t.Error("model was called for a command, want commands answered locally")
	}

	answers, edits := sender.snapshot()
	if len(answers) != 1 || answers[0] != commands.reply {
		t.Errorf("answers = %q, want the command reply %q", answers, commands.reply)
	}
	if len(edits) != 0 {
		t.Errorf("edits = %+v, want none: a command needs no placeholder", edits)
	}
}

func TestCommandReplyShrinksAfterItsTTL(t *testing.T) {
	sender := &fakeSender{}
	commands := &fakeCommands{reply: "111 — доступ выдан", handled: true}
	h := newHandler(sender, &fakeModel{}, handlerOpts{commands: commands, commandReplyTTL: time.Minute})

	var delay time.Duration
	var fire func()
	h.after = func(d time.Duration, f func()) { delay, fire = d, f }

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot /add 111")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if _, edits := sender.snapshot(); len(edits) != 0 {
		t.Fatalf("edits = %+v, want the answer left alone until the TTL passes", edits)
	}
	if delay != time.Minute {
		t.Errorf("cleanup scheduled in %s, want the configured TTL", delay)
	}

	fire()

	_, edits := sender.snapshot()
	if len(edits) != 1 {
		t.Fatalf("edits = %+v, want the answer shrunk once", edits)
	}
	if edits[0].markdown != msgCommandExpired {
		t.Errorf("edit = %q, want %q", edits[0].markdown, msgCommandExpired)
	}
	if edits[0].inlineMessageID != testInlineID {
		t.Errorf("edit targeted %q, want the command's own message %q", edits[0].inlineMessageID, testInlineID)
	}
}

func TestCommandReplyStaysWhenTTLIsZero(t *testing.T) {
	sender := &fakeSender{}
	commands := &fakeCommands{reply: "список пуст", handled: true}
	h := newHandler(sender, &fakeModel{}, handlerOpts{commands: commands})

	scheduled := false
	h.after = func(time.Duration, func()) { scheduled = true }

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot /list")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if scheduled {
		t.Error("cleanup was scheduled with no TTL configured, want the answer left standing")
	}
}

func TestCommandCleanupSurvivesACancelledHandler(t *testing.T) {
	sender := &fakeSender{}
	commands := &fakeCommands{reply: "111 — доступ выдан", handled: true}
	h := newHandler(sender, &fakeModel{}, handlerOpts{commands: commands, commandReplyTTL: time.Minute})

	var fire func()
	h.after = func(_ time.Duration, f func()) { fire = f }

	ctx, cancel := context.WithCancel(context.Background())
	if err := h.HandleGuestMessage(ctx, guestMessage("@dr1wbot /add 111")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	// By the time the timer fires the handler's context is long gone; the edit
	// must not be cancelled with it.
	cancel()
	fire()

	if _, edits := sender.snapshot(); len(edits) != 1 {
		t.Errorf("edits = %+v, want the cleanup to run on a detached context", edits)
	}
}

func TestCommandSeesTheTextWithoutTheMention(t *testing.T) {
	commands := &fakeCommands{}
	h := newHandler(&fakeSender{}, &fakeModel{answer: "ок"}, handlerOpts{commands: commands})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot /add 222")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	commands.mu.Lock()
	defer commands.mu.Unlock()
	if len(commands.seenText) != 1 {
		t.Fatalf("command handler saw %d texts, want 1", len(commands.seenText))
	}
	if commands.seenText[0] != "/add 222" {
		t.Errorf("command handler saw %q, want the mention stripped", commands.seenText[0])
	}
	if commands.seenCaller[0] != 111 {
		t.Errorf("caller = %d, want the summoning user id 111", commands.seenCaller[0])
	}
}

func TestUnhandledCommandFallsThroughToTheModel(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ответ модели"}
	commands := &fakeCommands{handled: false}
	h := newHandler(sender, model, handlerOpts{commands: commands})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot что такое кворум?")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if model.callCount() != 1 {
		t.Error("model was not called, want ordinary questions to reach it")
	}
	if _, edits := sender.snapshot(); len(edits) != 1 || edits[0].markdown != model.answer {
		t.Errorf("edits = %+v, want the model answer", edits)
	}
}

func TestCommandsAreNotRunForRejectedUsers(t *testing.T) {
	commands := &fakeCommands{reply: "ок", handled: true}
	h := newHandler(&fakeSender{}, &fakeModel{}, handlerOpts{access: denyAll, commands: commands})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot /add 222")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	commands.mu.Lock()
	defer commands.mu.Unlock()
	if len(commands.seenText) != 0 {
		t.Errorf("command handler saw %q, want the access check to run first", commands.seenText)
	}
}

// fakeImages stands in for the picture generator.
type fakeImages struct {
	mu      sync.Mutex
	picture []byte
	err     error
	waitFn  func(ctx context.Context) ([]byte, error)
	prompts []string
}

func (f *fakeImages) Generate(ctx context.Context, prompt string) ([]byte, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	waitFn := f.waitFn
	f.mu.Unlock()

	if waitFn != nil {
		return waitFn(ctx)
	}
	return f.picture, f.err
}

func (f *fakeImages) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeImages) lastPrompt(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		t.Fatal("the generator was never called")
	}
	return f.prompts[len(f.prompts)-1]
}

func TestPlaceholderCarriesTheCustomEmoji(t *testing.T) {
	sender := &fakeSender{}
	h := newHandler(sender, &fakeModel{answer: "ок"}, handlerOpts{})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.answerHTML) != 1 {
		t.Fatalf("answers = %d, want one placeholder", len(sender.answerHTML))
	}
	got := sender.answerHTML[0]
	// A custom emoji has no Markdown syntax, so the placeholder must go as HTML.
	if !strings.Contains(got, `emoji-id="`+tgemoji.IDPlaceholder+`"`) {
		t.Errorf("placeholder = %q, want it to carry the custom emoji id", got)
	}
	// Three reads as a progress indicator; one reads as a message the bot
	// meant to send.
	if strings.Count(got, "<tg-emoji") != 3 {
		t.Errorf("placeholder = %q, want three emoji", got)
	}
	// The fallback must be a real emoji: it is what clients that cannot render
	// the custom one show instead.
	if !strings.Contains(got, tgemoji.PlaceholderAlt) {
		t.Errorf("placeholder = %q, want a plain fallback inside", got)
	}
}

func TestDrawingParksThePictureThenShowsIt(t *testing.T) {
	picture := []byte("\x89PNG fake bytes")
	sender := &fakeSender{}
	images := &fakeImages{picture: picture}
	model := &fakeModel{answer: "не должно попасть в чат"}
	h := newHandler(sender, model, handlerOpts{images: images})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй кота в скафандре")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if model.callCount() != 0 {
		t.Error("the text model was called for a drawing request, want only the image model")
	}
	if got := images.lastPrompt(t); got != "кота в скафандре" {
		t.Errorf("image prompt = %q, want the trigger stripped", got)
	}

	photos, medias := sender.pictures()
	if len(photos) != 1 {
		t.Fatalf("photos = %+v, want the picture parked once", photos)
	}
	if photos[0].chatID != storageChatID {
		t.Errorf("parked into chat %d, want the storage chat %d", photos[0].chatID, storageChatID)
	}
	if string(photos[0].bytes) != string(picture) {
		t.Errorf("parked %q, want the generated bytes", photos[0].bytes)
	}

	if len(medias) != 1 {
		t.Fatalf("media edits = %+v, want exactly one", medias)
	}
	if medias[0].inlineMessageID != testInlineID {
		t.Errorf("edited %q, want the inline message from answerGuestQuery", medias[0].inlineMessageID)
	}
	if medias[0].fileID != "parked-file-id" {
		t.Errorf("file id = %q, want the one Telegram returned for the parked photo", medias[0].fileID)
	}
	if medias[0].caption != "кота в скафандре" {
		t.Errorf("caption = %q, want the prompt", medias[0].caption)
	}
}

func TestDrawingViaCommand(t *testing.T) {
	images := &fakeImages{picture: []byte("png")}
	h := newHandler(&fakeSender{}, &fakeModel{}, handlerOpts{images: images})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot /img кот на луне")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if got := images.lastPrompt(t); got != "кот на луне" {
		t.Errorf("image prompt = %q, want the command stripped", got)
	}
}

func TestDrawingWithoutASubjectAsksForOne(t *testing.T) {
	sender := &fakeSender{}
	images := &fakeImages{picture: []byte("png")}
	h := newHandler(sender, &fakeModel{}, handlerOpts{images: images})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if images.callCount() != 0 {
		t.Error("the generator was called with an empty prompt, want no spend")
	}
	answers, _ := sender.snapshot()
	if len(answers) != 1 || answers[0] != msgEmptyDrawing {
		t.Errorf("answers = %q, want the hint %q", answers, msgEmptyDrawing)
	}
}

func TestDrawingWhenImagesAreOff(t *testing.T) {
	sender := &fakeSender{}
	h := newHandler(sender, &fakeModel{answer: "ок"}, handlerOpts{}) // no generator

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй кота")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	answers, edits := sender.snapshot()
	if len(answers) != 1 || answers[0] != msgImagesOff {
		t.Errorf("answers = %q, want %q", answers, msgImagesOff)
	}
	if len(edits) != 0 {
		t.Errorf("edits = %+v, want none: the refusal is the final answer", edits)
	}
}

func TestDrawingFailureIsExplained(t *testing.T) {
	sender := &fakeSender{}
	images := &fakeImages{err: errors.New("content policy")}
	h := newHandler(sender, &fakeModel{}, handlerOpts{images: images})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй кота")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 1 || edits[0].markdown != msgImageFailed {
		t.Errorf("edits = %+v, want a single edit with %q", edits, msgImageFailed)
	}
	if photos, medias := sender.pictures(); len(photos) != 0 || len(medias) != 0 {
		t.Error("nothing should be parked or shown when generation fails")
	}
}

func TestDrawingTimeoutIsExplained(t *testing.T) {
	sender := &fakeSender{}
	images := &fakeImages{waitFn: func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	h := New(Options{
		Sender: sender, Model: &fakeModel{}, Images: images, Access: allowAll,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername: "dr1wbot", StorageChatID: storageChatID,
		MaxConcurrent: 1, MaxReplyRunes: 3500,
		Timeout: time.Second, ImageTimeout: 20 * time.Millisecond,
	})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй кота")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 1 || edits[0].markdown != msgImageTimeout {
		t.Errorf("edits = %+v, want a single edit with %q", edits, msgImageTimeout)
	}
}

func TestParkingFailureIsExplained(t *testing.T) {
	sender := &fakeSender{sendPhotoErr: errors.New("bot is not a member of the storage chat")}
	images := &fakeImages{picture: []byte("png")}
	h := newHandler(sender, &fakeModel{}, handlerOpts{images: images})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй кота")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 1 || edits[0].markdown != msgImageRejected {
		t.Errorf("edits = %+v, want a single edit with %q", edits, msgImageRejected)
	}
}

func TestRejectedMediaEditFallsBackToText(t *testing.T) {
	sender := &fakeSender{editMediaErr: errors.New("MEDIA_EMPTY")}
	images := &fakeImages{picture: []byte("png")}
	h := newHandler(sender, &fakeModel{}, handlerOpts{images: images})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй кота")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 1 || edits[0].markdown != msgImageRejected {
		t.Errorf("edits = %+v, want the placeholder resolved with %q", edits, msgImageRejected)
	}
}

func TestOrdinaryQuestionNeverDraws(t *testing.T) {
	sender := &fakeSender{}
	images := &fakeImages{picture: []byte("png")}
	model := &fakeModel{answer: "кворум — это ..."}
	h := newHandler(sender, model, handlerOpts{images: images})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot что такое кворум?")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	if images.callCount() != 0 {
		t.Error("a plain question triggered the image model, want text only")
	}
	if model.callCount() != 1 {
		t.Error("the text model was not called")
	}
}

func TestPlaceholderIsResolvedEvenIfCallerContextIsCancelled(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{waitFn: func(ctx context.Context) (string, error) {
		return "поздний, но валидный ответ", nil
	}}
	h := newHandler(sender, model, handlerOpts{})

	ctx, cancel := context.WithCancel(context.Background())
	msg := guestMessage("@dr1wbot привет")
	cancel() // the handler was cancelled, e.g. the bot is shutting down

	if err := h.HandleGuestMessage(ctx, msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	_, edits := sender.snapshot()
	if len(edits) != 1 {
		t.Fatalf("edits = %+v, want the placeholder resolved despite cancellation", edits)
	}
	if edits[0].markdown == "" {
		t.Error("the placeholder was left in the chat, want it replaced")
	}
}

func TestOneQuestionAtATimePerPerson(t *testing.T) {
	sender := &fakeSender{}
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	model := &fakeModel{waitFn: func(context.Context) (string, error) {
		<-release
		return "ок", nil
	}}
	h := newHandler(sender, model, handlerOpts{maxConcurrent: 4, timeout: time.Second})

	busy := make(chan struct{})
	go func() {
		defer close(busy)
		_ = h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot первый"))
	}()

	deadline := time.After(2 * time.Second)
	for model.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("first summon never reached the model")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Same person, second question: there are free workers, but one caller must
	// not be able to occupy several of them.
	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot второй")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	close(release)
	<-busy

	answers, _ := sender.snapshot()
	var sawRefusal bool
	for _, a := range answers {
		if a == msgAlreadyBusy {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Errorf("answers = %q, want the second question turned away", answers)
	}
	if model.callCount() != 1 {
		t.Errorf("model called %d times, want the second question rejected before spending", model.callCount())
	}
}

func TestTheSlotIsFreedAfterAnAnswer(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{})

	for i := range 3 {
		if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot вопрос")); err != nil {
			t.Fatalf("HandleGuestMessage() #%d error = %v", i, err)
		}
	}
	if model.callCount() != 3 {
		t.Errorf("model called %d times, want the slot released between questions", model.callCount())
	}
}

// fakeRation is a scripted verdict source.
type fakeRation struct {
	on      bool
	verdict quota.Verdict
	seen    []int64
}

func (f *fakeRation) Enabled() bool { return f.on }

func (f *fakeRation) Judge(userID int64) (quota.Verdict, int, int) {
	f.seen = append(f.seen, userID)
	return f.verdict, 1, 30
}

func TestPublicQuestionIsServedAndCounted(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	ration := &fakeRation{on: true, verdict: quota.Granted}
	h := newHandler(sender, model, handlerOpts{access: denyAll, ration: ration})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if model.callCount() != 1 {
		t.Errorf("model called %d times, want a public question answered", model.callCount())
	}
	if len(ration.seen) != 1 || ration.seen[0] != 111 {
		t.Errorf("judged %v, want the asker charged once", ration.seen)
	}
}

func TestSpentAllowanceIsExplained(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{
		access: denyAll,
		ration: &fakeRation{on: true, verdict: quota.Spent},
	})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	answers, _ := sender.snapshot()
	if len(answers) != 1 || !strings.Contains(answers[0], "На сегодня всё") {
		t.Errorf("answers = %q, want the limit explained", answers)
	}
	if model.callCount() != 0 {
		t.Error("model was called for a caller with nothing left")
	}
}

func TestBannedCallerGetsSilence(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{
		access: denyAll,
		ration: &fakeRation{on: true, verdict: quota.Banned},
	})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	// Not a word: an answer is the feedback a script is looking for, and
	// sending one is a Telegram call we would be making on a bot's behalf.
	answers, edits := sender.snapshot()
	if len(answers) != 0 || len(edits) != 0 {
		t.Errorf("answers = %q, edits = %+v, want nothing sent to a banned caller", answers, edits)
	}
	if model.callCount() != 0 {
		t.Error("model was called for a banned caller")
	}
}

func TestPublicQuestionIsCappedAndStripped(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{
		access:          denyAll,
		ration:          &fakeRation{on: true, verdict: quota.Granted},
		publicMaxTokens: 512,
	})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if got := model.lastMaxTokens(t); got != 512 {
		t.Errorf("max tokens = %d, want the public cap", got)
	}
}

func TestOverlongPublicQuestionIsRefusedBeforeSpending(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	ration := &fakeRation{on: true, verdict: quota.Granted}
	h := newHandler(sender, model, handlerOpts{
		access:         denyAll,
		ration:         ration,
		publicMaxRunes: 20,
	})

	msg := guestMessage("@dr1wbot " + strings.Repeat("длинный вопрос ", 20))
	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	answers, _ := sender.snapshot()
	if len(answers) != 1 || !strings.Contains(answers[0], "длинный вопрос") == false && !strings.Contains(answers[0], "Слишком длинный") {
		t.Errorf("answers = %q, want the length explained", answers)
	}
	if len(ration.seen) != 0 {
		t.Error("an allowance was spent on a question that was never sent")
	}
	if model.callCount() != 0 {
		t.Error("model was called for an overlong question")
	}
}

func TestPublicCallersGetNoVision(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{
		access: denyAll,
		ration: &fakeRation{on: true, verdict: quota.Granted},
		files:  &fakeFiles{data: []byte("\x89PNG\r\n\x1a\n")},
	})

	msg := guestMessage("@dr1wbot что тут")
	msg.Photo = []telego.PhotoSize{{FileID: "photo-1"}}
	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	// A photo is worth a page of text in input tokens; the public path is text
	// only.
	if n := model.lastImageCount(t); n != 0 {
		t.Errorf("images sent = %d, want none for a public caller", n)
	}
}

func TestPublicCallersCannotDraw(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{
		access: denyAll,
		ration: &fakeRation{on: true, verdict: quota.Granted},
		images: &fakeImages{picture: []byte("\x89PNG\r\n\x1a\n")},
	})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot нарисуй кота")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	answers, _ := sender.snapshot()
	if len(answers) != 1 || !strings.Contains(answers[0], "списке доступа") {
		t.Errorf("answers = %q, want drawing refused for a public caller", answers)
	}
}

// directMessage builds an ordinary private-chat message.
func directMessage(from int64, text string) telego.Message {
	return telego.Message{
		Chat: telego.Chat{ID: from, Type: telego.ChatTypePrivate},
		From: &telego.User{ID: from},
		Text: text,
	}
}

func TestDirectMessageStreamsADraftThenSendsTheAnswer(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "# Ответ\n\nВот **таблица**."}
	h := newHandler(sender, model, handlerOpts{})

	if err := h.HandleDirectMessage(context.Background(), directMessage(111, "объясни CAP")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}

	// Silence for the seconds an answer takes reads as a broken bot, so the
	// draft goes out before the model is even called. Telegram animates it for
	// us, which is why no placeholder message is posted any more.
	drafts := sender.draftCalls()
	if len(drafts) == 0 {
		t.Fatal("drafts = none, want a thinking draft while the answer is written")
	}
	if drafts[0].chatID != 111 || drafts[0].draftID == 0 {
		t.Errorf("draft = %+v, want the private chat and a non-zero id", drafts[0])
	}
	if drafts[0].thinking == "" {
		t.Errorf("draft = %+v, want the native thinking block first", drafts[0])
	}
	if got := sender.directs(); len(got) != 0 {
		t.Errorf("plain messages = %+v, want none: the draft replaces the placeholder", got)
	}

	// The draft is a 30-second preview and is never persisted, so the finished
	// answer is a message of its own. Rich markdown is what carries headings and
	// tables; the legacy parse mode cannot render either.
	rich := sender.richSends()
	if len(rich) != 1 {
		t.Fatalf("rich messages = %+v, want exactly the finished answer", rich)
	}
	if rich[0].chatID != 111 || rich[0].text != model.answer {
		t.Errorf("answer = %+v, want the answer as a rich message in the chat", rich[0])
	}
	if _, edits := sender.snapshot(); len(edits) != 0 {
		t.Errorf("edits = %+v, want none: nothing is edited in a private chat any more", edits)
	}
}

func TestDirectMessageKeepsTheConversation(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	store := memory.New(memory.Options{})
	h := newHandler(sender, model, handlerOpts{memory: store})

	if err := h.HandleDirectMessage(context.Background(), directMessage(111, "что такое CAP")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}
	if err := h.HandleDirectMessage(context.Background(), directMessage(111, "а подробнее?")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}

	// A private chat is a real conversation: every message continues it, with
	// no reply-to signal to wait for the way guest mode needs one.
	history := model.lastHistory(t)
	if len(history) == 0 {
		t.Fatal("history = empty, want the private chat to remember itself")
	}
}

func TestDirectMessageRespectsTheWhitelist(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{access: denyAll})

	if err := h.HandleDirectMessage(context.Background(), directMessage(999, "привет")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}
	// A private chat is not a way around the whitelist.
	if len(sender.directs()) != 0 || model.callCount() != 0 {
		t.Error("a stranger got an answer in a private chat")
	}
}

func TestDirectMessageHandlesCommands(t *testing.T) {
	sender := &fakeSender{}
	commands := &fakeCommands{reply: "список пуст", handled: true}
	h := newHandler(sender, &fakeModel{}, handlerOpts{commands: commands})

	if err := h.HandleDirectMessage(context.Background(), directMessage(111, "/list")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}

	directs := sender.directs()
	if len(directs) != 1 || directs[0].text != "список пуст" {
		t.Errorf("sent %+v, want the command answered without a placeholder", directs)
	}
}

func TestDirectMessageIgnoresEmptyText(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{})

	if err := h.HandleDirectMessage(context.Background(), directMessage(111, "   ")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}
	if len(sender.directs()) != 0 || model.callCount() != 0 {
		t.Error("an empty message was answered")
	}
}

func TestDirectMessageBannedCallerGetsSilence(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ок"}
	h := newHandler(sender, model, handlerOpts{
		access: denyAll,
		ration: &fakeRation{on: true, verdict: quota.Banned},
	})

	if err := h.HandleDirectMessage(context.Background(), directMessage(999, "привет")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}
	if len(sender.directs()) != 0 {
		t.Errorf("sent %+v, want nothing to a banned caller", sender.directs())
	}
}

// fakeBouncer answers the ban question the way a test wants it answered.
type fakeBouncer struct {
	mu      sync.Mutex
	banned  bool
	noted   []string
	notedID []int64
}

func (f *fakeBouncer) Note(id int64, username string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notedID = append(f.notedID, id)
	f.noted = append(f.noted, username)
	return false
}

func (f *fakeBouncer) Blocked(int64, string) (quota.Ban, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.banned {
		return quota.Ban{}, false
	}
	return quota.Ban{ID: 111, Forever: true, Manual: true}, true
}

func TestABannedCallerIsIgnoredEvenWhenWhitelisted(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "не должно прозвучать"}
	bouncer := &fakeBouncer{banned: true}
	// allowAll stands for a whitelist that would otherwise let them through: a
	// ban that only applies to strangers is not a ban.
	h := newHandler(sender, model, handlerOpts{bans: bouncer})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if answers, _ := sender.snapshot(); len(answers) != 0 {
		t.Errorf("answers = %v, want silence for a banned caller", answers)
	}
	if got := model.callCount(); got != 0 {
		t.Errorf("model calls = %d, want none", got)
	}
}

func TestABannedCallerIsIgnoredInPrivate(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "не должно прозвучать"}
	h := newHandler(sender, model, handlerOpts{bans: &fakeBouncer{banned: true}})

	msg := telego.Message{
		Chat: telego.Chat{ID: 111, Type: telego.ChatTypePrivate},
		From: &telego.User{ID: 111},
		Text: "привет",
	}
	if err := h.HandleDirectMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}
	if got := sender.directs(); len(got) != 0 {
		t.Errorf("direct messages = %+v, want silence", got)
	}
}

func TestAnAdminIsNeverBanned(t *testing.T) {
	sender := &fakeSender{}
	model := &fakeModel{answer: "ответ"}
	// A bot whose owner can lock themselves out of it is a bot with a footgun.
	h := newHandler(sender, model, handlerOpts{bans: &fakeBouncer{banned: true}, access: adminGate})

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Errorf("model calls = %d, want the admin served", got)
	}
}

func TestTheUsernameIsRememberedForBansByName(t *testing.T) {
	sender := &fakeSender{}
	bouncer := &fakeBouncer{}
	h := newHandler(sender, &fakeModel{answer: "ответ"}, handlerOpts{bans: bouncer})

	msg := guestMessage("@dr1wbot привет")
	msg.From.Username = "someone"
	if err := h.HandleGuestMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	bouncer.mu.Lock()
	defer bouncer.mu.Unlock()
	// Telegram has no username-to-id lookup, so this is the only directory that
	// can exist: /ban @someone works because the bot saw them speak.
	if len(bouncer.noted) != 1 || bouncer.noted[0] != "someone" || bouncer.notedID[0] != 111 {
		t.Errorf("noted = %v/%v, want the username recorded against the id", bouncer.notedID, bouncer.noted)
	}
}

// streamingModel writes an answer in pieces, like a real streamed completion.
type streamingModel struct {
	pieces []string
	// release lets a test hold the stream open long enough for the editor to
	// tick, without sleeping for a fixed time.
	release chan struct{}
}

func (m *streamingModel) Complete(context.Context, llm.Request) (string, error) {
	return strings.Join(m.pieces, ""), nil
}

func (m *streamingModel) Stream(_ context.Context, _ llm.Request, sink llm.Sink) (string, error) {
	var whole strings.Builder
	for _, piece := range m.pieces {
		whole.WriteString(piece)
		sink(llm.Event{Text: whole.String()})
		if m.release != nil {
			<-m.release
		}
	}
	return whole.String(), nil
}

func TestStreamingGrowsTheDraftBeforeTheAnswerIsFinished(t *testing.T) {
	sender := &fakeSender{}
	release := make(chan struct{})
	model := &streamingModel{pieces: []string{"Первое ", "второе ", "третье"}, release: release}

	h := New(Options{
		Sender:        sender,
		Model:         model,
		Access:        allowAll,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername:   "dr1wbot",
		MaxConcurrent: 4,
		MaxReplyRunes: 3500,
		Timeout:       5 * time.Second,
		StreamEvery:   time.Millisecond,
	})
	h.SetStreaming(true)

	done := make(chan error, 1)
	go func() { done <- h.HandleDirectMessage(context.Background(), directMessage(111, "привет")) }()

	// Let each piece through, giving the draft time to carry it into the chat.
	for range model.pieces {
		time.Sleep(10 * time.Millisecond)
		release <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}

	drafts := sender.draftCalls()
	if len(drafts) < 2 {
		t.Fatalf("drafts = %+v, want the answer shown while it is written", drafts)
	}
	// Every draft is one animation step of the same draft, so they share an id.
	for _, d := range drafts {
		if d.draftID != drafts[0].draftID {
			t.Fatalf("draft ids = %+v, want one draft animated in place", drafts)
		}
	}
	// A partial answer is unformatted: half-written Markdown is not valid
	// Markdown, and the finished answer is a different message.
	partial := drafts[len(drafts)-1]
	if partial.text == "" || !strings.HasPrefix("Первое второе третье", partial.text) {
		t.Errorf("last draft = %+v, want a prefix of the answer as plain text", partial)
	}

	rich := sender.richSends()
	if len(rich) != 1 || rich[0].text != "Первое второе третье" {
		t.Errorf("rich messages = %+v, want the finished answer sent once", rich)
	}
}

func TestGuestAnswerIsNeverStreamed(t *testing.T) {
	sender := &fakeSender{}
	model := &streamingModel{pieces: []string{"раз ", "два"}}

	h := New(Options{
		Sender:        sender,
		Model:         model,
		Access:        allowAll,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername:   "dr1wbot",
		MaxConcurrent: 4,
		MaxReplyRunes: 3500,
		Timeout:       time.Second,
		StreamEvery:   time.Millisecond,
	})
	h.SetStreaming(true)

	if err := h.HandleGuestMessage(context.Background(), guestMessage("@dr1wbot привет")); err != nil {
		t.Fatalf("HandleGuestMessage() error = %v", err)
	}

	// Guest mode answers an inline message that drafts cannot address, and
	// editing one over and over is what the drafts replaced. So the guest path
	// waits and answers once.
	_, edits := sender.snapshot()
	if len(edits) != 1 {
		t.Fatalf("edits = %+v, want exactly one: the finished answer", edits)
	}
	if edits[0].markdown != "раз два" {
		t.Errorf("edit = %+v, want the whole answer at once", edits[0])
	}
	if got := sender.draftCalls(); len(got) != 0 {
		t.Errorf("drafts = %+v, want none outside a private chat", got)
	}
}

func TestStreamingCanBeSwitchedOff(t *testing.T) {
	sender := &fakeSender{}
	model := &streamingModel{pieces: []string{"раз ", "два"}}

	h := New(Options{
		Sender:        sender,
		Model:         model,
		Access:        allowAll,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername:   "dr1wbot",
		MaxConcurrent: 4,
		MaxReplyRunes: 3500,
		Timeout:       time.Second,
		StreamEvery:   time.Millisecond,
	})
	h.SetStreaming(false)

	if err := h.HandleDirectMessage(context.Background(), directMessage(111, "привет")); err != nil {
		t.Fatalf("HandleDirectMessage() error = %v", err)
	}

	// The draft still says "thinking" — that is the placeholder, not streaming
	// — but no half-written answer is ever shown.
	for _, d := range sender.draftCalls() {
		if d.text != "" {
			t.Errorf("draft = %+v, want no partial answer with streaming off", d)
		}
	}
	rich := sender.richSends()
	if len(rich) != 1 || rich[0].text != "раз два" {
		t.Errorf("rich messages = %+v, want the whole answer at once", rich)
	}
}

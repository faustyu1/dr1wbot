// Package llm talks to Google AI Studio through its OpenAI-compatible chat
// completions endpoint.
//
// Three things shape this client. Free-tier quota is metered per key, so it
// holds a ring of keys and moves to the next one on 429. A quota is also
// metered per model, so it holds a list of models: when every key is spent on
// the first model, the whole ring is tried again on the next one before giving
// up. And a model only knows what it was trained on, so it is handed a search
// tool and told today's date — an answer about this week is otherwise confident
// and wrong.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"dr1wbot/internal/keyring"
)

// ErrRateLimited means every key and model refused because a quota is
// exhausted, not because anything is wrong with the request. The advice differs
// from a generic failure: wait for the reset or add a key, do not retry now.
var ErrRateLimited = errors.New("rate limited")

// errOverloaded marks a failure the provider says is temporary (5xx). Worth
// another key or model, but it is not a quota problem, so it never surfaces as
// ErrRateLimited.
var errOverloaded = errors.New("provider temporarily unavailable")

// Turn is one earlier message in the conversation.
type Turn struct {
	Role    string // "user" or "assistant"
	Content string
}

// Request is one call to the model.
type Request struct {
	Prompt string
	Images [][]byte
	// History is earlier turns, oldest first. Guest mode carries none, so the
	// caller supplies whatever it remembered itself.
	History []Turn
	// System replaces the client's system prompt for this one request. Empty
	// keeps the configured one.
	System string
	// MaxTokens caps this one request's budget below the configured one. Zero
	// keeps the configured one. It exists so a public caller can be given a
	// smaller answer than an admin without a second client.
	MaxTokens int
}

// Event is what a streaming caller is told while the answer is being written.
// Exactly one of the two fields is set: Searching while a lookup is running,
// Text for everything the model has written so far.
type Event struct {
	// Text is the whole answer as it stands, not the newest fragment. Telegram
	// edits a message to its full new contents, so a delta would only have to
	// be reassembled by every caller.
	Text string
	// Searching carries the query the model asked to look up. A round of tool
	// calls throws away whatever text preceded it, so the caller is told what
	// is happening rather than watching a half-sentence vanish.
	Searching string
}

// Sink receives streaming events. It is called from the goroutine that reads
// the response, so it must not block for long.
type Sink func(Event)

// Completer produces an answer for one request.
type Completer interface {
	Complete(ctx context.Context, req Request) (string, error)
}

// Searcher looks something up on the web and returns what the model should
// read. A nil one leaves the model with nothing but its training data.
type Searcher interface {
	Lookup(ctx context.Context, query string) (string, error)
}

// Client is an OpenAI-compatible chat completions client.
type Client struct {
	http            *http.Client
	baseURL         string
	keys            *keyring.Ring
	models          []string
	systemPrompt    string
	maxTokens       int
	reasoningEffort string
	search          Searcher
	now             func() time.Time

	// knobs the admin panel flips while requests are in flight.
	policy    sync.RWMutex
	searchOff bool

	// counters back the admin menu. The provider exposes no quota endpoint, so
	// what the bot has observed itself is the only picture of the day there is.
	mu       sync.Mutex
	requests int
	answers  map[string]int // per model
	quotaOut int            // requests that ran out of keys and models
	failures int
	searches int
}

// Options configures a Client.
type Options struct {
	BaseURL string
	// APIKeys is the rotation pool. One key is a pool of one.
	APIKeys []string
	// KeyCooldown is how long a key that hit a quota is skipped for.
	KeyCooldown time.Duration
	// Models are tried in order, each with the whole key pool.
	Models       []string
	SystemPrompt string
	MaxTokens    int
	// ReasoningEffort maps to Gemini's thinking budget. Empty leaves it to the
	// provider. Thinking tokens are billed against MaxTokens, so a low effort is
	// what keeps a chat-sized budget from being spent before the answer starts.
	ReasoningEffort string
	Timeout         time.Duration
	// Search gives the model a way to look things up. Nil switches the tool off
	// entirely, which is not the same as SetSearchEnabled(false): that one is
	// reversible from the panel.
	Search     Searcher
	HTTPClient *http.Client // optional; tests inject their own
	Now        func() time.Time
}

// New builds a Client. Timeout bounds a whole request including connection setup.
func New(opts Options) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	cooldown := opts.KeyCooldown
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Client{
		http:            httpClient,
		baseURL:         strings.TrimRight(opts.BaseURL, "/"),
		keys:            keyring.New(opts.APIKeys, cooldown),
		models:          opts.Models,
		systemPrompt:    opts.SystemPrompt,
		maxTokens:       opts.MaxTokens,
		reasoningEffort: opts.ReasoningEffort,
		search:          opts.Search,
		now:             opts.Now,
		answers:         make(map[string]int),
	}
}

// SetSearchEnabled turns the web tool on and off without a restart. It does
// nothing when no searcher was configured in the first place.
func (c *Client) SetSearchEnabled(on bool) {
	c.policy.Lock()
	defer c.policy.Unlock()
	c.searchOff = !on
}

// SearchAvailable reports whether the model can look things up right now.
func (c *Client) SearchAvailable() bool {
	c.policy.RLock()
	defer c.policy.RUnlock()
	return c.search != nil && !c.searchOff
}

// ModelStat is one model's share of the answers.
type ModelStat struct {
	Name    string
	Answers int
}

// Stats is what the admin menu shows. Everything in it is observed, not asked
// for: Google exposes no endpoint for "how much of my free tier is left".
type Stats struct {
	Keys keyring.Stats
	// Models are in fallback order, so the first one carrying no answers while
	// the second does is the visible sign that the day's first quota is gone.
	Models   []ModelStat
	Requests int
	QuotaOut int // requests that exhausted every key on every model
	Failures int
	Searches int // how many times the model looked something up
}

// Stats reports what this process has seen since it started.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := Stats{
		Keys:     c.keys.Stats(),
		Requests: c.requests,
		QuotaOut: c.quotaOut,
		Failures: c.failures,
		Searches: c.searches,
		Models:   make([]ModelStat, 0, len(c.models)),
	}
	for _, m := range c.models {
		out.Models = append(out.Models, ModelStat{Name: m, Answers: c.answers[m]})
	}
	return out
}

func (c *Client) recordAnswer(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.answers[model]++
}

func (c *Client) recordFailure(quotaOut bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.failures++
	if quotaOut {
		c.quotaOut++
	}
}

func (c *Client) recordSearch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.searches++
}

// message carries either a plain string or, when pictures are attached, the
// multipart content array that vision models expect. A tool round adds two more
// shapes: the assistant's request to call something, and the result of that
// call coming back.
type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	Index    int          `json:"-"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
	// ExtraContent is whatever the backend hangs off the call outside the
	// OpenAI schema. Gemini's thinking models put a signed record of the
	// reasoning that led to the call there and reject the next round with a
	// 400 if it does not come back untouched, so it is carried through
	// verbatim rather than parsed.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// userContent builds the user message body: a bare string when there is
// nothing attached, the multipart form when there is.
func userContent(prompt string, images [][]byte) any {
	if len(images) == 0 {
		return prompt
	}

	parts := make([]contentPart, 0, len(images)+1)
	if prompt != "" {
		parts = append(parts, contentPart{Type: "text", Text: prompt})
	}
	for _, img := range images {
		if len(img) == 0 {
			continue
		}
		mime, _, _ := strings.Cut(http.DetectContentType(img), ";")
		parts = append(parts, contentPart{
			Type:     "image_url",
			ImageURL: &imageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)},
		})
	}
	return parts
}

// tool describes a function the model may call.
type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// searchToolName is what the model calls to look something up.
const searchToolName = "web_search"

// searchTool is the only tool the bot offers. It is described in the terms the
// model has to decide by — "you do not know this, go and look" — because a
// vaguer description gets it called either never or on every "привет".
var searchTool = tool{
	Type: "function",
	Function: toolFunction{
		Name: searchToolName,
		Description: "Ищет в интернете через DuckDuckGo и возвращает свежие результаты со ссылками. " +
			"Вызывай, когда ответ зависит от того, что происходит сейчас или изменилось после обучения: " +
			"новости, события, даты, цены, курсы, погода, версии программ, состав команд, кто чем занят сегодня. " +
			"Также вызывай, если не уверен в факте и его можно проверить. Не вызывай для общих знаний, " +
			"объяснений, математики и кода.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type": "string",
					"description": "Поисковый запрос на языке источников: ключевые слова, а не вопрос целиком. " +
						"Для местных тем — на языке вопроса.",
				},
			},
			"required": []string{"query"},
		},
	},
}

type completionRequest struct {
	Model           string    `json:"model"`
	Messages        []message `json:"messages"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Stream          bool      `json:"stream"`
	Tools           []tool    `json:"tools,omitempty"`
	ToolChoice      string    `json:"tool_choice,omitempty"`
}

type completionResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// streamChunk is one server-sent event of a streamed completion.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index        int             `json:"index"`
				ID           string          `json:"id"`
				Type         string          `json:"type"`
				Function     functionCall    `json:"function"`
				ExtraContent json.RawMessage `json:"extra_content"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// reply is one round's outcome: either text, or a request to call something.
type reply struct {
	content      string
	toolCalls    []toolCall
	finishReason string
}

// errBodyLimit caps how much of a failed response we read into an error message.
const errBodyLimit = 512

// maxToolRounds bounds how many times the model may search before it has to
// answer. Two rounds cover "look it up, then look up what that turned out to
// be"; more than that is a loop, and a loop here is billed per turn.
const maxToolRounds = 2

// Complete sends one prompt and returns the assistant's text.
func (c *Client) Complete(ctx context.Context, req Request) (string, error) {
	return c.converse(ctx, req, nil)
}

// Stream is Complete with the answer reported as it is written. The returned
// string is the whole answer, so a caller that also wants the final text does
// not have to assemble it from the events.
func (c *Client) Stream(ctx context.Context, req Request, sink Sink) (string, error) {
	return c.converse(ctx, req, sink)
}

// converse runs one exchange to its end: the model is called, and if it asks to
// search, the search runs and the model is called again with the results, until
// it answers or runs out of rounds.
func (c *Client) converse(ctx context.Context, req Request, sink Sink) (string, error) {
	if len(c.models) == 0 {
		return "", fmt.Errorf("no model configured")
	}
	if c.keys.Len() == 0 {
		return "", fmt.Errorf("no api key configured")
	}

	searcher := c.searcher()
	messages := c.conversation(req, searcher != nil)

	maxTokens := c.maxTokens
	if req.MaxTokens > 0 && req.MaxTokens < maxTokens {
		maxTokens = req.MaxTokens
	}

	var tools []tool
	if searcher != nil {
		tools = []tool{searchTool}
	}

	for round := 0; ; round++ {
		// The last round is asked for prose only: leaving the tool in would
		// invite a call we have no budget left to answer.
		offered := tools
		if round >= maxToolRounds {
			offered = nil
		}

		answer, err := c.round(ctx, messages, offered, maxTokens, sink)
		if err != nil {
			return "", err
		}
		if len(answer.toolCalls) == 0 {
			text := strings.TrimSpace(answer.content)
			if text == "" {
				return "", emptyAnswer(answer.finishReason)
			}
			return text, nil
		}

		messages = append(messages, message{Role: "assistant", ToolCalls: answer.toolCalls})
		for _, call := range answer.toolCalls {
			messages = append(messages, message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    c.runTool(ctx, searcher, call, sink),
			})
		}
	}
}

// searcher is the lookup tool if one is configured and switched on.
func (c *Client) searcher() Searcher {
	c.policy.RLock()
	defer c.policy.RUnlock()
	if c.searchOff {
		return nil
	}
	return c.search
}

// runTool answers one tool call. A failed search is reported to the model as
// text rather than as an error: "I could not look that up" is something it can
// work with, and losing the whole answer over a scraped page is not.
func (c *Client) runTool(ctx context.Context, searcher Searcher, call toolCall, sink Sink) string {
	if call.Function.Name != searchToolName || searcher == nil {
		return "Инструмент недоступен."
	}

	var args struct {
		Query string `json:"query"`
	}
	// A model that emits malformed arguments still meant to search something;
	// the raw string is a better query than giving up.
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args.Query == "" {
		args.Query = strings.TrimSpace(call.Function.Arguments)
	}
	if args.Query == "" {
		return "Пустой запрос — искать нечего."
	}

	if sink != nil {
		sink(Event{Searching: args.Query})
	}
	c.recordSearch()

	found, err := searcher.Lookup(ctx, args.Query)
	if err != nil {
		return fmt.Sprintf("Поиск по запросу %q не удался: %s. Отвечай по тому, что знаешь, "+
			"и предупреди, что данные могут быть неактуальны.", args.Query, err)
	}
	return found
}

// conversation assembles the messages one exchange starts from.
func (c *Client) conversation(req Request, searching bool) []message {
	system := c.systemPrompt
	if req.System != "" {
		system = req.System
	}
	system = strings.TrimSpace(system + "\n\n" + c.groundingNote(searching))

	messages := make([]message, 0, len(req.History)+2)
	if system != "" {
		messages = append(messages, message{Role: "system", Content: system})
	}
	for _, t := range req.History {
		if t.Content == "" {
			continue
		}
		messages = append(messages, message{Role: t.Role, Content: t.Content})
	}
	return append(messages, message{Role: "user", Content: userContent(req.Prompt, req.Images)})
}

// weekdays name the current day in the language the bot answers in.
var weekdays = [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}

var months = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря"}

// groundingNote is appended to every system prompt. The date is there because a
// model asked "what year is it" answers with the year it was trained in, and
// the search rule is there because a model that has a tool still has to be told
// that not knowing is a reason to use it.
func (c *Client) groundingNote(searching bool) string {
	now := c.now().UTC()
	date := fmt.Sprintf("%s, %d %s %d года (UTC)",
		weekdays[int(now.Weekday())], now.Day(), months[int(now.Month())-1], now.Year())

	note := "Сегодня " + date + ". Твои знания об этом мире заканчиваются раньше — " +
		"не выдавай устаревшее за текущее и не считай последним то, что было последним на момент обучения."
	if !searching {
		return note + " Доступа к интернету сейчас нет: если ответ зависит от свежих данных, скажи об этом прямо."
	}
	return note + " У тебя есть инструмент " + searchToolName + " — поиск в интернете. " +
		"Используй его молча, без фраз «сейчас поищу», и отвечай уже по найденному. " +
		"Ссылайся на источники, если они важны для ответа; не выдумывай ссылки, которых не было в результатах."
}

// round is one call to the model: every model in order, and for each the whole
// key pool, so a spent quota costs a retry rather than the answer.
func (c *Client) round(ctx context.Context, messages []message, tools []tool, maxTokens int, sink Sink) (reply, error) {
	var lastErr error
	quotaHit := false

	for _, model := range c.models {
		request := completionRequest{
			Model:           model,
			Messages:        messages,
			MaxTokens:       maxTokens,
			ReasoningEffort: c.reasoningEffort,
			Stream:          sink != nil,
		}
		if len(tools) > 0 {
			request.Tools = tools
			request.ToolChoice = "auto"
		}
		payload, err := json.Marshal(request)
		if err != nil {
			return reply{}, fmt.Errorf("encode request: %w", err)
		}

		for _, lease := range c.keys.Lease() {
			answer, err := c.call(ctx, model, lease.Key, payload, sink)
			if err == nil {
				c.keys.Works(lease.Index)
				// A tool round is not an answer: counting it would report two
				// answers for one question.
				if len(answer.toolCalls) == 0 {
					c.recordAnswer(model)
				}
				return answer, nil
			}
			lastErr = err

			switch {
			case errors.Is(err, ErrRateLimited):
				c.keys.Limit(lease.Index)
				quotaHit = true
			case errors.Is(err, errOverloaded):
				// Another key may land on a healthier backend; nothing to park.
			default:
				c.recordFailure(false)
				return reply{}, err // a broken request stays broken on every key
			}
			if ctx.Err() != nil {
				c.recordFailure(false)
				return reply{}, lastErr // out of time, not out of options
			}
		}
	}

	c.recordFailure(quotaHit)
	if quotaHit {
		return reply{}, errors.Join(ErrRateLimited, lastErr)
	}
	return reply{}, lastErr
}

// emptyAnswer explains a response that carried no text.
func emptyAnswer(finishReason string) error {
	if finishReason == "length" {
		// Gemini bills thinking against max_tokens, so a budget that is too
		// small is spent before a single word of the answer is emitted.
		return fmt.Errorf("llm spent the whole token budget on thinking and returned nothing: raise LLM_MAX_TOKENS")
	}
	return fmt.Errorf("llm returned an empty answer")
}

// KeyProbe is what one key answered when asked what it can do.
type KeyProbe struct {
	// Index is the key's position in the pool. Keys are never shown, only
	// counted: a panel screenshot must not leak one.
	Index int
	// Models is how many models the key may use. Zero with no error means the
	// key works but has nothing enabled.
	Models int
	// Err is set when the probe failed, and its message is what Google said.
	Err error
}

// Probe asks every key what models it can reach. Google publishes no endpoint
// for "how much of my quota is left" — the models list is the closest thing to
// a health check that exists, and it is free, so it answers the only question
// the list can: which keys are actually alive.
func (c *Client) Probe(ctx context.Context) []KeyProbe {
	leases := c.keys.Lease()
	out := make([]KeyProbe, 0, len(leases))
	for _, lease := range leases {
		models, err := c.listModels(ctx, lease.Key)
		out = append(out, KeyProbe{Index: lease.Index, Models: models, Err: err})
	}
	// Lease order follows the rotation cursor; the panel reads better in a
	// fixed order.
	slices.SortFunc(out, func(a, b KeyProbe) int { return a.Index - b.Index })
	return out
}

// listModels counts what one key may use.
func (c *Client) listModels(ctx context.Context, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
		return 0, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	return len(parsed.Data), nil
}

// probeBodyLimit is shorter than errBodyLimit: a probe failure goes into a
// menu screen, not a log line.
const probeBodyLimit = 160

// call is one attempt: one model, one key.
func (c *Client) call(ctx context.Context, model, apiKey string, payload []byte, sink Sink) (reply, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return reply{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	if sink != nil {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return reply{}, fmt.Errorf("call %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return reply{}, statusError(model, resp)
	}
	if sink != nil {
		return readStream(resp.Body, sink)
	}
	return readWhole(resp.Body)
}

// readWhole parses an ordinary, non-streamed completion.
func readWhole(body io.Reader) (reply, error) {
	var parsed completionResponse
	if err := json.NewDecoder(body).Decode(&parsed); err != nil {
		return reply{}, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Error != nil {
		return reply{}, fmt.Errorf("llm error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return reply{}, fmt.Errorf("llm returned no choices")
	}
	choice := parsed.Choices[0]
	return reply{
		content:      choice.Message.Content,
		toolCalls:    choice.Message.ToolCalls,
		finishReason: choice.FinishReason,
	}, nil
}

// streamBufferLimit is the longest single server-sent event we accept. A chunk
// is a fragment of a sentence; a megabyte of it is a broken stream.
const streamBufferLimit = 1 << 20

// readStream consumes a server-sent event stream, reporting the answer as it
// grows and reassembling any tool call the model asks for. Tool-call arguments
// arrive split across chunks, keyed by index, which is why they are accumulated
// into a map rather than appended.
func readStream(body io.Reader, sink Sink) (reply, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 8192), streamBufferLimit)

	var text strings.Builder
	calls := make(map[int]*toolCall)
	out := reply{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		payload, found := strings.CutPrefix(line, "data:")
		if !found {
			continue // comments, blank lines and event: fields
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // a chunk we cannot read is not worth losing the answer over
		}
		if chunk.Error != nil {
			return reply{}, fmt.Errorf("llm error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			out.finishReason = choice.FinishReason
		}

		for _, delta := range choice.Delta.ToolCalls {
			call, ok := calls[delta.Index]
			if !ok {
				call = &toolCall{Index: delta.Index, Type: "function"}
				calls[delta.Index] = call
			}
			if delta.ID != "" {
				call.ID = delta.ID
			}
			if delta.Type != "" {
				call.Type = delta.Type
			}
			if delta.Function.Name != "" {
				call.Function.Name = delta.Function.Name
			}
			if len(delta.ExtraContent) > 0 {
				call.ExtraContent = delta.ExtraContent
			}
			call.Function.Arguments += delta.Function.Arguments
		}

		if choice.Delta.Content != "" {
			text.WriteString(choice.Delta.Content)
			sink(Event{Text: text.String()})
		}
	}
	if err := scanner.Err(); err != nil {
		return reply{}, fmt.Errorf("read stream: %w", err)
	}

	out.content = text.String()
	out.toolCalls = collect(calls)
	return out, nil
}

// collect flattens the accumulated tool calls back into request order.
func collect(calls map[int]*toolCall) []toolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]toolCall, 0, len(calls))
	for _, call := range calls {
		if call.Function.Name == "" {
			continue // never completed; calling it would be a guess
		}
		if call.ID == "" {
			// Some backends omit the id when there is only one call, but the
			// result message has to reference something.
			call.ID = fmt.Sprintf("call_%d", call.Index)
		}
		out = append(out, *call)
	}
	slices.SortFunc(out, func(a, b toolCall) int { return a.Index - b.Index })
	return out
}

// statusError turns a non-200 response into an error, marking the two cases
// worth retrying on another key or model.
func statusError(model string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	err := fmt.Errorf("llm %s returned %s: %s", model, resp.Status, strings.TrimSpace(string(snippet)))
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusPaymentRequired:
		return errors.Join(ErrRateLimited, err)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return errors.Join(errOverloaded, err)
	}
	return err
}

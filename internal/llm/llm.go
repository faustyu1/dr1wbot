// Package llm talks to an OpenAI-compatible chat completions endpoint (by
// default OpenRouter) through its chat completions API.
//
// Two things shape this client. Rate limits are metered per key, so it holds
// a ring of keys and moves to the next one on 429. And a quota is also metered
// per model, so it holds a list of models: when every key is spent on the first
// model, the whole ring is tried again on the next one before giving up.
package llm

import (
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

// Completer produces an answer for one request.
type Completer interface {
	Complete(ctx context.Context, req Request) (string, error)
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

	// counters back the admin menu. The provider exposes no quota endpoint, so
	// what the bot has observed itself is the only picture of the day there is.
	mu       sync.Mutex
	requests int
	answers  map[string]int // per model
	quotaOut int            // requests that ran out of keys and models
	failures int

	// modelsMu guards the models slice so it can be swapped at runtime from
	// the admin menu without stopping the bot.
	modelsMu sync.RWMutex

	// promptMu guards the system prompt so it can be swapped at runtime when
	// the operator switches between presets in the admin panel.
	promptMu      sync.RWMutex
	promptBacking string
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
	// ReasoningEffort maps to the model's thinking budget. Empty leaves it to
	// the provider. Thinking tokens are billed against MaxTokens, so a low effort
	// is what keeps a chat-sized budget from being spent before the answer starts.
	ReasoningEffort string
	Timeout         time.Duration
	HTTPClient      *http.Client // optional; tests inject their own
}

// SystemPrompt returns the client's configured system prompt. The tool loop
// needs it because Chat sends raw messages and does not inject one itself,
// while Complete does so internally.
func (c *Client) SystemPrompt() string {
	c.promptMu.RLock()
	defer c.promptMu.RUnlock()
	return c.promptBacking
}

// SetSystemPrompt replaces the system prompt at runtime, used when the operator
// switches between presets in the admin panel. Thread-safe.
func (c *Client) SetSystemPrompt(prompt string) {
	c.promptMu.Lock()
	c.promptBacking = prompt
	c.promptMu.Unlock()
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
	c := &Client{
		http:            httpClient,
		baseURL:         strings.TrimRight(opts.BaseURL, "/"),
		keys:            keyring.New(opts.APIKeys, cooldown),
		models:          opts.Models,
		systemPrompt:    opts.SystemPrompt,
		maxTokens:       opts.MaxTokens,
		reasoningEffort: opts.ReasoningEffort,
		answers:         make(map[string]int),
	}
	c.promptBacking = opts.SystemPrompt
	return c
}

// ModelStat is one model's share of the answers.
type ModelStat struct {
	Name    string
	Answers int
}

// Stats is what the admin menu shows. Everything in it is observed, not asked
// for: OpenRouter exposes no endpoint for "how much of my quota is left".
type Stats struct {
	Keys keyring.Stats
	// Models are in fallback order, so the first one carrying no answers while
	// the second does is the visible sign that the day's first quota is gone.
	Models   []ModelStat
	Requests int
	QuotaOut int // requests that exhausted every key on every model
	Failures int
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

// toolCall is a function call the model requested.
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

// toolCallFunc carries the name and raw-JSON arguments of one call.
type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolDef is a function definition the model may call.
type toolDef struct {
	Type     string         `json:"type"`
	Function toolDefinition `json:"function"`
}

// toolDefinition describes one callable function to the model.
type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// message carries either a plain string or, when pictures are attached, the
// multipart content array that vision models expect. For the tool-calling loop
// it also carries tool_calls (assistant) and tool_call_id (tool result).
type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
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

type completionRequest struct {
	Model           string    `json:"model"`
	Messages        []message `json:"messages"`
	MaxTokens       int       `json:"max_completion_tokens,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Stream          bool      `json:"stream"`
	Tools           []toolDef `json:"tools,omitempty"`
}

type completionResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			Refusal   string     `json:"refusal"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// errBodyLimit caps how much of a failed response we read into an error message.
const errBodyLimit = 512

// Complete sends one prompt and returns the assistant's text. It walks the
// models in order and, for each, the key pool, so a spent quota costs a retry
// rather than the answer.
func (c *Client) Complete(ctx context.Context, req Request) (string, error) {
	models := c.ActiveModels()
	if len(models) == 0 {
		return "", fmt.Errorf("no model configured")
	}
	if c.keys.Len() == 0 {
		return "", fmt.Errorf("no api key configured")
	}

	system := c.SystemPrompt()
	if req.System != "" {
		system = req.System
	}

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
	messages = append(messages, message{Role: "user", Content: userContent(req.Prompt, req.Images)})

	maxTokens := c.maxTokens
	if req.MaxTokens > 0 && req.MaxTokens < maxTokens {
		maxTokens = req.MaxTokens
	}

	var lastErr error
	quotaHit := false
	for _, model := range models {
		payload, err := json.Marshal(completionRequest{
			Model:           model,
			Messages:        messages,
			MaxTokens:       maxTokens,
			ReasoningEffort: c.reasoningEffort,
			Stream:          false,
		})
		if err != nil {
			return "", fmt.Errorf("encode request: %w", err)
		}

		for _, lease := range c.keys.Lease() {
			answer, err := c.call(ctx, model, lease.Key, payload)
			if err == nil {
				c.keys.Works(lease.Index)
				c.recordAnswer(model)
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
				return "", err // a broken request stays broken on every key
			}
			if ctx.Err() != nil {
				c.recordFailure(false)
				return "", lastErr // out of time, not out of options
			}
		}
	}

	c.recordFailure(quotaHit)
	if quotaHit {
		return "", errors.Join(ErrRateLimited, lastErr)
	}
	return "", lastErr
}

// KeyProbe is what one key answered when asked what it can do.
type KeyProbe struct {
	// Index is the key's position in the pool. Keys are never shown, only
	// counted: a panel screenshot must not leak one.
	Index int
	// Models is how many models the key may use. Zero with no error means the
	// key works but has nothing enabled.
	Models int
	// Err is set when the probe failed, and its message is what the provider said.
	Err error
}

// Probe asks every key what models it can reach. The provider publishes no
// endpoint for "how much of my quota is left" — the models list is the closest
// thing to a health check that exists, and it is free, so it answers the only
// question the list can: which keys are actually alive.
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

// callRaw sends one request and returns the parsed response. It is shared by
// call (which extracts text) and Chat (which also needs tool calls).
func (c *Client) callRaw(ctx context.Context, model, apiKey string, payload []byte) (completionResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return completionResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return completionResponse{}, fmt.Errorf("call %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return completionResponse{}, statusError(model, resp)
	}

	var parsed completionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return completionResponse{}, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Error != nil {
		return completionResponse{}, fmt.Errorf("llm error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return completionResponse{}, fmt.Errorf("llm returned no choices")
	}
	return parsed, nil
}

// errToolCallsNotText means the model asked to call a function, but Complete —
// which never sends tools — was used instead of Chat. The caller should switch
// to Chat or disable tools on the model side.
var errToolCallsNotText = errors.New("model returned tool calls; use Chat to handle them")

// call is one attempt: one model, one key. It extracts the text answer; Chat
// uses callRaw when it also needs tool calls.
func (c *Client) call(ctx context.Context, model, apiKey string, payload []byte) (string, error) {
	parsed, err := c.callRaw(ctx, model, apiKey, payload)
	if err != nil {
		return "", err
	}

	answer := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if answer == "" {
		if len(parsed.Choices[0].Message.ToolCalls) > 0 {
			return "", errToolCallsNotText
		}
		if refusal := strings.TrimSpace(parsed.Choices[0].Message.Refusal); refusal != "" {
			return "", fmt.Errorf("model refused: %s", refusal)
		}
		if parsed.Choices[0].FinishReason == "length" {
			// Some models bill thinking against max_completion_tokens, so a budget
			// that is too small is spent before a single word of the answer is emitted.
			return "", fmt.Errorf("llm spent the whole token budget on thinking and returned nothing: raise LLM_MAX_TOKENS")
		}
		return "", fmt.Errorf("llm returned an empty answer")
	}
	return answer, nil
}

// statusError turns a non-200 response into an error, marking the two cases
// worth retrying on another key or model.
func statusError(model string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	err := fmt.Errorf("llm %s returned %s: %s", model, resp.Status, strings.TrimSpace(string(snippet)))
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusPaymentRequired, http.StatusUnauthorized:
		// 401 is per-key (a revoked or expired credential), not per-request,
		// so parking the key and advancing the cursor lets the rest of the
		// pool keep answering instead of bricking the bot on one dead key.
		return errors.Join(ErrRateLimited, err)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return errors.Join(errOverloaded, err)
	}
	return err
}

// ActiveModels returns a snapshot of the configured model list. Thread-safe.
func (c *Client) ActiveModels() []string {
	c.modelsMu.RLock()
	defer c.modelsMu.RUnlock()
	out := make([]string, len(c.models))
	copy(out, c.models)
	return out
}

// SetModels replaces the active model list at runtime. Thread-safe.
func (c *Client) SetModels(models []string) {
	c.modelsMu.Lock()
	c.models = models
	c.modelsMu.Unlock()
}

// ListModels fetches the available model IDs from the /models endpoint.
// Uses the first available key.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	leases := c.keys.Lease()
	if len(leases) == 0 {
		return nil, fmt.Errorf("no api key configured")
	}
	apiKey := leases[0].Key

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// TestModel sends a minimal "hi" request to one model and returns nil on success.
func (c *Client) TestModel(ctx context.Context, modelID string) error {
	leases := c.keys.Lease()
	if len(leases) == 0 {
		return fmt.Errorf("no api key configured")
	}
	apiKey := leases[0].Key

	payload, err := json.Marshal(completionRequest{
		Model:     modelID,
		Messages:  []message{{Role: "user", Content: "hi"}},
		MaxTokens: 10,
		Stream:    false,
	})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	_, err = c.callRaw(ctx, modelID, apiKey, payload)
	return err
}

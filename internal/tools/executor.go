// Package tools runs the function-calling loop between the LLM and the bot's
// own tools (web search, Telegram channel reading).
//
// An Executor wraps an *llm.Client and implements llm.Completer, so it drops
// into reply.Handler as a drop-in replacement for the bare model. When no tools
// are registered, or when the request carries images (vision path), it simply
// delegates to the underlying Complete — the tool loop costs nothing until the
// model decides to call a function.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"dr1wbot/internal/llm"
	"dr1wbot/internal/search"
	"dr1wbot/internal/tgchannel"
)

// defaultMaxRounds bounds how many back-and-forth tool exchanges one answer may
// take. Each round is one model call plus N tool executions; five rounds is
// enough for a search-then-read-then-answer chain without letting a confused
// model spin forever.
const defaultMaxRounds = 5

// Executor wraps an LLM client with tool-calling support. It satisfies
// llm.Completer so the handler does not change.
type Executor struct {
	model     *llm.Client
	search    *search.Searcher  // nil disables web_search
	tgreader  *tgchannel.Reader // nil disables telegram_* tools
	maxRounds int
	log       *slog.Logger
}

// Options configures an Executor.
type Options struct {
	Model     *llm.Client
	Search    *search.Searcher  // nil = web search off
	TGReader  *tgchannel.Reader // nil = telegram reading off
	MaxRounds int               // 0 = defaultMaxRounds
	Log       *slog.Logger
}

// New builds an Executor. When neither Search nor TGReader is set, the executor
// is a pass-through and every call goes straight to the model.
func New(opts Options) *Executor {
	maxRounds := opts.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxRounds
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Executor{
		model:     opts.Model,
		search:    opts.Search,
		tgreader:  opts.TGReader,
		maxRounds: maxRounds,
		log:       log,
	}
}

// HasTools reports whether any tool is registered.
func (e *Executor) HasTools() bool {
	return true // fetch_url is always available
}

// Complete implements llm.Completer. It delegates to the bare model when no
// tools are available or when the request carries images (the tool loop does
// not support multipart content). Otherwise it runs the tool-calling loop.
func (e *Executor) Complete(ctx context.Context, req llm.Request) (string, error) {
	if !e.HasTools() || len(req.Images) > 0 {
		return e.model.Complete(ctx, req)
	}
	return e.runLoop(ctx, req)
}

// runLoop executes the function-calling cycle: ask the model, run any tool
// calls it makes, feed the results back, repeat until it returns plain text or
// the round budget is spent.
func (e *Executor) runLoop(ctx context.Context, req llm.Request) (string, error) {
	system := req.System
	if system == "" {
		system = e.model.SystemPrompt()
	}

	messages := llm.BuildMessages(system, req.History, req.Prompt)
	tools := e.toolDefinitions()
	budget := req.MaxTokens

	var sources []string       // collected source URLs from web_search
	userQuestion := req.Prompt // original question for search context

	for round := 0; round < e.maxRounds; round++ {
		chatReq := llm.ChatRequest{
			Messages:  messages,
			Tools:     tools,
			MaxTokens: budget,
		}

		completion, err := e.model.Chat(ctx, chatReq)
		if err != nil {
			return "", err
		}

		// Plain text answer — done.
		if len(completion.ToolCalls) == 0 {
			e.log.Debug("tool loop finished",
				"round", round, "runes", len([]rune(completion.Content)))
			return appendSources(completion.Content, sources), nil
		}

		e.log.Debug("model requested tool calls",
			"round", round, "calls", len(completion.ToolCalls))

		// Append the assistant message that carries the tool calls, then one
		// tool message per call with its result.
		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			Content:   completion.Content,
			ToolCalls: completion.ToolCalls,
		})

		for _, tc := range completion.ToolCalls {
			result := e.executeWithSources(ctx, tc, &sources, userQuestion)
			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}

		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}

	// Loop exhausted: the model kept calling tools. Make one final call
	// without tools so it must produce a text answer from gathered context.
	e.log.Debug("tool loop exhausted, forcing final answer", "rounds", e.maxRounds)
	forcedMessages := append(messages, llm.ChatMessage{
		Role:    "system",
		Content: "Отвечай обычным текстом на основе собранной информации. Не используй никакие инструменты, теги или разметку инструментов. Просто дай развёрнутый ответ пользователю.",
	})
	finalReq := llm.ChatRequest{
		Messages:  forcedMessages,
		MaxTokens: budget,
	}
	final, err := e.model.Chat(ctx, finalReq)
	if err != nil {
		return "", err
	}
	content := stripToolCallMarkup(final.Content)
	if strings.TrimSpace(content) == "" {
		// Model returned empty even without tools — return sources only.
		return appendSources("", sources), nil
	}
	return appendSources(content, sources), nil
}

// executeWithSources runs one tool call, collects source URLs from web_search
// results, and returns the result string. The original user question is included
// as context in search results so the model keeps track of what was actually asked.
func (e *Executor) executeWithSources(ctx context.Context, tc llm.ToolCall, sources *[]string, userQuestion string) string {
	name := tc.Function.Name
	if name == "web_search" || name == "telegram_read_channel" || name == "telegram_read_post" || name == "telegram_search_channel" {
		result := e.execute(ctx, tc)
		// Collect URLs from the result for citation (web search links AND
		// Telegram post links).
		if urls := extractURLs(result); len(urls) > 0 {
			*sources = append(*sources, urls...)
		}
		// Prepend the original question so the model can self-correct if
		// its search query was imprecise (e.g. ambiguous city names).
		if userQuestion != "" {
			result = "[Контекст: пользователь спросил: \"" + userQuestion + "\"]\n" + result
		}
		return result
	}
	return e.execute(ctx, tc)
}

// stripToolCallMarkup removes tool-call-like XML/HTML tags that some models emit
// as plain text instead of using the proper tool-calling API.
func stripToolCallMarkup(s string) string {
	// Remove anything that looks like <tool...>...</tool...> including nested tags.
	// These appear when the model tries to call tools via text instead of the API.
	for {
		start := strings.Index(s, "<tool")
		if start < 0 {
			break
		}
		// Find the matching closing tag - look for </tool
		end := strings.Index(s[start:], "</tool")
		if end < 0 {
			// No closing tag — trim from <tool to end
			s = s[:start]
			break
		}
		// Find the > after </tool...
		closeEnd := strings.Index(s[start+end:], ">")
		if closeEnd < 0 {
			s = s[:start]
			break
		}
		s = s[:start] + s[start+end+closeEnd+1:]
	}
	return strings.TrimSpace(s)
}

// appendSources adds superscript citation links at the end of the text.
func appendSources(text string, sources []string) string {
	if len(sources) == 0 {
		return text
	}
	// Deduplicate URLs while preserving order.
	seen := make(map[string]bool)
	unique := make([]string, 0, len(sources))
	for _, u := range sources {
		if !seen[u] {
			seen[u] = true
			unique = append(unique, u)
		}
	}
	// Limit to 9 sources (¹²³⁴⁵⁶⁷⁸⁹).
	if len(unique) > 9 {
		unique = unique[:9]
	}
	superscripts := []string{"¹", "²", "³", "⁴", "⁵", "⁶", "⁷", "⁸", "⁹"}
	var b strings.Builder
	b.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		b.WriteString("\n\n")
	} else if !strings.HasSuffix(text, "\n\n") {
		b.WriteString("\n")
	}
	for i, u := range unique {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString("[" + superscripts[i] + "](" + u + ")")
	}
	return b.String()
}

// extractURLs pulls HTTP(S) URLs from a string.
var reURL = regexp.MustCompile(`https?://[^\s
]+`)

func extractURLs(s string) []string {
	matches := reURL.FindAllString(s, -1)
	// Trim trailing punctuation that's not part of the URL.
	for i, m := range matches {
		matches[i] = strings.TrimRight(m, ".,);:!")
	}
	return matches
}

// execute runs one tool call and returns its result as a string. Errors are
// returned as plain text so the model can react to them rather than crashing
// the loop.
func (e *Executor) execute(ctx context.Context, tc llm.ToolCall) string {
	name := tc.Function.Name
	args := tc.Function.Arguments

	switch name {
	case "web_search":
		return e.execSearch(ctx, args)
	case "fetch_url":
		return e.execFetchURL(ctx, args)
	case "telegram_read_channel":
		return e.execReadChannel(ctx, args)
	case "telegram_read_post":
		return e.execReadPost(ctx, args)
	case "telegram_search_channel":
		return e.execSearchChannel(ctx, args)
	default:
		return fmt.Sprintf("Error: unknown tool %q", name)
	}
}

// execFetchURL makes an HTTP request and returns status + headers + body.
func (e *Executor) execFetchURL(ctx context.Context, rawArgs string) string {
	args, err := parseArgs(rawArgs)
	if err != nil {
		return err.Error()
	}

	urlStr, _ := args["url"].(string)
	if urlStr == "" {
		return "Error: url is required"
	}

	method := strings.ToUpper(argString(args, "method"))
	if method == "" {
		method = "GET"
	}
	switch method {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "PATCH":
	default:
		return "Error: method must be one of GET, HEAD, POST, PUT, DELETE, PATCH"
	}

	// Body for POST/PUT/PATCH.
	var reqBody io.Reader
	if bodyStr, ok := args["body"].(string); ok && bodyStr != "" {
		reqBody = strings.NewReader(bodyStr)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(fetchCtx, method, urlStr, reqBody)
	if err != nil {
		return fmt.Sprintf("Error building request: %v", err)
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (compatible; dr1wbot/1.0)")

	// Custom headers from the model.
	if headers, ok := args["headers"].(map[string]any); ok {
		for k, v := range headers {
			httpReq.Header.Set(k, fmt.Sprintf("%v", v))
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Sprintf("Error fetching URL: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8000))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("HTTP %d %s\n", resp.StatusCode, resp.Status))
	sb.WriteString("Headers:\n")
	for k, v := range resp.Header {
		sb.WriteString(fmt.Sprintf("  %s: %s\n", k, strings.Join(v, ", ")))
	}
	sb.WriteString("\nBody:\n")
	bodyStr := string(respBody)
	if len(bodyStr) > 4000 {
		bodyStr = bodyStr[:4000] + "\n... (truncated)"
	}
	sb.WriteString(bodyStr)
	return sb.String()
}

// parseArgs unmarshals the JSON arguments string into a map. A malformed blob is
// returned as an error string, not a Go error, so the model sees what went
// wrong and can fix its next call.
func parseArgs(raw string) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return m, nil
}

func argString(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		return fmt.Sprintf("%v", s)
	}
}

func argInt(args map[string]any, key string, fallback int) int {
	v, ok := args[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return fallback
	}
}

// --- web_search ---

func (e *Executor) execSearch(ctx context.Context, rawArgs string) string {
	if e.search == nil {
		return "Error: web search is not configured."
	}
	args, err := parseArgs(rawArgs)
	if err != nil {
		return "Error: " + err.Error()
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: the 'query' parameter is required."
	}
	count := argInt(args, "count", 5)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	results, err := e.search.Search(ctx, query, count)
	if err != nil {
		return "Error: search failed: " + err.Error()
	}
	return search.FormatResults(results)
}

// --- telegram_read_channel ---

func (e *Executor) execReadChannel(ctx context.Context, rawArgs string) string {
	if e.tgreader == nil {
		return "Error: Telegram channel reading is not configured."
	}
	args, err := parseArgs(rawArgs)
	if err != nil {
		return "Error: " + err.Error()
	}
	channel := strings.TrimSpace(argString(args, "channel"))
	if channel == "" {
		return "Error: the 'channel' parameter is required."
	}
	limit := argInt(args, "limit", 10)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	ch, err := e.tgreader.ReadChannel(ctx, channel, limit)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatChannel(ch, ch.Posts)
}

// --- telegram_read_post ---

func (e *Executor) execReadPost(ctx context.Context, rawArgs string) string {
	if e.tgreader == nil {
		return "Error: Telegram channel reading is not configured."
	}
	args, err := parseArgs(rawArgs)
	if err != nil {
		return "Error: " + err.Error()
	}
	postURL := strings.TrimSpace(argString(args, "post_url"))
	if postURL == "" {
		return "Error: the 'post_url' parameter is required."
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	ch, post, err := e.tgreader.ReadPost(ctx, postURL)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatChannel(ch, []tgchannel.Post{*post})
}

// --- telegram_search_channel ---

func (e *Executor) execSearchChannel(ctx context.Context, rawArgs string) string {
	if e.tgreader == nil {
		return "Error: Telegram channel reading is not configured."
	}
	args, err := parseArgs(rawArgs)
	if err != nil {
		return "Error: " + err.Error()
	}
	channel := strings.TrimSpace(argString(args, "channel"))
	if channel == "" {
		return "Error: the 'channel' parameter is required."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: the 'query' parameter is required."
	}
	limit := argInt(args, "limit", 10)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ch, posts, err := e.tgreader.Search(ctx, channel, query, limit)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatChannel(ch, posts)
}

// formatChannel renders a channel and its posts as compact text for the model.
func formatChannel(ch *tgchannel.Channel, posts []tgchannel.Post) string {
	var b strings.Builder
	if ch != nil {
		if ch.Title != "" {
			fmt.Fprintf(&b, "Канал: %s\n", ch.Title)
		}
		if ch.Username != "" {
			fmt.Fprintf(&b, "@%s\n", ch.Username)
		}
		if ch.Counter != "" {
			fmt.Fprintf(&b, "%s\n", ch.Counter)
		}
	}
	if len(posts) == 0 {
		b.WriteString("Постов не найдено.")
		return b.String()
	}
	for i, p := range posts {
		if i > 0 || ch != nil {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "--- Пост %d ---\n", i+1)
		if p.Date != "" {
			fmt.Fprintf(&b, "Дата: %s\n", p.Date)
		}
		if p.Views != "" {
			fmt.Fprintf(&b, "Просмотры: %s\n", p.Views)
		}
		fmt.Fprintf(&b, "Ссылка: %s\n", p.Link)
		if len(p.Media) > 0 {
			fmt.Fprintf(&b, "Медиа: %s\n", strings.Join(p.Media, ", "))
		}
		if p.Text != "" {
			text := p.Text
			if len(text) > 1400 {
				text = text[:1400] + "..."
			}
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// toolDefinitions builds the OpenAI tool list from whatever is configured.
func (e *Executor) toolDefinitions() []llm.Tool {
	var tools []llm.Tool

	if e.search != nil {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolDef{
				Name:        "web_search",
				Description: "Искать актуальную информацию в интернете: новости, погода, факты, свежие данные. Используй когда ответ требует текущих данных или ты не уверен в фактах. Формулируй запрос точно — указывай полный и точный контекст (например полный название города, а не сокращение).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Поисковый запрос на языке, соответствующем вопросу.",
						},
						"count": map[string]any{
							"type":        "integer",
							"description": "Количество результатов (1-5, по умолчанию 5).",
							"default":     5,
						},
					},
					"required": []string{"query"},
				},
			},
		})
	}

	// fetch_url is always available — lets the bot act as an agent that can
	// probe URLs, check API health, read web pages, etc.
	tools = append(tools, llm.Tool{
		Type: "function",
		Function: llm.ToolDef{
			Name:        "fetch_url",
			Description: "Получить содержимое URL (HTTP GET). Используй для проверки доступности API, чтения веб-страниц, получения JSON/XML ответов. Возвращает HTTP-статус, заголовки и тело (до 4000 символов).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "Полный URL включая https:// или http://",
					},
					"method": map[string]any{
						"type":        "string",
						"description": "HTTP метод: GET (по умолчанию), POST, HEAD.",
						"default":     "GET",
					},
					"headers": map[string]any{
						"type":        "object",
						"description": "HTTP заголовки в виде ключ-значение, например: Authorization Bearer sk-1234.",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "Тело запроса для POST/PUT/PATCH (JSON, plain text и т.д.).",
					},
				},
				"required": []string{"url"},
			},
		},
	})

	if e.tgreader != nil {
		tools = append(tools,
			llm.Tool{
				Type: "function",
				Function: llm.ToolDef{
					Name:        "telegram_read_channel",
					Description: "Читать последние посты публичного Telegram-канала по @username или ссылке t.me/.... Без аккаунта, только публичные каналы.",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"channel": map[string]any{
								"type":        "string",
								"description": "@username канала или ссылка t.me/канал.",
							},
							"limit": map[string]any{
								"type":        "integer",
								"description": "Сколько постов вернуть (1-300, по умолчанию 10). Можно запросить до 300 последних постов канала.",
								"default":     10,
							},
						},
						"required": []string{"channel"},
					},
				},
			},
			llm.Tool{
				Type: "function",
				Function: llm.ToolDef{
					Name:        "telegram_read_post",
					Description: "Прочитать один конкретный пост публичного Telegram-канала по прямой ссылке вида https://t.me/канал/123.",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"post_url": map[string]any{
								"type":        "string",
								"description": "Прямая ссылка на пост, например https://t.me/durov/123.",
							},
						},
						"required": []string{"post_url"},
					},
				},
			},
			llm.Tool{
				Type: "function",
				Function: llm.ToolDef{
					Name:        "telegram_search_channel",
					Description: "Искать по ключевым словам в последних постах публичного Telegram-канала. Перебирает несколько страниц превью.",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"channel": map[string]any{
								"type":        "string",
								"description": "@username канала или ссылка t.me/канал.",
							},
							"query": map[string]any{
								"type":        "string",
								"description": "Ключевые слова для поиска в постах канала.",
							},
							"limit": map[string]any{
								"type":        "integer",
								"description": "Сколько результатов вернуть (1-50, по умолчанию 10).",
								"default":     10,
							},
						},
						"required": []string{"channel", "query"},
					},
				},
			},
		)
	}

	return tools
}

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient points a Client at a stub server.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return New(Options{
		BaseURL:      server.URL,
		APIKeys:      []string{"sk-test"},
		Models:       []string{"test-model"},
		SystemPrompt: "будь краток",
		MaxTokens:    256,
		Timeout:      5 * time.Second,
	})
}

func writeAnswer(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]string{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	})
}

func TestCompleteSendsAnOpenAICompatibleRequest(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody completionRequest

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")

		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		writeAnswer(t, w, "  # Ответ  ")
	})

	got, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if got != "# Ответ" {
		t.Errorf("Complete() = %q, want the answer trimmed", got)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.Model != "test-model" {
		t.Errorf("model = %q, want test-model", gotBody.Model)
	}
	if gotBody.MaxTokens != 256 {
		t.Errorf("max_tokens = %d, want 256", gotBody.MaxTokens)
	}
	if gotBody.Stream {
		t.Error("stream = true, want false: the bot edits once with the full answer")
	}
	if len(gotBody.Messages) != 2 {
		t.Fatalf("messages = %+v, want a system and a user message", gotBody.Messages)
	}
	// The grounding note is appended to whatever prompt is configured: a model
	// that does not know today's date answers about "now" with the year it was
	// trained in.
	system, _ := gotBody.Messages[0].Content.(string)
	if gotBody.Messages[0].Role != "system" || !strings.HasPrefix(system, "будь краток") {
		t.Errorf("first message = %+v, want the system prompt first", gotBody.Messages[0])
	}
	if !strings.Contains(system, "Сегодня") {
		t.Errorf("system = %q, want today's date appended", system)
	}
	if gotBody.Messages[1].Role != "user" || gotBody.Messages[1].Content != "вопрос" {
		t.Errorf("second message = %+v, want the user prompt", gotBody.Messages[1])
	}
}

func TestCompleteHonoursAPerRequestSystemPrompt(t *testing.T) {
	var body completionRequest
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeAnswer(t, w, "ок")
	})

	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос", System: "без правил"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("messages = %+v, want a system and a user message", body.Messages)
	}
	system, _ := body.Messages[0].Content.(string)
	if !strings.HasPrefix(system, "без правил") {
		t.Errorf("system = %v, want the per-request override, not the configured prompt", body.Messages[0].Content)
	}
}

func TestCompleteOmitsAnEmptySystemMessage(t *testing.T) {
	var body completionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeAnswer(t, w, "ок")
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: time.Second,
	})
	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	// An empty prompt leaves only the grounding note, which every request
	// carries: it is what keeps "какой сейчас год" from being answered wrong.
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want the grounding note and the user message", body.Messages)
	}
	if system, _ := body.Messages[0].Content.(string); !strings.HasPrefix(system, "Сегодня") {
		t.Errorf("system = %q, want nothing but the grounding note", system)
	}
}

func TestCompleteTrimsTrailingSlashFromBaseURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeAnswer(t, w, "ок")
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL + "/v1/",
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: time.Second,
	})
	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions with no doubled slash", gotPath)
	}
}

func TestCompleteErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantIn  string
	}{
		{
			name: "http error carries the status and body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":"rate limited"}`)
			},
			wantIn: "429",
		},
		{
			name: "structured error field",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"message": "model not found", "type": "invalid_request"},
				})
			},
			wantIn: "model not found",
		},
		{
			name: "no choices",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
			},
			wantIn: "no choices",
		},
		{
			name: "empty answer",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeAnswer(t, w, "   ")
			},
			wantIn: "empty answer",
		},
		{
			name: "malformed json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "not json")
			},
			wantIn: "decode response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTestClient(t, tt.handler).Complete(context.Background(), Request{Prompt: "вопрос"})
			if err == nil {
				t.Fatal("Complete() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantIn)
			}
		})
	}
}

func TestCompleteRespectsContextDeadline(t *testing.T) {
	// The handler must outlive the client's deadline but still return, or
	// httptest's Close would block on it. Cleanups run last-registered-first,
	// so releasing here happens before the server is closed.
	release := make(chan struct{})
	client := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		<-release
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := client.Complete(ctx, Request{Prompt: "вопрос"})
	if err == nil {
		t.Fatal("Complete() error = nil, want a deadline error")
	}
	// reply.Handler distinguishes a timeout from other failures via errors.Is,
	// so the wrapping must preserve it.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// newRotatingClient points a Client with two keys and two models at a stub.
func newRotatingClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"key-one", "key-two"},
		Models:  []string{"model-a", "model-b"},
		Timeout: 5 * time.Second,
	})
}

// attempt is one request the stub saw.
type attempt struct {
	key   string
	model string
}

// record reads the key and model out of a request.
func record(t *testing.T, r *http.Request) attempt {
	t.Helper()
	var body completionRequest
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Errorf("request body is not valid JSON: %v", err)
	}
	return attempt{key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), model: body.Model}
}

func TestCompleteRotatesToTheNextKeyOnRateLimit(t *testing.T) {
	var seen []attempt
	client := newRotatingClient(t, func(w http.ResponseWriter, r *http.Request) {
		got := record(t, r)
		seen = append(seen, got)
		if got.key == "key-one" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
			return
		}
		writeAnswer(t, w, "ответ")
	})

	got, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err != nil {
		t.Fatalf("Complete() error = %v, want the second key to save the answer", err)
	}
	if got != "ответ" {
		t.Errorf("Complete() = %q, want the answer from the second key", got)
	}
	if len(seen) != 2 || seen[0].key != "key-one" || seen[1].key != "key-two" {
		t.Fatalf("attempts = %+v, want key-one then key-two", seen)
	}
	if seen[1].model != "model-a" {
		t.Errorf("second attempt used %q, want the first model retried on the next key", seen[1].model)
	}

	// The spent key stays parked, so the next question starts on the good one.
	seen = nil
	if _, err := client.Complete(context.Background(), Request{Prompt: "ещё"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(seen) != 1 || seen[0].key != "key-two" {
		t.Errorf("attempts = %+v, want the next request to go straight to key-two", seen)
	}
}

func TestCompleteFallsBackToTheNextModelWhenEveryKeyIsSpent(t *testing.T) {
	var seen []attempt
	client := newRotatingClient(t, func(w http.ResponseWriter, r *http.Request) {
		got := record(t, r)
		seen = append(seen, got)
		if got.model == "model-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeAnswer(t, w, "ответ")
	})

	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос"}); err != nil {
		t.Fatalf("Complete() error = %v, want the fallback model to answer", err)
	}
	if len(seen) != 3 {
		t.Fatalf("attempts = %+v, want both keys on model-a before model-b", seen)
	}
	if seen[0].model != "model-a" || seen[1].model != "model-a" || seen[2].model != "model-b" {
		t.Errorf("models tried = %+v, want model-a twice then model-b", seen)
	}
}

func TestCompleteReportsRateLimitedWhenEverythingIsSpent(t *testing.T) {
	calls := 0
	client := newRotatingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
	})

	_, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	// reply.Handler tells a spent quota from a broken call via errors.Is.
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("error = %v, want it to wrap ErrRateLimited", err)
	}
	if calls != 4 {
		t.Errorf("calls = %d, want every key tried on every model (2×2)", calls)
	}
}

func TestCompleteDoesNotBurnKeysOnABrokenRequest(t *testing.T) {
	calls := 0
	client := newRotatingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
	})

	_, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	if errors.Is(err, ErrRateLimited) {
		t.Errorf("error = %v, want a 400 reported as itself, not as a quota problem", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1: a request the provider rejects stays rejected on every key", calls)
	}
}

func TestCompleteRetriesAnotherKeyWhenOverloaded(t *testing.T) {
	calls := 0
	client := newRotatingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeAnswer(t, w, "ответ")
	})

	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос"}); err != nil {
		t.Fatalf("Complete() error = %v, want a 503 retried", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want the overloaded attempt retried once", calls)
	}
}

func TestCompleteSendsReasoningEffort(t *testing.T) {
	var body completionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeAnswer(t, w, "ок")
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL:         server.URL,
		APIKeys:         []string{"sk-test"},
		Models:          []string{"gemini-3.6-flash"},
		ReasoningEffort: "low",
		Timeout:         time.Second,
	})
	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if body.ReasoningEffort != "low" {
		t.Errorf("reasoning_effort = %q, want it passed through so thinking does not eat the budget", body.ReasoningEffort)
	}
}

func TestCompleteExplainsAnAnswerLostToThinking(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]string{"role": "assistant", "content": ""},
				"finish_reason": "length",
			}},
		})
	})

	_, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "LLM_MAX_TOKENS") {
		t.Errorf("error = %q, want it to name the setting that fixes it", err)
	}
}

func TestCompleteNeedsKeysAndModels(t *testing.T) {
	if _, err := New(Options{BaseURL: "http://x", Models: []string{"m"}}).
		Complete(context.Background(), Request{Prompt: "q"}); err == nil {
		t.Error("Complete() with no keys = nil error, want a configuration error")
	}
	if _, err := New(Options{BaseURL: "http://x", APIKeys: []string{"k"}}).
		Complete(context.Background(), Request{Prompt: "q"}); err == nil {
		t.Error("Complete() with no models = nil error, want a configuration error")
	}
}

func TestCompleteTruncatesLongErrorBodies(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 10_000))
	})

	_, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	if len(err.Error()) > errBodyLimit+128 {
		t.Errorf("error is %d bytes, want the body snippet capped near %d", len(err.Error()), errBodyLimit)
	}
}

// fakeSearcher stands in for DuckDuckGo.
type fakeSearcher struct {
	queries []string
	answer  string
	err     error
}

func (f *fakeSearcher) Lookup(_ context.Context, query string) (string, error) {
	f.queries = append(f.queries, query)
	if f.err != nil {
		return "", f.err
	}
	return f.answer, nil
}

// writeChunks sends a server-sent event stream, one chunk per line.
func writeChunks(t *testing.T, w http.ResponseWriter, chunks ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range chunks {
		if _, err := io.WriteString(w, "data: "+chunk+"\n\n"); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		t.Fatalf("write done: %v", err)
	}
}

func TestStreamReportsTheAnswerAsItGrows(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeChunks(t,
			w,
			`{"choices":[{"delta":{"content":"Пер"}}]}`,
			`{"choices":[{"delta":{"content":"вое "}}]}`,
			`{"choices":[{"delta":{"content":"слово"},"finish_reason":"stop"}]}`,
		)
	})

	var seen []string
	answer, err := client.Stream(context.Background(), Request{Prompt: "вопрос"}, func(e Event) {
		seen = append(seen, e.Text)
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if answer != "Первое слово" {
		t.Errorf("Stream() = %q, want the assembled answer", answer)
	}
	// Every event carries the whole answer so far, because that is what an edit
	// to a Telegram message needs.
	want := []string{"Пер", "Первое ", "Первое слово"}
	if len(seen) != len(want) {
		t.Fatalf("events = %q, want %q", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, seen[i], want[i])
		}
	}
}

func TestStreamAsksForAStream(t *testing.T) {
	var body completionRequest
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeChunks(t, w, `{"choices":[{"delta":{"content":"ок"},"finish_reason":"stop"}]}`)
	})

	if _, err := client.Stream(context.Background(), Request{Prompt: "вопрос"}, func(Event) {}); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if !body.Stream {
		t.Error("stream = false, want a streamed request")
	}
}

func TestCompleteOffersTheSearchToolWhenOneIsConfigured(t *testing.T) {
	var body completionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeAnswer(t, w, "ок")
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: time.Second,
		Search:  &fakeSearcher{},
	})
	if _, err := client.Complete(context.Background(), Request{Prompt: "что нового"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Function.Name != searchToolName {
		t.Fatalf("tools = %+v, want the search tool offered", body.Tools)
	}
	if body.ToolChoice != "auto" {
		t.Errorf("tool_choice = %q, want auto: the model decides when to look something up", body.ToolChoice)
	}
}

func TestCompleteRunsASearchAndAnswersFromIt(t *testing.T) {
	search := &fakeSearcher{answer: "1. Вышла Go 1.26\nhttps://go.dev"}
	var bodies []completionRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body completionRequest
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)

		if len(bodies) == 1 {
			// The model asks to look something up before answering.
			_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function",`+
				`"function":{"name":"web_search","arguments":"{\"query\":\"последняя версия go\"}"}}]},`+
				`"finish_reason":"tool_calls"}]}`)
			return
		}
		writeAnswer(t, w, "Последняя — Go 1.26.")
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: time.Second,
		Search:  search,
	})

	answer, err := client.Complete(context.Background(), Request{Prompt: "какая последняя версия go"})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if answer != "Последняя — Go 1.26." {
		t.Errorf("Complete() = %q, want the answer written after the search", answer)
	}
	if len(search.queries) != 1 || search.queries[0] != "последняя версия go" {
		t.Fatalf("queries = %q, want the model's own query", search.queries)
	}
	if len(bodies) != 2 {
		t.Fatalf("calls = %d, want the search round and the answer round", len(bodies))
	}

	// The second round carries the tool call and its result, or the model has
	// no idea what it just looked up.
	last := bodies[1].Messages[len(bodies[1].Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "call_1" {
		t.Fatalf("last message = %+v, want the tool result", last)
	}
	if content, _ := last.Content.(string); !strings.Contains(content, "Go 1.26") {
		t.Errorf("tool content = %v, want the search results", last.Content)
	}
	// One question, one answer: a tool round is not an answer.
	if got := client.Stats(); got.Searches != 1 || got.Requests != 1 {
		t.Errorf("stats = %+v, want one request and one search", got)
	}
}

func TestStreamReassemblesASplitToolCall(t *testing.T) {
	search := &fakeSearcher{answer: "нашлось"}
	round := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		round++
		if round == 1 {
			// Arguments arrive split across chunks, keyed by index.
			writeChunks(t, w,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"web_search","arguments":"{\"que"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ry\":\"погода\"}"}}]},"finish_reason":"tool_calls"}]}`,
			)
			return
		}
		writeChunks(t, w, `{"choices":[{"delta":{"content":"Дождь."},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: time.Second,
		Search:  search,
	})

	var searching []string
	answer, err := client.Stream(context.Background(), Request{Prompt: "погода"}, func(e Event) {
		if e.Searching != "" {
			searching = append(searching, e.Searching)
		}
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if answer != "Дождь." {
		t.Errorf("Stream() = %q, want the answer after the lookup", answer)
	}
	if len(search.queries) != 1 || search.queries[0] != "погода" {
		t.Errorf("queries = %q, want the split arguments reassembled", search.queries)
	}
	// The caller is told a lookup is happening, so the chat does not sit on a
	// half-sentence that is about to be thrown away.
	if len(searching) != 1 || searching[0] != "погода" {
		t.Errorf("searching events = %q, want the query announced", searching)
	}
}

func TestSearchCanBeSwitchedOff(t *testing.T) {
	var body completionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeAnswer(t, w, "ок")
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: time.Second,
		Search:  &fakeSearcher{},
	})
	client.SetSearchEnabled(false)

	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(body.Tools) != 0 {
		t.Errorf("tools = %+v, want none once search is off", body.Tools)
	}
	if client.SearchAvailable() {
		t.Error("SearchAvailable() = true after switching search off")
	}
}

func TestAFailedSearchStillProducesAnAnswer(t *testing.T) {
	search := &fakeSearcher{err: errors.New("captcha")}
	round := 0
	var second completionRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		if round == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function",`+
				`"function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &second)
		writeAnswer(t, w, "По памяти:")
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: time.Second,
		Search:  search,
	})

	answer, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err != nil {
		t.Fatalf("Complete() error = %v, want a failed lookup to be survivable", err)
	}
	if answer != "По памяти:" {
		t.Errorf("Complete() = %q, want the model to answer anyway", answer)
	}
	last := second.Messages[len(second.Messages)-1]
	if content, _ := last.Content.(string); !strings.Contains(content, "captcha") {
		t.Errorf("tool content = %v, want the failure explained to the model", last.Content)
	}
}

func TestToolRoundsAreBounded(t *testing.T) {
	search := &fakeSearcher{answer: "ещё результаты"}
	rounds := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rounds++
		if rounds > maxToolRounds {
			writeAnswer(t, w, "хватит")
			return
		}
		// A model that only ever wants to search again would loop forever, and
		// every turn of that loop is billed.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"c","type":"function",`+
			`"function":{"name":"web_search","arguments":"{\"query\":\"ещё\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Models:  []string{"test-model"},
		Timeout: 2 * time.Second,
		Search:  search,
	})

	if _, err := client.Complete(context.Background(), Request{Prompt: "вопрос"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if rounds != maxToolRounds+1 {
		t.Errorf("rounds = %d, want the loop cut off after %d searches", rounds, maxToolRounds)
	}
}

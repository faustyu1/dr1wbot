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
		t.Errorf("max_completion_tokens = %d, want 256", gotBody.MaxTokens)
	}
	if gotBody.Stream {
		t.Error("stream = true, want false: the bot edits once with the full answer")
	}
	if len(gotBody.Messages) != 2 {
		t.Fatalf("messages = %+v, want a system and a user message", gotBody.Messages)
	}
	if gotBody.Messages[0].Role != "system" || gotBody.Messages[0].Content != "будь краток" {
		t.Errorf("first message = %+v, want the system prompt", gotBody.Messages[0])
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
	if body.Messages[0].Content != "без правил" {
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
	// An empty string is not a system prompt; sending one just wastes a message.
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want only the user message", body.Messages)
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
		Models:          []string{"openai/gpt-5.6-terra"},
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

// TestCompleteRotatesToTheNextKeyOn401 verifies that a 401 (revoked or expired
// credential) is treated as a per-key failure: the key is parked and the next
// one is tried, instead of bricking the bot on one dead key.
func TestCompleteRotatesToTheNextKeyOn401(t *testing.T) {
	var seen []attempt
	client := newRotatingClient(t, func(w http.ResponseWriter, r *http.Request) {
		got := record(t, r)
		seen = append(seen, got)
		if got.key == "key-one" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
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

	// A dead key stays parked just like a rate-limited one.
	seen = nil
	if _, err := client.Complete(context.Background(), Request{Prompt: "ещё"}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(seen) != 1 || seen[0].key != "key-two" {
		t.Errorf("attempts = %+v, want the next request to go straight to key-two", seen)
	}
}

// TestCompleteReportsRefusalAsAnError verifies that when the model returns an
// empty content but a non-empty refusal, the error names the refusal so the
// caller can tell it apart from other empty-answer causes.
func TestCompleteReportsRefusalAsAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]string{"role": "assistant", "content": "", "refusal": "I cannot help with that"},
				"finish_reason": "stop",
			}},
		})
	})

	_, err := client.Complete(context.Background(), Request{Prompt: "вопрос"})
	if err == nil {
		t.Fatal("Complete() error = nil, want a refusal error")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %q, want it to mention the refusal", err)
	}
}

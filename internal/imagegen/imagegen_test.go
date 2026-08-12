package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pngBytes is a tiny but structurally real PNG.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Model:   "openai/gpt-image-1",
		Size:    "1024x1024",
		Timeout: 5 * time.Second,
	})
}

// writeImage answers the way the endpoint normally does: raw base64.
func writeImage(t *testing.T, w http.ResponseWriter, b64 string) {
	t.Helper()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": []any{map[string]string{"b64_json": b64}},
	})
}

// writeImageURL answers with a data URI instead, which the compatibility layer
// is free to do.
func writeImageURL(t *testing.T, w http.ResponseWriter, uri string) {
	t.Helper()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": []any{map[string]string{"url": uri}},
	})
}

func TestGenerateReturnsDecodedBytes(t *testing.T) {
	var gotPath, gotAuth string
	var body request

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		writeImage(t, w, base64.StdEncoding.EncodeToString(pngBytes))
	})

	got, err := client.Generate(context.Background(), "кот в скафандре")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if string(got) != string(pngBytes) {
		t.Errorf("Generate() returned %q, want the decoded image", got)
	}
	if gotPath != "/images/generations" {
		t.Errorf("path = %q, want /images/generations", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	if body.Model != "openai/gpt-image-1" {
		t.Errorf("model = %q, want the configured image model", body.Model)
	}
	if body.Prompt != "кот в скафандре" {
		t.Errorf("prompt = %q, want it passed through", body.Prompt)
	}
	if body.N != 1 {
		t.Errorf("n = %d, want a single picture", body.N)
	}
	if body.ResponseFormat != "" {
		t.Errorf("response_format = %q, want empty: gpt-image models return base64 without it", body.ResponseFormat)
	}
	if body.Size != "1024x1024" {
		t.Errorf("size = %q, want the configured size", body.Size)
	}
}

func TestGenerateAcceptsADataURI(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeImageURL(t, w, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes))
	})

	got, err := client.Generate(context.Background(), "кот")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if string(got) != string(pngBytes) {
		t.Errorf("Generate() returned %q, want the decoded image", got)
	}
}

func TestGenerateRotatesKeysWhenAKeyCannotPay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer free-key" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
			return
		}
		writeImage(t, w, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"free-key", "billed-key"},
		Model:   "openai/gpt-image-1",
		Timeout: 5 * time.Second,
	})

	got, err := client.Generate(context.Background(), "кот")
	if err != nil {
		t.Fatalf("Generate() error = %v, want the billed key to draw", err)
	}
	if string(got) != string(pngBytes) {
		t.Errorf("Generate() returned %q, want the decoded image", got)
	}
}

func TestGenerateReportsOutOfCreditsWhenNoKeyCanPay(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := client.Generate(context.Background(), "кот")
	// reply.Handler turns this into "enable billing" rather than "try again".
	if !errors.Is(err, ErrOutOfCredits) {
		t.Errorf("error = %v, want it to wrap ErrOutOfCredits", err)
	}
}

func TestGenerateErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantIn  string
	}{
		{
			name: "http error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusPaymentRequired)
				_, _ = io.WriteString(w, `{"error":"insufficient credits"}`)
			},
			wantIn: "402",
		},
		{
			name: "structured error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"message": "content policy"},
				})
			},
			wantIn: "content policy",
		},
		{
			name: "model refused and drew nothing",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			},
			wantIn: "no picture",
		},
		{
			name: "neither bytes nor url",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeImage(t, w, "")
			},
			wantIn: "empty image url",
		},
		{
			name: "plain url instead of a data uri",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeImageURL(t, w, "https://example.com/cat.png")
			},
			wantIn: "unsupported image url format",
		},
		{
			name: "broken base64",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeImage(t, w, "!!!not base64!!!")
			},
			wantIn: "decode image",
		},
		{
			name: "empty payload",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeImageURL(t, w, "data:image/png;base64,")
			},
			wantIn: "empty",
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
			_, err := newTestClient(t, tt.handler).Generate(context.Background(), "кот")
			if err == nil {
				t.Fatal("Generate() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantIn)
			}
		})
	}
}

func TestGenerateRespectsContextDeadline(t *testing.T) {
	release := make(chan struct{})
	client := newTestClient(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := client.Generate(ctx, "кот")
	if err == nil {
		t.Fatal("Generate() error = nil, want a deadline error")
	}
	// reply.Handler tells a timeout apart from a refusal via errors.Is.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// TestGenerateRetriesAnotherKeyOn5xx verifies that a transient server error
// (5xx) is retried on the next key, which may land on a healthier backend.
func TestGenerateRetriesAnotherKeyOn5xx(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"bad gateway"}`)
			return
		}
		writeImage(t, w, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"key-one", "key-two"},
		Model:   "openai/gpt-image-1",
		Timeout: 5 * time.Second,
	})

	got, err := client.Generate(context.Background(), "кот")
	if err != nil {
		t.Fatalf("Generate() error = %v, want a 502 retried on the next key", err)
	}
	if string(got) != string(pngBytes) {
		t.Errorf("Generate() returned %q, want the decoded image", got)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want the 5xx retried once on the second key", calls)
	}
}

// TestGenerateRotatesToTheNextKeyOn401 verifies that a revoked or expired key
// (HTTP 401) is parked and the next key is tried, instead of breaking image
// generation entirely. This mirrors the same fix applied to llm.Client.
func TestGenerateRotatesToTheNextKeyOn401(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "Bearer dead-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
			return
		}
		writeImage(t, w, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"dead-key", "good-key"},
		Model:   "openai/gpt-image-1",
		Timeout: 5 * time.Second,
	})

	got, err := client.Generate(context.Background(), "кот")
	if err != nil {
		t.Fatalf("Generate() error = %v, want the good key to draw after 401", err)
	}
	if string(got) != string(pngBytes) {
		t.Errorf("Generate() returned %q, want the decoded image", got)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want exactly two: 401 on dead-key then success on good-key", calls)
	}
}

// TestGenerateSendsResponseFormatForDallE verifies that response_format=b64_json
// is sent for dall-e models, which default to returning a URL.
func TestGenerateSendsResponseFormatForDallE(t *testing.T) {
	var body request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeImage(t, w, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Model:   "dall-e-3",
		Timeout: 5 * time.Second,
	})

	_, err := client.Generate(context.Background(), "кот")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if body.ResponseFormat != "b64_json" {
		t.Errorf("response_format = %q, want b64_json for a dall-e model", body.ResponseFormat)
	}
}

// TestGenerateSendsResponseFormatForPrefixedDallE verifies that the dall-e
// detection works with provider-prefixed model names like "openai/dall-e-3".
func TestGenerateSendsResponseFormatForPrefixedDallE(t *testing.T) {
	var body request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeImage(t, w, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	t.Cleanup(server.Close)

	client := New(Options{
		BaseURL: server.URL,
		APIKeys: []string{"sk-test"},
		Model:   "openai/dall-e-3",
		Timeout: 5 * time.Second,
	})

	_, err := client.Generate(context.Background(), "кот")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if body.ResponseFormat != "b64_json" {
		t.Errorf("response_format = %q, want b64_json for a prefixed dall-e model", body.ResponseFormat)
	}
}

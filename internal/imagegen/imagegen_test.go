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
		Model:   "gemini-3.1-flash-image",
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
	if body.Model != "gemini-3.1-flash-image" {
		t.Errorf("model = %q, want the configured image model", body.Model)
	}
	if body.Prompt != "кот в скафандре" {
		t.Errorf("prompt = %q, want it passed through", body.Prompt)
	}
	if body.N != 1 {
		t.Errorf("n = %d, want a single picture", body.N)
	}
	if body.ResponseFormat != "b64_json" {
		t.Errorf("response_format = %q, want the bytes inline", body.ResponseFormat)
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
		Model:   "gemini-3.1-flash-image",
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

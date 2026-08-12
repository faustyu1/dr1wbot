package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSearchViaPaxsenix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools/web-search" {
			t.Errorf("path = %q, want /tools/web-search", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("q") != "погода москва" {
			t.Errorf("query = %q", r.URL.Query().Get("q"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"websites": []map[string]any{
					{"title": "Gismeteo", "url": "https://gismeteo.ru", "description": "Прогноз погоды"},
					{"title": "Yandex", "url": "https://yandex.ru/pogoda", "description": "Погода сейчас"},
				},
			},
		})
	}))
	t.Cleanup(server.Close)

	s := New(Options{
		PaxsenixURL: server.URL,
		PaxsenixKey: "test-key",
		Timeout:     5 * time.Second,
	})

	results, err := s.Search(context.Background(), "погода москва", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Title != "Gismeteo" {
		t.Errorf("first result title = %q", results[0].Title)
	}
	if results[0].Snippet != "Прогноз погоды" {
		t.Errorf("first result snippet = %q", results[0].Snippet)
	}
}

func TestSearchFallsBackToSearxng(t *testing.T) {
	paxsenix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(paxsenix.Close)

	searxng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format = %q, want json", r.URL.Query().Get("format"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Result 1", "url": "https://example.com/1", "content": "Snippet 1"},
			},
		})
	}))
	t.Cleanup(searxng.Close)

	s := New(Options{
		PaxsenixURL: paxsenix.URL,
		PaxsenixKey: "test-key",
		SearxngURL:  searxng.URL,
		Timeout:     5 * time.Second,
	})

	results, err := s.Search(context.Background(), "test query", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 from fallback", len(results))
	}
	if results[0].Title != "Result 1" {
		t.Errorf("title = %q", results[0].Title)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	s := New(Options{PaxsenixKey: "k"})
	if _, err := s.Search(context.Background(), "  ", 5); err == nil {
		t.Error("Search() with empty query = nil error, want an error")
	}
}

func TestFormatResults(t *testing.T) {
	out := FormatResults([]Result{
		{Title: "Test", URL: "https://example.com", Snippet: "A snippet"},
	})
	if out == "" || !contains(out, "Test") || !contains(out, "https://example.com") || !contains(out, "A snippet") {
		t.Errorf("FormatResults() = %q, want title, url, and snippet", out)
	}

	empty := FormatResults(nil)
	if empty != "Ничего не найдено." {
		t.Errorf("FormatResults(nil) = %q, want the empty message", empty)
	}
}

func TestConfigured(t *testing.T) {
	if New(Options{}).Configured() {
		t.Error("New({}).Configured() = true, want false with no backends")
	}
	if !New(Options{PaxsenixKey: "k"}).Configured() {
		t.Error("with key, Configured() should be true")
	}
	if !New(Options{SearxngURL: "http://localhost:8080"}).Configured() {
		t.Error("with searxng url, Configured() should be true")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

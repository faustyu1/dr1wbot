// Package search runs web queries through the PaxSenix search API, with a
// local SearXNG instance as a fallback.
//
// PaxSenix is a hosted search aggregator that returns ranked results with
// titles, URLs, snippets, and dates. When it is unavailable or returns nothing,
// the query is retried against the local SearXNG instance — so the bot keeps
// answering even if the hosted service has an outage.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Searcher queries PaxSenix and falls back to SearXNG.
type Searcher struct {
	http        *http.Client
	paxsenixURL string // e.g. "https://api.paxsenix.org"
	paxsenixKey string // API key; empty disables PaxSenix
	searxngURL  string // e.g. "http://127.0.0.1:8080"; empty disables fallback
	log         *slog.Logger
}

// Options configures a Searcher.
type Options struct {
	PaxsenixURL string        // defaults to https://api.paxsenix.org
	PaxsenixKey string        // required for PaxSenix; empty = go straight to SearXNG
	SearxngURL  string        // defaults to http://127.0.0.1:8080; empty = no fallback
	Timeout     time.Duration // 0 = 15s
	HTTPClient  *http.Client  // optional; tests inject their own
	Log         *slog.Logger
}

const (
	defaultPaxsenixURL = "https://api.paxsenix.org"
	defaultSearxngURL  = "http://127.0.0.1:8080"
	defaultTimeout     = 15 * time.Second
)

// New builds a Searcher. PaxSenix is used when PaxsenixKey is set; otherwise the
// searcher goes straight to SearXNG. When neither is configured, Search returns
// an error on every call — the caller should check Configured first.
func New(opts Options) *Searcher {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Searcher{
		http:        client,
		paxsenixURL: strings.TrimRight(opts.PaxsenixURL, "/"),
		paxsenixKey: strings.TrimSpace(opts.PaxsenixKey),
		searxngURL:  strings.TrimRight(opts.SearxngURL, "/"),
		log:         log,
	}
}

// Configured reports whether at least one backend can serve queries.
func (s *Searcher) Configured() bool {
	return s.paxsenixKey != "" || s.searxngURL != ""
}

// maxResults caps how many hits are returned so a single tool call cannot
// flood the model's context with pages of snippets.
const maxResults = 5

// paxsenixResponse is the subset of the PaxSenix JSON we read.
type paxsenixResponse struct {
	Result struct {
		Websites []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Content     string `json:"content"`
			Snippet     string `json:"snippet"`
		} `json:"websites"`
	} `json:"result"`
}

// searxResponse is the subset of SearXNG's JSON we read.
type searxResponse struct {
	Results []searxResult `json:"results"`
}

type searxResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// Search queries PaxSenix first; on failure or empty results it falls back to
// SearXNG. Returns at most count results (capped at maxResults).
func (s *Searcher) Search(ctx context.Context, query string, count int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty search query")
	}
	if count <= 0 || count > maxResults {
		count = maxResults
	}

	if s.paxsenixKey != "" {
		results, err := s.searchPaxsenix(ctx, query, count)
		if err == nil && len(results) > 0 {
			s.log.Debug("search via paxsenix", "query", query, "results", len(results))
			return results, nil
		}
		if err != nil {
			s.log.Warn("paxsenix failed, falling back to searxng", "query", query, "err", err)
		} else {
			s.log.Debug("paxsenix returned no results, falling back to searxng", "query", query)
		}
	}

	if s.searxngURL != "" {
		results, err := s.searchSearxng(ctx, query, count)
		if err != nil {
			return nil, fmt.Errorf("searxng: %w", err)
		}
		s.log.Debug("search via searxng", "query", query, "results", len(results))
		return results, nil
	}

	return nil, fmt.Errorf("no search backend configured")
}

// searchPaxsenix calls the PaxSenix web-search endpoint.
func (s *Searcher) searchPaxsenix(ctx context.Context, query string, count int) ([]Result, error) {
	base := s.paxsenixURL
	if base == "" {
		base = defaultPaxsenixURL
	}

	endpoint, err := url.Parse(base + "/tools/web-search")
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}
	params := url.Values{"q": {query}}
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.paxsenixKey)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; dr1wbot-search/1.0)")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("paxsenix returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed paxsenixResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	results := make([]Result, 0, count)
	for _, w := range parsed.Result.Websites {
		if w.URL == "" {
			continue
		}
		snippet := w.Description
		if snippet == "" {
			snippet = w.Content
		}
		if snippet == "" {
			snippet = w.Snippet
		}
		results = append(results, Result{
			Title:   strings.TrimSpace(w.Title),
			URL:     w.URL,
			Snippet: strings.TrimSpace(snippet),
		})
		if len(results) >= count {
			break
		}
	}
	return results, nil
}

// searchSearxng calls the local SearXNG JSON endpoint.
func (s *Searcher) searchSearxng(ctx context.Context, query string, count int) ([]Result, error) {
	base := s.searxngURL
	if base == "" {
		base = defaultSearxngURL
	}

	endpoint, err := url.Parse(base + "/search")
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}
	params := url.Values{
		"q":        {query},
		"format":   {"json"},
		"language": {"auto"},
	}
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; dr1wbot-search/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("searxng returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed searxResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	results := make([]Result, 0, count)
	for _, r := range parsed.Results {
		if r.URL == "" {
			continue
		}
		results = append(results, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     r.URL,
			Snippet: strings.TrimSpace(r.Content),
		})
		if len(results) >= count {
			break
		}
	}
	return results, nil
}

// FormatResults turns search results into a compact text block the model can
// read as a tool result. Each hit is title, URL, snippet — enough for the model
// to cite a source without fetching the page itself.
func FormatResults(results []Result) string {
	if len(results) == 0 {
		return "Ничего не найдено."
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%d. %s\n%s", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			b.WriteString("\n" + r.Snippet)
		}
	}
	return b.String()
}

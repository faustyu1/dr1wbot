package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// samplePage is the shape DuckDuckGo's HTML endpoint answers with: results as
// anchors carrying a redirect href, each followed by its snippet.
const samplePage = `<html><body>
<div class="result results_links">
  <h2 class="result__title">
    <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Fgo1.26&amp;rut=x">Go 1.26 <b>Release Notes</b></a>
  </h2>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev">Go 1.26 is  released &amp; ready.</a>
</div>
<div class="result results_links">
  <h2 class="result__title">
    <a rel="nofollow" class="result__a" href="https://example.com/go">Прямая ссылка</a>
  </h2>
  <a class="result__snippet">Второй результат.</a>
</div>
</body></html>`

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(Options{BaseURL: server.URL, MaxResults: 5, Timeout: time.Second})
}

func TestSearchParsesResults(t *testing.T) {
	var gotQuery, gotMethod string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_ = r.ParseForm()
		gotQuery = r.PostFormValue("q")
		_, _ = w.Write([]byte(samplePage))
	})

	results, err := client.Search(context.Background(), "go 1.26")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotQuery != "go 1.26" {
		t.Errorf("q = %q, want the query", gotQuery)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want two", results)
	}

	first := results[0]
	// The redirect is unwrapped: a model shown duckduckgo.com/l/?uddg=… quotes
	// that as the source, which is useless to whoever reads the answer.
	if first.URL != "https://go.dev/doc/go1.26" {
		t.Errorf("URL = %q, want the unwrapped target", first.URL)
	}
	if first.Title != "Go 1.26 Release Notes" {
		t.Errorf("Title = %q, want the tags stripped", first.Title)
	}
	if first.Snippet != "Go 1.26 is released & ready." {
		t.Errorf("Snippet = %q, want entities decoded and spaces collapsed", first.Snippet)
	}
	if results[1].URL != "https://example.com/go" {
		t.Errorf("second URL = %q, want a direct href kept as is", results[1].URL)
	}
}

func TestSearchHonoursMaxResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(samplePage))
	}))
	t.Cleanup(server.Close)

	client := New(Options{BaseURL: server.URL, MaxResults: 1, Timeout: time.Second})
	results, err := client.Search(context.Background(), "go")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want the cap honoured", len(results))
	}
}

func TestSearchReportsNoResults(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>No results.</body></html>`))
	})

	if _, err := client.Search(context.Background(), "asdkjhasd"); !errors.Is(err, ErrNoResults) {
		t.Errorf("Search() error = %v, want ErrNoResults", err)
	}
}

func TestSearchFailsOnABadStatus(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	if _, err := client.Search(context.Background(), "go"); err == nil {
		t.Error("Search() error = nil, want a captcha or throttle reported")
	}
}

func TestLookupTurnsNothingFoundIntoAnAnswer(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>nothing</body></html>`))
	})

	// The model is handed text, not an error: losing the whole answer because a
	// lookup came back empty is worse than telling it the lookup came back
	// empty.
	text, err := client.Lookup(context.Background(), "нечегонайти")
	if err != nil {
		t.Fatalf("Lookup() error = %v, want the emptiness reported as text", err)
	}
	if !strings.Contains(text, "ничего не нашлось") {
		t.Errorf("Lookup() = %q, want it to say nothing was found", text)
	}
}

func TestLookupRendersSourcesForTheModel(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(samplePage))
	})

	text, err := client.Lookup(context.Background(), "go 1.26")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	for _, want := range []string{"go 1.26", "Go 1.26 Release Notes", "https://go.dev/doc/go1.26", "1."} {
		if !strings.Contains(text, want) {
			t.Errorf("Lookup() = %q, want it to carry %q", text, want)
		}
	}
}

func TestUnwrapRejectsNonHTTP(t *testing.T) {
	// A javascript: or data: href in a scraped page is not a source.
	if got := unwrap("javascript:alert(1)"); got != "" {
		t.Errorf("unwrap() = %q, want a non-http link dropped", got)
	}
}

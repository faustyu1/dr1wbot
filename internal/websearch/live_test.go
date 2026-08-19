package websearch

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveDuckDuckGo hits the real endpoint. It is skipped by default because a
// unit test must not depend on somebody else's website — but the parser reads
// HTML meant for a browser, so when answers start coming back empty, this is
// the test that says whether the page changed shape:
//
//	LIVE_SEARCH=1 go test ./internal/websearch/ -run Live -v
func TestLiveDuckDuckGo(t *testing.T) {
	if os.Getenv("LIVE_SEARCH") == "" {
		t.Skip("set LIVE_SEARCH=1 to hit the real endpoint")
	}

	client := New(Options{MaxResults: 3, Timeout: 20 * time.Second, Region: "ru-ru"})
	results, err := client.Search(context.Background(), "go release notes")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search() returned nothing: the result page has probably changed shape")
	}
	for _, r := range results {
		t.Logf("%s — %s", r.Title, r.URL)
		if !strings.HasPrefix(r.URL, "http") {
			t.Errorf("URL = %q, want a real address: the redirect is no longer being unwrapped", r.URL)
		}
	}
}

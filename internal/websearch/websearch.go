// Package websearch gives the model a way to look something up.
//
// DuckDuckGo is used through its HTML endpoint rather than an API, because
// there is no free DuckDuckGo API: the instant-answer endpoint answers almost
// nothing, and every "search API" worth the name wants a key and a card. The
// HTML page is what a browser gets, so it is parsed the way a browser would
// have to — with the fragility that implies, which is why a failed search
// degrades into "no results" instead of into a failed answer.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// endpoint is DuckDuckGo's no-JavaScript result page. The lite variant is
// smaller and easier to parse, but it drops the snippets, which are the half of
// a result the model actually reads.
const endpoint = "https://html.duckduckgo.com/html/"

// userAgent is sent because the endpoint answers a blank one with a captcha
// page. It is an ordinary desktop browser string: nothing here pretends to be
// something it is not beyond what any HTTP client has to claim to be served.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// bodyLimit caps what we read of a result page. A page of ten results is well
// under this; anything far above it is not a result page.
const bodyLimit = 2 << 20

// ErrNoResults means the search ran and found nothing. It is not a failure of
// the bot, and the model is told exactly that.
var ErrNoResults = errors.New("no results")

// Result is one hit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Client searches DuckDuckGo.
type Client struct {
	http    *http.Client
	baseURL string
	max     int
	// region and safe are passed through to DuckDuckGo. They exist because the
	// same query answers differently in different regions, and a bot answering
	// a Russian question wants the Russian result set.
	region string
	safe   string
}

// Options configures a Client.
type Options struct {
	// MaxResults bounds how many hits are handed to the model. More than a
	// handful is mostly noise that is billed as input tokens.
	MaxResults int
	Timeout    time.Duration
	Region     string // e.g. ru-ru; empty lets DuckDuckGo guess
	// SafeSearch is off by default: the bot's own system prompt decides what it
	// will talk about, and a filtered result set only makes it answer worse.
	SafeSearch bool
	BaseURL    string       // tests point this at their own server
	HTTPClient *http.Client // optional
}

// New builds a Client.
func New(opts Options) *Client {
	if opts.MaxResults <= 0 {
		opts.MaxResults = 5
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	base := opts.BaseURL
	if base == "" {
		base = endpoint
	}
	safe := "-1" // DuckDuckGo's "off"
	if opts.SafeSearch {
		safe = "1"
	}
	return &Client{http: httpClient, baseURL: base, max: opts.MaxResults, region: opts.Region, safe: safe}
}

// Search returns the top hits for query.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, ErrNoResults
	}

	form := url.Values{"q": {query}, "kp": {c.safe}}
	if c.region != "" {
		form.Set("kl", c.region)
	}

	// POST is what the page's own form does, and it keeps a long query out of
	// the URL, where DuckDuckGo is quicker to answer with a captcha.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "ru,en;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read search results: %w", err)
	}

	results := parse(string(body), c.max)
	if len(results) == 0 {
		return nil, ErrNoResults
	}
	return results, nil
}

// Lookup is Search rendered for a model to read. It is the whole tool-facing
// surface: a model is handed text, not a struct, and "nothing found" is an
// answer it can work with rather than an error that loses the whole reply.
func (c *Client) Lookup(ctx context.Context, query string) (string, error) {
	results, err := c.Search(ctx, query)
	if errors.Is(err, ErrNoResults) {
		return fmt.Sprintf("По запросу %q ничего не нашлось. Скажи об этом прямо, "+
			"если ответ без этого не получается.", query), nil
	}
	if err != nil {
		return "", err
	}
	return Text(query, results), nil
}

// Text renders results the way the model reads them best: numbered, with the
// source under each line. The numbering is what lets an answer say "по данным
// (2)" without inventing a link.
func Text(query string, results []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Результаты поиска по запросу %q:\n\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n%s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "%s\n", r.Snippet)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

var (
	// linkPattern matches a result's anchor: its href and its title.
	linkPattern = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	// snippetPattern matches the grey line under a result.
	snippetPattern = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
	tagPattern     = regexp.MustCompile(`<[^>]*>`)
	spacePattern   = regexp.MustCompile(`\s+`)
)

// parse pulls the results out of a DuckDuckGo HTML page. Snippets are matched
// separately and zipped by position: a result without one still counts, and a
// page that changes shape yields fewer results rather than nonsense.
func parse(page string, max int) []Result {
	links := linkPattern.FindAllStringSubmatch(page, -1)
	snippets := snippetPattern.FindAllStringSubmatch(page, -1)

	out := make([]Result, 0, len(links))
	for i, m := range links {
		title := clean(m[2])
		link := unwrap(html.UnescapeString(m[1]))
		if title == "" || link == "" {
			continue
		}
		r := Result{Title: title, URL: link}
		if i < len(snippets) {
			r.Snippet = clean(snippets[i][1])
		}
		out = append(out, r)
		if len(out) >= max {
			break
		}
	}
	return out
}

// unwrap turns DuckDuckGo's redirect into the real address. A model shown
// "duckduckgo.com/l/?uddg=…" quotes that as the source, which is useless to
// whoever reads the answer.
func unwrap(raw string) string {
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if target := parsed.Query().Get("uddg"); target != "" {
		return target
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.String()
}

// clean turns a fragment of result HTML into the plain sentence it renders as.
func clean(s string) string {
	s = tagPattern.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spacePattern.ReplaceAllString(s, " "))
}

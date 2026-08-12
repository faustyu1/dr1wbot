// Package tgchannel reads public Telegram channels through the web preview
// pages that Telegram serves at https://t.me/s/<channel>.
//
// No account, no session, no bot token: everything is parsed from the public
// HTML. A channel that is open shows its recent posts there; a private one
// shows nothing, and that is not fixable from this side.
package tgchannel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Post is one message from a public channel preview page.
type Post struct {
	MessageID int
	Link      string
	Date      string // ISO timestamp from <time datetime="…">
	Views     string
	Text      string
	Media     []string // photo, video, document, poll, location, link_preview, voice
}

// Channel is the result of reading a preview page.
type Channel struct {
	Title    string
	Username string
	Counter  string // subscriber count label, e.g. "12K subscribers"
	Posts    []Post
}

// Reader fetches and parses public Telegram preview pages.
type Reader struct {
	http *http.Client
	log  *slog.Logger
}

// Options configures a Reader.
type Options struct {
	HTTPClient *http.Client  // nil = a client with Timeout
	Timeout    time.Duration // 0 = 15s
	Log        *slog.Logger
}

const (
	defaultTimeout    = 15 * time.Second
	maxPostsLimit     = 300
	defaultPostsLimit = 10
	maxSearchPages    = 5
	maxReadPages      = 20      // enough to reach 300 posts (~15-20 per page)
	fetchBodyLimit    = 4 << 20 // 4 MB cap on one page
)

// New builds a Reader.
func New(opts Options) *Reader {
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
	return &Reader{http: client, log: log}
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

var reservedPaths = map[string]bool{
	"addemoji": true, "addstickers": true, "c": true, "iv": true,
	"joinchat": true, "proxy": true, "share": true,
}

// NormalizeChannelRef reduces @username, t.me/канал, or a full URL to a bare
// username. Returns an error when the input is not a valid public channel
// reference.
func NormalizeChannelRef(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("empty channel reference")
	}

	if strings.Contains(value, "://") || strings.HasPrefix(value, "t.me/") || strings.HasPrefix(value, "telegram.me/") {
		// url.Parse needs a scheme to populate Host; prepend one when absent.
		parseVal := value
		if !strings.Contains(parseVal, "://") {
			parseVal = "https://" + parseVal
		}
		u, err := url.Parse(parseVal)
		if err == nil {
			host := strings.ToLower(u.Host)
			if host == "t.me" || host == "telegram.me" || host == "www.t.me" || host == "www.telegram.me" {
				parts := strings.Split(strings.Trim(u.Path, "/"), "/")
				if len(parts) > 0 && parts[0] == "s" {
					parts = parts[1:]
				}
				if len(parts) > 0 {
					value = parts[0]
				}
			}
		}
	}

	value = strings.TrimPrefix(value, "@")
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "/")

	if !usernameRe.MatchString(value) || reservedPaths[strings.ToLower(value)] {
		return "", fmt.Errorf("invalid channel username: %q", raw)
	}
	return value, nil
}

var postLinkRe = regexp.MustCompile(`(?:https?://)?(?:t\.me|telegram\.me)/(?:s/)?([A-Za-z0-9_]{3,32})/(\d+)`)

// ParsePostLink extracts username and message ID from a link like
// https://t.me/durov/123.
func ParsePostLink(raw string) (string, int, error) {
	value := strings.TrimSpace(raw)
	if i := strings.IndexAny(value, "?#"); i >= 0 {
		value = value[:i]
	}
	m := postLinkRe.FindStringSubmatch(value)
	if m == nil {
		return "", 0, fmt.Errorf("invalid Telegram post URL: %q", raw)
	}
	var msgID int
	if _, err := fmt.Sscanf(m[2], "%d", &msgID); err != nil || msgID <= 0 {
		return "", 0, fmt.Errorf("invalid message ID in URL: %q", raw)
	}
	return m[1], msgID, nil
}

func clampLimit(n int) int {
	if n <= 0 {
		return defaultPostsLimit
	}
	if n > maxPostsLimit {
		return maxPostsLimit
	}
	return n
}

func channelURL(username string, before string, messageID int) string {
	base := "https://t.me/s/" + url.PathEscape(username)
	if messageID > 0 {
		return fmt.Sprintf("%s/%d", base, messageID)
	}
	if before != "" {
		return fmt.Sprintf("%s?before=%s", base, url.QueryEscape(before))
	}
	return base
}

// fetchPage downloads one preview page and parses it.
func (r *Reader) fetchPage(ctx context.Context, pageURL string) (*Channel, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; dr1wbot-tgchannel/1.0)")

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return &Channel{}, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("t.me/s returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchBodyLimit))
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}

	ch, before := parsePage(string(body))
	return ch, before, nil
}

// ReadChannel returns the latest posts from a public channel (up to limit,
// capped at 300). It fetches the first preview page; when more posts are needed
// than one page holds, it follows the pagination cursor.
func (r *Reader) ReadChannel(ctx context.Context, channel string, limit int) (*Channel, error) {
	username, err := NormalizeChannelRef(channel)
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit)

	var posts []Post
	seen := make(map[int]bool)
	before := ""
	var firstPage *Channel

	for page := 0; page < maxReadPages; page++ {
		ch, nextBefore, err := r.fetchPage(ctx, channelURL(username, before, 0))
		if err != nil {
			return nil, err
		}
		if page == 0 {
			firstPage = ch
		}
		for _, p := range ch.Posts {
			if !seen[p.MessageID] {
				seen[p.MessageID] = true
				posts = append(posts, p)
			}
		}
		if len(posts) >= limit || nextBefore == "" || nextBefore == before {
			break
		}
		before = nextBefore
	}

	if len(posts) > limit {
		posts = posts[:limit]
	}

	if firstPage == nil {
		firstPage = &Channel{}
	}
	firstPage.Username = username
	firstPage.Posts = posts
	return firstPage, nil
}

// ReadPost fetches one specific post by its link (e.g. https://t.me/durov/123).
func (r *Reader) ReadPost(ctx context.Context, postURL string) (*Channel, *Post, error) {
	username, msgID, err := ParsePostLink(postURL)
	if err != nil {
		return nil, nil, err
	}

	ch, _, err := r.fetchPage(ctx, channelURL(username, "", msgID))
	if err != nil {
		return nil, nil, err
	}
	ch.Username = username

	for i := range ch.Posts {
		if ch.Posts[i].MessageID == msgID {
			return ch, &ch.Posts[i], nil
		}
	}
	return nil, nil, fmt.Errorf("post not found in the public preview; it may be private, deleted, or too old")
}

// Search looks for keyword matches across the latest preview pages (up to
// maxSearchPages) and returns the matching posts.
func (r *Reader) Search(ctx context.Context, channel, query string, limit int) (*Channel, []Post, error) {
	username, err := NormalizeChannelRef(channel)
	if err != nil {
		return nil, nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil, fmt.Errorf("empty search query")
	}
	limit = clampLimit(limit)

	var results []Post
	seen := make(map[int]bool)
	before := ""
	var firstPage *Channel
	queryLower := strings.ToLower(query)

	for page := 0; page < maxSearchPages; page++ {
		ch, nextBefore, err := r.fetchPage(ctx, channelURL(username, before, 0))
		if err != nil {
			return nil, nil, err
		}
		if page == 0 {
			firstPage = ch
		}
		for _, p := range ch.Posts {
			if seen[p.MessageID] {
				continue
			}
			seen[p.MessageID] = true
			if strings.Contains(strings.ToLower(p.Text), queryLower) {
				results = append(results, p)
				if len(results) >= limit {
					if firstPage == nil {
						firstPage = &Channel{}
					}
					firstPage.Username = username
					return firstPage, results, nil
				}
			}
		}
		if nextBefore == "" || nextBefore == before {
			break
		}
		before = nextBefore
	}

	if firstPage == nil {
		firstPage = &Channel{}
	}
	firstPage.Username = username
	return firstPage, results, nil
}

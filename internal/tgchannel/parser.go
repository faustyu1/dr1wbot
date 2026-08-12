package tgchannel

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// mediaLabels maps CSS class fragments to media type names.
var mediaLabels = []string{
	"photo", "video", "document", "poll", "location", "link_preview", "voice",
}

// parsePage parses the HTML of a t.me/s/<channel> preview page and extracts
// the channel header (title, counter) and posts.
func parsePage(htmlStr string) (*Channel, string) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return &Channel{}, ""
	}

	var title, counter, before string
	var posts []Post

	// Walk the whole tree once, collecting everything.
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		// Channel header.
		if n.Type == html.ElementNode {
			if classContains(n, "tgme_header_title") {
				title = strings.TrimSpace(extractText(n))
			}
			if classContains(n, "tgme_header_counter") {
				counter = strings.TrimSpace(extractText(n))
			}
			// Pagination cursor.
			if n.Data == "a" && classContains(n, "js-messages_more") {
				before = attrValue(n, "data-before")
			}
			// Post container.
			if n.Data == "div" && classContains(n, "js-widget_message") {
				dataPost := attrValue(n, "data-post")
				if dataPost != "" {
					if post := parsePostNode(n, dataPost); post != nil {
						posts = append(posts, *post)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return &Channel{Title: title, Counter: counter, Posts: posts}, before
}

// parsePostNode extracts one post from its container div.
func parsePostNode(container *html.Node, dataPost string) *Post {
	post := postFromDataPost(dataPost)
	if post == nil {
		return nil
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if classContains(n, "tgme_widget_message_text") {
				post.Text = normalizeMultiline(extractTextWithBreaks(n))
			}
			if classContains(n, "tgme_widget_message_views") {
				post.Views = strings.TrimSpace(extractText(n))
			}
			if n.Data == "time" {
				dt := attrValue(n, "datetime")
				if dt != "" && post.Date == "" {
					post.Date = dt
				}
			}
			for _, label := range mediaLabels {
				if classContains(n, "tgme_widget_message_"+label) && !classContains(n, "tgme_widget_message_text") {
					post.Media = appendUnique(post.Media, label)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(container)

	return post
}

// extractTextWithBreaks collects text, converting <br> to newlines.
func extractTextWithBreaks(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.ElementNode && n.Data == "br" {
		return "\n"
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(extractTextWithBreaks(c))
	}
	return b.String()
}

// extractText recursively collects all text content of a node.
func extractText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(extractText(c))
	}
	return b.String()
}

// classContains reports whether the class attribute contains a substring.
func classContains(n *html.Node, fragment string) bool {
	for _, attr := range n.Attr {
		if attr.Key == "class" && strings.Contains(attr.Val, fragment) {
			return true
		}
	}
	return false
}

// attrValue returns the value of the named attribute, or "" if absent.
func attrValue(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func normalizeMultiline(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = collapseSpaces(s)
	s = trimNewlineSpaces(s)
	s = collapseNewlines(s)
	return strings.TrimSpace(s)
}

func collapseSpaces(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\f' || r == '\v' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
	}
	return b.String()
}

func trimNewlineSpaces(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "\n")
}

func collapseNewlines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

func postFromDataPost(dataPost string) *Post {
	parts := strings.SplitN(dataPost, "/", 2)
	if len(parts) != 2 || !usernameRe.MatchString(parts[0]) {
		return nil
	}
	msgID, err := strconv.Atoi(parts[1])
	if err != nil || msgID <= 0 {
		return nil
	}
	return &Post{
		MessageID: msgID,
		Link:      "https://t.me/" + parts[0] + "/" + parts[1],
	}
}

func appendUnique(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}

// Package memory keeps short conversation threads, one per answer.
//
// Guest mode gives the bot no history at all: it is not a member of the chat,
// so a reply to its own answer arrives without the text that was replied to.
// Remembering the last few turns ourselves is the only way a follow-up like
// "а что там ещё?" can mean anything.
//
// History follows the reply chain rather than the chat or the user: every
// answer remembers the thread that led to it, and a reply to that answer
// continues exactly that thread — whoever sends it, and however many other
// conversations happened in the chat since. Replying to an older answer
// branches off it instead of dragging in whatever was discussed last. Who said
// what is carried in the turn text.
//
// Telegram does not tell us which of our messages a reply points at — a guest
// answer is an inline message with no chat message id — so an answer is
// recognised by its text: see Key.
package memory

import (
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Turn is one message in a conversation.
type Turn struct {
	Role    string // "user" or "assistant"
	Content string
}

const (
	// MaxTurns is how many messages are kept per thread, counting both sides.
	// A long tail mostly adds cost and drift rather than context.
	MaxTurns = 8
	// MaxContentRunes trims a remembered message; the point is the gist, not a
	// verbatim transcript.
	MaxContentRunes = 1500
	// keyRunes is how much of an answer identifies it. A prefix rather than the
	// whole text, so an answer Telegram cut short or rendered slightly
	// differently further down is still found.
	keyRunes = 64
)

type threadID struct {
	chat int64
	key  string
}

type thread struct {
	turns []Turn
	seen  time.Time
}

// Store holds recent threads. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	threads map[threadID]*thread
	// latest is the last answer given in each chat, for a private chat where a
	// plain message — not a reply — continues the conversation.
	latest map[int64]string

	ttl       time.Duration
	maxThread int
	now       func() time.Time
}

// Options configures a Store.
type Options struct {
	// TTL is how long a thread stays warm after its last message.
	TTL time.Duration
	// MaxConversations bounds memory use; the stalest thread is dropped first.
	MaxConversations int
	// Now is injectable for tests.
	Now func() time.Time
}

// New builds a Store.
func New(opts Options) *Store {
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Minute
	}
	if opts.MaxConversations <= 0 {
		opts.MaxConversations = 500
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Store{
		threads:   make(map[threadID]*thread),
		latest:    make(map[int64]string),
		ttl:       opts.TTL,
		maxThread: opts.MaxConversations,
		now:       opts.Now,
	}
}

// Thread returns the turns that led up to and include one of our answers,
// oldest first. answer is the text as the reply quoted it; chatIDs are the ids
// the conversation may be filed under, tried in order. There can be more than
// one: in a private chat between two people Telegram may show the chat to the
// bot under a different id depending on who is writing.
func (s *Store) Thread(answer string, chatIDs ...int64) []Turn {
	key := Key(answer)
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, chatID := range chatIDs {
		if turns := s.lookupLocked(threadID{chatID, key}); turns != nil {
			return turns
		}
	}
	return nil
}

// Latest returns the thread of the last answer given in a chat.
func (s *Store) Latest(chatID int64) []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.latest[chatID]
	if !ok {
		return nil
	}
	return s.lookupLocked(threadID{chatID, key})
}

// Remember records one exchange on top of the thread it continued (nil for a
// fresh question) and files the result under the answer, so a reply to that
// answer picks up from here. The thread passed in is not changed: two replies
// to the same answer become two branches. A failed answer is not remembered,
// so it leaves every existing thread as it was.
func (s *Store) Remember(chatID int64, history []Turn, question, answer string) {
	key := Key(answer)
	if key == "" {
		return
	}

	turns := append([]Turn(nil), history...)
	if question != "" {
		turns = append(turns, Turn{Role: "user", Content: trim(question)})
	}
	turns = append(turns, Turn{Role: "assistant", Content: trim(answer)})
	if len(turns) > MaxTurns {
		turns = turns[len(turns)-MaxTurns:]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictLocked()
	s.threads[threadID{chatID, key}] = &thread{turns: turns, seen: s.now()}
	s.latest[chatID] = key
}

func (s *Store) lookupLocked(id threadID) []Turn {
	t, ok := s.threads[id]
	if !ok {
		return nil
	}
	if s.now().Sub(t.seen) > s.ttl {
		delete(s.threads, id)
		return nil
	}
	return append([]Turn(nil), t.turns...)
}

// evictLocked drops expired threads and, if still over the cap, the stalest
// one. Requires the lock.
func (s *Store) evictLocked() {
	now := s.now()
	for k, t := range s.threads {
		if now.Sub(t.seen) > s.ttl {
			delete(s.threads, k)
		}
	}
	for len(s.threads) >= s.maxThread {
		var oldest threadID
		var oldestAt time.Time
		first := true
		for k, t := range s.threads {
			if first || t.seen.Before(oldestAt) {
				oldest, oldestAt, first = k, t.seen, false
			}
		}
		if first {
			break
		}
		delete(s.threads, oldest)
	}
	for chat, key := range s.latest {
		if _, ok := s.threads[threadID{chat, key}]; !ok {
			delete(s.latest, chat)
		}
	}
}

// linkTarget is the "(url)" half of a Markdown link, which the rendered message
// does not show.
var linkTarget = regexp.MustCompile(`\]\([^)]*\)`)

// Key identifies an answer by its text in a form that survives rendering: the
// model writes Markdown, while a reply quotes the message as Telegram displays
// it, without the markup. Only letters and digits are kept, lowercased, and only
// the opening of the answer counts.
func Key(text string) string {
	text = linkTarget.ReplaceAllString(text, "]")
	var b strings.Builder
	n := 0
	for _, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
		if n++; n == keyRunes {
			break
		}
	}
	return b.String()
}

func trim(s string) string {
	r := []rune(s)
	if len(r) <= MaxContentRunes {
		return s
	}
	return string(r[:MaxContentRunes]) + "…"
}

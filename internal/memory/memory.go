// Package memory keeps a short conversation history per chat.
//
// Guest mode gives the bot no history at all: it is not a member of the chat,
// so a reply to its own answer arrives without the text that was replied to.
// Remembering the last few turns ourselves is the only way a follow-up like
// "а что там ещё?" can mean anything.
//
// History is kept per chat rather than per user: in a group the conversation is
// shared, and the person who follows up is often not the one who asked. Who
// said what is carried in the turn text instead.
package memory

import (
	"sync"
	"time"
)

// Turn is one message in a conversation.
type Turn struct {
	Role    string // "user" or "assistant"
	Content string
}

const (
	// MaxTurns is how many messages are kept per conversation, counting both
	// sides. Guest mode has no threading, so a long tail mostly adds cost and
	// drift rather than context.
	MaxTurns = 8
	// MaxContentRunes trims a remembered message; the point is the gist, not a
	// verbatim transcript.
	MaxContentRunes = 1500
)

type conversation struct {
	turns []Turn
	seen  time.Time
}

// Store holds recent conversations. It is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	convos map[int64]*conversation

	ttl      time.Duration
	maxConvo int
	now      func() time.Time
}

// Options configures a Store.
type Options struct {
	// TTL is how long a conversation stays warm after its last message.
	TTL time.Duration
	// MaxConversations bounds memory use; the stalest entry is dropped first.
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
		convos:   make(map[int64]*conversation),
		ttl:      opts.TTL,
		maxConvo: opts.MaxConversations,
		now:      opts.Now,
	}
}

// History returns the remembered turns for one chat, oldest first.
func (s *Store) History(chatID int64) []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.convos[chatID]
	if !ok {
		return nil
	}
	if s.now().Sub(c.seen) > s.ttl {
		delete(s.convos, chatID)
		return nil
	}
	return append([]Turn(nil), c.turns...)
}

// Remember appends one exchange. Empty sides are skipped so a failed answer
// does not poison the next turn.
func (s *Store) Remember(chatID int64, question, answer string) {
	if question == "" && answer == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictLocked()

	c, ok := s.convos[chatID]
	if !ok || s.now().Sub(c.seen) > s.ttl {
		c = &conversation{}
		s.convos[chatID] = c
	}

	if question != "" {
		c.turns = append(c.turns, Turn{Role: "user", Content: trim(question)})
	}
	if answer != "" {
		c.turns = append(c.turns, Turn{Role: "assistant", Content: trim(answer)})
	}
	if len(c.turns) > MaxTurns {
		c.turns = append([]Turn(nil), c.turns[len(c.turns)-MaxTurns:]...)
	}
	c.seen = s.now()
}

// Forget drops one chat's history, so a conversation can start over.
func (s *Store) Forget(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.convos, chatID)
}

// evictLocked drops expired conversations and, if still over the cap, the
// stalest one. Requires the lock.
func (s *Store) evictLocked() {
	now := s.now()
	for k, c := range s.convos {
		if now.Sub(c.seen) > s.ttl {
			delete(s.convos, k)
		}
	}
	for len(s.convos) >= s.maxConvo {
		var oldest int64
		var oldestAt time.Time
		first := true
		for k, c := range s.convos {
			if first || c.seen.Before(oldestAt) {
				oldest, oldestAt, first = k, c.seen, false
			}
		}
		if first {
			return
		}
		delete(s.convos, oldest)
	}
}

func trim(s string) string {
	r := []rune(s)
	if len(r) <= MaxContentRunes {
		return s
	}
	return string(r[:MaxContentRunes]) + "…"
}

// Package answers caches replies to identical questions for a short while.
//
// It is aimed at one specific waste: the same question arriving over and over.
// That is mostly what a script does, and a script is usually banned before it
// gets far — but the cache costs nothing when it misses, and when it hits it
// saves a whole request rather than a few tokens.
//
// Only questions with no history and no pictures are cached. A follow-up means
// the answer depends on a conversation this key knows nothing about, and an
// attached picture makes two identical texts two different questions.
package answers

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// entry is one cached answer.
type entry struct {
	answer string
	until  time.Time
}

// Cache holds recent answers. It is safe for concurrent use.
type Cache struct {
	mu      sync.Mutex
	entries map[string]entry

	ttl     time.Duration
	maxSize int
	now     func() time.Time

	hits, misses int
}

// Options configures a Cache.
type Options struct {
	// TTL is how long an answer stays usable. Short on purpose: a cached reply
	// is a stale reply, and the point is catching repeats within minutes, not
	// pretending the model is a database.
	TTL time.Duration
	// MaxSize bounds how many answers are held.
	MaxSize int
	// Now is injectable for tests.
	Now func() time.Time
}

// New builds a Cache. A TTL of zero disables it entirely.
func New(opts Options) *Cache {
	if opts.MaxSize <= 0 {
		opts.MaxSize = 200
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{
		entries: make(map[string]entry),
		ttl:     opts.TTL,
		maxSize: opts.MaxSize,
		now:     opts.Now,
	}
}

// Enabled reports whether anything is cached at all.
func (c *Cache) Enabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttl > 0
}

// SetTTL changes how long answers live, or switches the cache off with zero.
// Turning it off empties it too: keeping stale answers around for a setting
// that says "do not reuse answers" would be a trap.
func (c *Cache) SetTTL(ttl time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ttl = ttl
	if ttl <= 0 {
		c.entries = make(map[string]entry)
	}
}

// Key derives the cache key. The system prompt is part of it because the same
// question asked with the house style and without it are different questions.
func Key(system, prompt string) string {
	h := sha256.New()
	h.Write([]byte(system))
	h.Write([]byte{0}) // a separator, so "ab"+"c" and "a"+"bc" differ
	h.Write([]byte(prompt))
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns a cached answer if one is still fresh.
func (c *Cache) Get(key string) (string, bool) {
	if c == nil {
		return "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ttl <= 0 {
		return "", false
	}
	e, ok := c.entries[key]
	if !ok || !c.now().Before(e.until) {
		delete(c.entries, key)
		c.misses++
		return "", false
	}
	c.hits++
	return e.answer, true
}

// Put stores an answer. An empty one is ignored: caching a failure would turn
// one bad minute into ten.
func (c *Cache) Put(key, answer string) {
	if c == nil || answer == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ttl <= 0 {
		return
	}
	c.evictLocked()
	c.entries[key] = entry{answer: answer, until: c.now().Add(c.ttl)}
}

// Stats is what the panel shows.
type Stats struct {
	Size   int
	Hits   int
	Misses int
}

// Stats reports how much the cache has actually saved.
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Size: len(c.entries), Hits: c.hits, Misses: c.misses}
}

// evictLocked drops expired entries and, if still at the cap, an arbitrary one.
// Arbitrary is good enough here: everything expires within minutes anyway, so
// the choice of victim barely outlives the decision. Requires the lock.
func (c *Cache) evictLocked() {
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.until) {
			delete(c.entries, k)
		}
	}
	for k := range c.entries {
		if len(c.entries) < c.maxSize {
			return
		}
		delete(c.entries, k)
	}
}

package answers

import (
	"testing"
	"time"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func TestDisabledWithoutATTL(t *testing.T) {
	c := New(Options{})

	if c.Enabled() {
		t.Error("Enabled() = true with no TTL")
	}
	c.Put("k", "ответ")
	if _, ok := c.Get("k"); ok {
		t.Error("Get() returned something from a disabled cache")
	}
}

func TestRoundTrip(t *testing.T) {
	clk := &clock{at: time.Now()}
	c := New(Options{TTL: 10 * time.Minute, Now: clk.now})

	c.Put("k", "ответ")
	got, ok := c.Get("k")
	if !ok || got != "ответ" {
		t.Errorf("Get() = (%q, %t), want the stored answer", got, ok)
	}
}

func TestEntriesExpire(t *testing.T) {
	clk := &clock{at: time.Now()}
	c := New(Options{TTL: 10 * time.Minute, Now: clk.now})

	c.Put("k", "ответ")
	clk.at = clk.at.Add(11 * time.Minute)
	// A cached reply is a stale reply; the point is catching repeats within
	// minutes, not pretending the model is a database.
	if _, ok := c.Get("k"); ok {
		t.Error("Get() returned an expired answer")
	}
}

func TestEmptyAnswersAreNotCached(t *testing.T) {
	c := New(Options{TTL: time.Minute})

	// Caching a failure would turn one bad minute into ten.
	c.Put("k", "")
	if _, ok := c.Get("k"); ok {
		t.Error("an empty answer was cached")
	}
}

func TestKeyDependsOnBothParts(t *testing.T) {
	if Key("sys", "prompt") == Key("sys2", "prompt") {
		t.Error("Key() ignores the system prompt, so a raw answer could be served to a normal question")
	}
	if Key("sys", "prompt") == Key("sys", "prompt2") {
		t.Error("Key() ignores the question")
	}
	// The separator is what stops "ab"+"c" from colliding with "a"+"bc".
	if Key("ab", "c") == Key("a", "bc") {
		t.Error("Key() concatenates without a separator")
	}
	if Key("sys", "prompt") != Key("sys", "prompt") {
		t.Error("Key() is not stable")
	}
}

func TestSizeIsBounded(t *testing.T) {
	clk := &clock{at: time.Now()}
	c := New(Options{TTL: time.Hour, MaxSize: 4, Now: clk.now})

	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		c.Put(k, "ответ")
	}
	if got := c.Stats().Size; got > 4 {
		t.Errorf("Size = %d, want it capped at 4", got)
	}
}

func TestSetTTLOffEmptiesTheCache(t *testing.T) {
	clk := &clock{at: time.Now()}
	c := New(Options{TTL: time.Hour, Now: clk.now})
	c.Put("k", "ответ")

	c.SetTTL(0)
	if c.Enabled() {
		t.Error("Enabled() = true after the cache was switched off")
	}
	// Keeping stale answers for a setting that says "do not reuse answers"
	// would be a trap.
	if got := c.Stats().Size; got != 0 {
		t.Errorf("Size = %d after switching off, want the entries dropped", got)
	}
}

func TestStatsCountHitsAndMisses(t *testing.T) {
	c := New(Options{TTL: time.Minute})

	c.Put("k", "ответ")
	c.Get("k")
	c.Get("missing")

	got := c.Stats()
	if got.Hits != 1 || got.Misses != 1 {
		t.Errorf("Stats() = %+v, want one of each", got)
	}
}

func TestNilCacheIsInert(t *testing.T) {
	var c *Cache
	// main wires a real one, but a nil is the documented "off" and must not
	// take the bot down with it.
	if c.Enabled() {
		t.Error("a nil cache reports itself enabled")
	}
	c.Put("k", "v")
	if _, ok := c.Get("k"); ok {
		t.Error("a nil cache returned something")
	}
}

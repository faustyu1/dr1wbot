// Package keyring rotates a pool of API keys.
//
// Google AI Studio meters the free tier per key, so the way to survive a spent
// quota is to hold several keys and move to the next one when a request comes
// back 429. A key that hit a limit is parked for a cooldown rather than
// dropped: a per-minute limit heals on its own within a minute, and a daily one
// heals overnight, so the same key is worth trying again later.
package keyring

import (
	"sync"
	"time"
)

// Lease is one key handed out for an attempt. Index identifies it so the caller
// can report back how the attempt went.
type Lease struct {
	Index int
	Key   string
}

// Ring hands out keys in rotation. It is safe for concurrent use.
type Ring struct {
	mu       sync.Mutex
	keys     []string
	until    []time.Time // when a parked key may be tried again
	cursor   int         // where the next attempt starts
	cooldown time.Duration
	now      func() time.Time // swapped in tests
}

// New builds a Ring over keys. Blank entries are dropped, and cooldown is how
// long a rate-limited key is skipped for.
func New(keys []string, cooldown time.Duration) *Ring {
	kept := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "" {
			kept = append(kept, k)
		}
	}
	return &Ring{
		keys:     kept,
		until:    make([]time.Time, len(kept)),
		cooldown: cooldown,
		now:      time.Now,
	}
}

// Len is how many keys the ring holds.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.keys)
}

// Lease returns every key to try for one request, best first: keys that are not
// cooling down, in rotation order from the cursor, then the cooling ones as a
// last resort. Trying a parked key beats failing outright — the cooldown is a
// guess about when the quota heals, and a wrong guess should cost a request,
// not the answer.
func (r *Ring) Lease() []Lease {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	ready := make([]Lease, 0, len(r.keys))
	parked := make([]Lease, 0, len(r.keys))
	for offset := range r.keys {
		i := (r.cursor + offset) % len(r.keys)
		lease := Lease{Index: i, Key: r.keys[i]}
		if now.Before(r.until[i]) {
			parked = append(parked, lease)
			continue
		}
		ready = append(ready, lease)
	}
	return append(ready, parked...)
}

// Stats describes the pool's health.
type Stats struct {
	Total  int
	Ready  int
	Parked int
	// NextReady is when the earliest parked key comes back. Zero when nothing
	// is parked.
	NextReady time.Time
}

// Stats reports how much of the pool is still spendable.
func (r *Ring) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	out := Stats{Total: len(r.keys)}
	for i := range r.keys {
		if now.Before(r.until[i]) {
			out.Parked++
			if out.NextReady.IsZero() || r.until[i].Before(out.NextReady) {
				out.NextReady = r.until[i]
			}
			continue
		}
		out.Ready++
	}
	return out
}

// Limit parks the key that just hit a quota and moves the cursor past it, so
// the next request starts on a different key instead of re-learning this one.
func (r *Ring) Limit(index int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= len(r.keys) {
		return
	}
	r.until[index] = r.now().Add(r.cooldown)
	r.cursor = (index + 1) % len(r.keys)
}

// Works clears a key's cooldown and makes it the starting point for the next
// request. Staying on a key that answers keeps the other quotas untouched.
func (r *Ring) Works(index int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= len(r.keys) {
		return
	}
	r.until[index] = time.Time{}
	r.cursor = index
}

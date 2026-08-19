// Package settings holds the knobs worth changing without a redeploy.
//
// The split with the environment is deliberate. Things that decide what the
// process *is* — the bot token, the API keys, the endpoint, the file paths —
// stay in .env, because changing them means a restart anyway and because a
// running bot should not be able to lock its owner out of it. Things that
// decide how it *behaves* towards people — how much the public gets, how fast
// is too fast, how long a ban lasts — live here, because those are exactly the
// numbers an operator wants to change at 3am while watching abuse happen.
//
// The environment seeds this store on first run and is ignored afterwards: a
// value edited from the panel must not silently revert on the next restart.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Values is the mutable configuration.
type Values struct {
	// PublicDailyLimit is how many requests one non-whitelisted person gets per
	// day. Zero closes the bot to everyone but the whitelist.
	PublicDailyLimit int `json:"public_daily_limit"`
	// Burst is how many requests inside BurstWindow are still human.
	Burst int `json:"burst"`
	// BurstWindow is the span Burst is measured over.
	BurstWindow time.Duration `json:"burst_window"`
	// GlobalDailyLimit caps what the public costs in total per day. Zero leaves
	// the total unbounded.
	GlobalDailyLimit int `json:"global_daily_limit"`
	// NewAccountThreshold halves the allowance for user ids at or above it.
	NewAccountThreshold int64 `json:"new_account_threshold"`
	// BanFor is how long a warned caller sits out.
	BanFor time.Duration `json:"ban_for"`
	// PublicOpenLimit remembers what the daily limit was before the kill
	// switch closed it, so turning the public back on restores the number
	// instead of dropping it to whatever the first cycle step happens to be.
	PublicOpenLimit int `json:"public_open_limit"`
	// PublicMaxTokens caps what a public answer may spend.
	PublicMaxTokens int `json:"public_max_tokens"`
	// PublicMaxRunes caps how long a public question may be.
	PublicMaxRunes int `json:"public_max_runes"`
	// RawFlagEnabled turns the admin "-s" flag on and off.
	RawFlagEnabled bool `json:"raw_flag_enabled"`
	// CacheTTL is how long an answer to an identical question is reused. Zero
	// disables the cache.
	CacheTTL time.Duration `json:"cache_ttl"`
	// SearchEnabled lets the model look things up on the web. It is a setting
	// rather than a constant because a search costs a second round trip and a
	// larger prompt, and an operator watching their quota may want it off.
	SearchEnabled bool `json:"search_enabled"`
	// StreamEnabled makes the answer appear as it is written instead of all at
	// once. It costs one Telegram edit every couple of seconds, which is why it
	// can be turned off.
	StreamEnabled bool `json:"stream_enabled"`
}

// Store keeps Values on disk. It is safe for concurrent use.
type Store struct {
	mu     sync.RWMutex
	values Values
	path   string // empty keeps changes in memory only
}

// Load reads the store, seeding it from defaults when there is no file yet.
func Load(path string, defaults Values) (*Store, error) {
	s := &Store{values: defaults, path: path}
	if path == "" {
		return s, nil
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil // first run: the environment's values stand
	}
	if err != nil {
		return nil, fmt.Errorf("read settings %s: %w", path, err)
	}

	// Decoding into the defaults means a field added in a later version keeps
	// its default instead of arriving as a zero.
	saved := defaults
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", path, err)
	}
	s.values = saved
	return s, nil
}

// Get returns the current values.
func (s *Store) Get() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.values
}

// Update applies a change and persists it. The mutation runs under the lock, so
// a caller reading a field to compute the next one cannot race another edit.
func (s *Store) Update(mutate func(*Values)) (Values, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	before := s.values
	mutate(&s.values)
	if err := s.saveLocked(); err != nil {
		s.values = before // keep memory consistent with disk
		return before, err
	}
	return s.values, nil
}

// saveLocked writes the file atomically. Requires the lock.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}

	data, err := json.MarshalIndent(s.values, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("create temp settings in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp settings: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace settings %s: %w", s.path, err)
	}
	return nil
}

// Cycle steps through a fixed list of choices, wrapping at the end. It is what
// a menu button does: there is no text input in an inline keyboard, so every
// numeric setting is edited by walking a short list of sensible values.
func Cycle[T comparable](choices []T, current T) T {
	for i, c := range choices {
		if c == current {
			return choices[(i+1)%len(choices)]
		}
	}
	if len(choices) == 0 {
		return current
	}
	return choices[0]
}

// Package quota rations the bot among people who are not on the whitelist.
//
// Guest mode makes the bot reachable from all of Telegram, so opening it to the
// public means opening the API key with it. A per-user daily allowance is what
// makes that survivable: a curious stranger costs a bounded number of requests,
// and a determined one cannot drain the day's quota on their own.
//
// The count is per user and resets at midnight UTC, matching nothing in
// particular — the point is a predictable line in the day, not alignment with
// any provider's own reset.
package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Verdict says why a request was refused, which decides how the bot answers:
// a spent allowance deserves an explanation, a banned script deserves silence.
type Verdict int

const (
	// Granted means the request is booked and may be served.
	Granted Verdict = iota
	// Spent means the day's allowance is gone. Worth telling the user.
	Spent
	// Banned means the caller is being ignored for abuse. Not worth telling
	// them: an answer is the feedback a script is looking for, and every reply
	// is a request to Telegram we are making on a bot's behalf.
	Banned
	// Exhausted means the bot's own daily budget is gone, not this caller's.
	// Per-person limits do not bound the total: a thousand new accounts at
	// thirty requests each is thirty thousand requests, every one of them
	// within the rules.
	Exhausted
)

// String makes a verdict readable in a log line.
func (v Verdict) String() string {
	switch v {
	case Granted:
		return "granted"
	case Spent:
		return "spent"
	case Banned:
		return "banned"
	case Exhausted:
		return "exhausted"
	default:
		return "unknown"
	}
}

// user is one caller's record for the day.
type user struct {
	Used int `json:"used"`
	// Recent holds the timestamps inside the burst window, oldest first.
	Recent []time.Time `json:"recent,omitempty"`
	// Warns holds the time of each burst violation. They expire, so a single
	// clumsy evening does not follow somebody forever, while a script that
	// keeps coming back accumulates them faster than they fall off.
	Warns []time.Time `json:"warns,omitempty"`
	// BannedUntil is zero when the caller is in good standing.
	BannedUntil time.Time `json:"banned_until,omitempty"`
	// Forever marks a permanent ban, which is what enough live warns earns.
	// A time far in the future would work too, but a flag says what it means
	// and cannot quietly expire.
	Forever bool `json:"forever,omitempty"`
	// Seen is the last time this caller asked anything, which is what orders
	// the panel's list of people to ban.
	Seen time.Time `json:"seen,omitempty"`
}

// Store counts today's requests per user and sidelines the ones that abuse it.
// It is safe for concurrent use.
type Store struct {
	mu sync.Mutex

	limit       int
	globalLimit int
	newAccount  int64
	burst       int
	window      time.Duration
	banFor      time.Duration
	warnTTL     time.Duration
	maxWarns    int

	day        string // UTC date the counters belong to
	users      map[int64]*user
	globalUsed int

	path string // empty disables persistence
	now  func() time.Time
}

// Options configures a Store.
type Options struct {
	// Limit is how many requests one non-whitelisted user gets per day. Zero
	// keeps the bot closed: nobody outside the whitelist is served at all.
	Limit int
	// GlobalLimit caps what the public costs in total per day, across
	// everybody. Zero leaves the total unbounded, which the per-person limit
	// alone does nothing about. It is the difference between "nobody can be
	// greedy" and "the day cannot cost more than this".
	GlobalLimit int
	// NewAccountThreshold halves the allowance for user ids at or above it.
	// Telegram hands out ids in ascending order, so a high one means a fresh
	// account — which is what a farm is made of and what a real person you
	// already know is not. Zero treats everybody the same.
	NewAccountThreshold int64
	// Burst is how many requests inside Window are still human. Exceeding it is
	// what a script does and a person does not: the daily limit alone would let
	// a bot spend a whole allowance in two seconds, and do it from a thousand
	// accounts before anybody noticed.
	Burst int
	// Window is the span Burst is measured over.
	Window time.Duration
	// BanFor is how long a warned caller sits out.
	BanFor time.Duration
	// WarnTTL is how long a warn counts against somebody. Warns that fall off
	// are what separates "had a bad day once" from "keeps doing this".
	WarnTTL time.Duration
	// MaxWarns is how many live warns earn a permanent ban.
	MaxWarns int
	// File holds the counters so a restart does not hand everyone a fresh day.
	File string
	// Now is injectable for tests.
	Now func() time.Time
}

// persisted is the on-disk format. Callers are keyed by user id as a string
// because that is what JSON objects allow.
type persisted struct {
	Day        string           `json:"day"`
	Users      map[string]*user `json:"users"`
	GlobalUsed int              `json:"global_used,omitempty"`
}

// New builds a Store and reloads today's counters if they are still current.
func New(opts Options) (*Store, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Burst <= 0 {
		opts.Burst = 5
	}
	if opts.Window <= 0 {
		opts.Window = time.Minute
	}
	if opts.BanFor <= 0 {
		opts.BanFor = 15 * time.Minute
	}
	if opts.WarnTTL <= 0 {
		opts.WarnTTL = 30 * 24 * time.Hour
	}
	if opts.MaxWarns <= 0 {
		opts.MaxWarns = 3
	}
	s := &Store{
		limit:       opts.Limit,
		globalLimit: opts.GlobalLimit,
		newAccount:  opts.NewAccountThreshold,
		burst:       opts.Burst,
		window:      opts.Window,
		banFor:      opts.BanFor,
		warnTTL:     opts.WarnTTL,
		maxWarns:    opts.MaxWarns,
		users:       make(map[int64]*user),
		path:        opts.File,
		now:         opts.Now,
	}
	s.day = today(s.now())

	if s.path == "" || s.limit <= 0 {
		return s, nil
	}

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil // first run
	}
	if err != nil {
		return nil, fmt.Errorf("read quota %s: %w", s.path, err)
	}

	var loaded persisted
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("parse quota %s: %w", s.path, err)
	}
	if loaded.Day != s.day {
		// Yesterday's counters are stale, but an unexpired ban is not: a script
		// must not be able to wash one off by waiting for midnight.
		for key, u := range loaded.Users {
			id, err := strconv.ParseInt(key, 10, 64)
			if err != nil || u == nil || !s.bannedLocked(u, s.now()) {
				continue
			}
			s.users[id] = &user{Warns: u.Warns, BannedUntil: u.BannedUntil, Forever: u.Forever}
		}
		return s, nil
	}
	for key, u := range loaded.Users {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || u == nil {
			continue // a corrupt key costs one caller's count, not the file
		}
		s.users[id] = u
	}
	s.globalUsed = loaded.GlobalUsed
	return s, nil
}

// SetPolicy replaces the limits at runtime. Counters already booked are left
// alone: lowering the daily limit takes effect from the next request, it does
// not retroactively ban whoever is already over the new number.
func (s *Store) SetPolicy(limit, globalLimit, burst int, window, banFor time.Duration, newAccount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.limit = limit
	s.globalLimit = globalLimit
	s.newAccount = newAccount
	if burst > 0 {
		s.burst = burst
	}
	if window > 0 {
		s.window = window
	}
	if banFor > 0 {
		s.banFor = banFor
	}
}

// Policy reports the per-person limits currently in force.
func (s *Store) Policy() (limit, burst int, window, banFor time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit, s.burst, s.window, s.banFor
}

// Enabled reports whether anyone outside the whitelist is served at all.
func (s *Store) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit > 0
}

// Limit is the daily allowance per user.
func (s *Store) Limit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

// Take books one request for userID and reports how it went. A refused request
// is not counted, so the number a user is shown does not creep up as they
// retry — and so a banned script cannot spend an allowance it never gets.
func (s *Store) Take(userID int64) (used, limit int, ok bool) {
	verdict, used, limit := s.Judge(userID)
	return used, limit, verdict == Granted
}

// Note records that a user interacted with the bot without consuming quota.
// This lets the admin panel show whitelisted and admin users in "who wrote
// today", not just public callers who go through Take/Judge.
func (s *Store) Note(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolloverLocked()
	u := s.userLocked(userID)
	u.Seen = s.now()
	s.saveLocked()
}

// Judge is Take with the reason attached.
func (s *Store) Judge(userID int64) (verdict Verdict, used, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.limit <= 0 {
		return Spent, 0, 0
	}
	s.rolloverLocked()

	now := s.now()
	u := s.userLocked(userID)
	limit = s.allowanceLocked(userID)

	if s.bannedLocked(u, now) {
		return Banned, u.Used, limit
	}
	// The bot's own budget is checked before the caller's: when the day is
	// gone it is gone for everybody, and there is no point telling one person
	// they still have twenty left.
	if s.globalLimit > 0 && s.globalUsed >= s.globalLimit {
		return Exhausted, u.Used, limit
	}
	if u.Used >= limit {
		return Spent, u.Used, limit
	}

	// Drop what has aged out of the window, then judge what is left.
	cutoff := now.Add(-s.window)
	kept := u.Recent[:0]
	for _, at := range u.Recent {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	u.Recent = kept

	if len(u.Recent) >= s.burst {
		s.warnLocked(u, now)
		s.saveLocked()
		return Banned, u.Used, limit
	}

	u.Recent = append(u.Recent, now)
	u.Used++
	u.Seen = now
	s.globalUsed++
	s.saveLocked()
	return Granted, u.Used, limit
}

// allowanceLocked is one caller's daily limit. Requires the lock.
func (s *Store) allowanceLocked(userID int64) int {
	if s.newAccount <= 0 || userID < s.newAccount {
		return s.limit
	}
	// Half, but never zero: a fresh account is a suspicion, not a conviction.
	if half := s.limit / 2; half > 0 {
		return half
	}
	return 1
}

// warnLocked records one violation and applies whatever it earns: a fixed
// timeout for the first few, a permanent ban once enough live warns pile up.
// Requires the lock.
func (s *Store) warnLocked(u *user, now time.Time) {
	u.Warns = append(pruneWarns(u.Warns, now.Add(-s.warnTTL)), now)
	u.Recent = nil

	if len(u.Warns) >= s.maxWarns {
		u.Forever = true
		u.BannedUntil = time.Time{}
		return
	}
	u.BannedUntil = now.Add(s.banFor)
}

// pruneWarns drops warns older than the cutoff.
func pruneWarns(warns []time.Time, cutoff time.Time) []time.Time {
	kept := warns[:0]
	for _, at := range warns {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

// bannedLocked reports whether a caller is sidelined right now. Requires the
// lock.
func (s *Store) bannedLocked(u *user, now time.Time) bool {
	return u.Forever || now.Before(u.BannedUntil)
}

// Peek reports the current count without booking anything.
func (s *Store) Peek(userID int64) (used, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolloverLocked()
	// The effective allowance, not the configured one: a fresh account is
	// shown the number it actually gets.
	return s.userLocked(userID).Used, s.allowanceLocked(userID)
}

// BannedUntil reports when a caller is let back in. A zero time means they are
// in good standing.
func (s *Store) BannedUntil(userID int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	u := s.userLocked(userID)
	if !s.bannedLocked(u, s.now()) {
		return time.Time{}
	}
	return u.BannedUntil
}

// Ban sidelines a caller by hand. The panel needs this because the burst
// detector only catches speed: somebody can be a nuisance at a perfectly human
// pace, and that is a judgement call, not a threshold.
func (s *Store) Ban(userID int64, d time.Duration) {
	if d <= 0 {
		d = s.banFor
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	u := s.userLocked(userID)
	u.BannedUntil = s.now().Add(d)
	u.Recent = nil
	s.saveLocked()
}

// Caller is one person the panel can act on.
type Caller struct {
	ID    int64
	Used  int
	Limit int
	// Until is when a ban lifts; zero means they are in good standing or the
	// ban is permanent, which Forever tells apart.
	Until   time.Time
	Forever bool
	Warns   int
	Seen    time.Time
}

// Banned reports whether this caller is sidelined right now.
func (c Caller) Banned() bool { return c.Forever || !c.Until.IsZero() }

// Callers lists today's public users, banned ones first and then the most
// recently active, capped at max. It exists because an inline keyboard has
// nowhere to type an id into: the only way to offer a ban button is to build
// one per person the bot already knows about.
func (s *Store) Callers(max int) []Caller {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolloverLocked()

	now := s.now()
	out := make([]Caller, 0, len(s.users))
	for id, u := range s.users {
		u.Warns = pruneWarns(u.Warns, now.Add(-s.warnTTL))
		c := Caller{
			ID:    id,
			Used:  u.Used,
			Limit: s.allowanceLocked(id),
			Seen:  u.Seen,
			Warns: len(u.Warns),
		}
		if s.bannedLocked(u, now) {
			c.Until, c.Forever = u.BannedUntil, u.Forever
		}
		out = append(out, c)
	}

	slices.SortFunc(out, func(a, b Caller) int {
		// Banned first: those are the ones an admin came here to review.
		if banned := boolCmp(a.Banned(), b.Banned()); banned != 0 {
			return banned
		}
		return b.Seen.Compare(a.Seen)
	})

	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

// Pardon clears a ban and its history, so an admin can undo a false positive.
func (s *Store) Pardon(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	u := s.userLocked(userID)
	u.BannedUntil = time.Time{}
	u.Forever = false
	u.Warns = nil
	u.Recent = nil
	s.saveLocked()
}

// userLocked returns the caller's record, creating it if needed. Requires the
// lock.
func (s *Store) userLocked(id int64) *user {
	u, ok := s.users[id]
	if !ok {
		u = &user{}
		s.users[id] = u
	}
	return u
}

// Stats summarises the day.
type Stats struct {
	Limit       int
	GlobalLimit int
	GlobalUsed  int
	Users       int // how many distinct people spent anything today
	Requests    int // how many requests they spent in total
	Banned      int // how many are sidelined right now
	ResetsIn    time.Duration
}

// Stats describes today so far.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rolloverLocked()

	now := s.now()
	out := Stats{
		Limit:       s.limit,
		GlobalLimit: s.globalLimit,
		GlobalUsed:  s.globalUsed,
		ResetsIn:    s.resetsInLocked(),
	}
	for _, u := range s.users {
		if u.Used > 0 {
			out.Users++
			out.Requests += u.Used
		}
		if s.bannedLocked(u, now) {
			out.Banned++
		}
	}
	return out
}

// rolloverLocked clears the day's counters when the UTC day has turned. Bans
// and strikes are deliberately kept: they are about behaviour, not about the
// allowance, and a script must not be able to wait out a ban by waiting for
// midnight. Requires the lock.
func (s *Store) rolloverLocked() {
	day := today(s.now())
	if day == s.day {
		return
	}
	s.day = day
	s.globalUsed = 0

	now := s.now()
	for id, u := range s.users {
		u.Warns = pruneWarns(u.Warns, now.Add(-s.warnTTL))
		if !s.bannedLocked(u, now) && len(u.Warns) == 0 {
			delete(s.users, id) // clean record, nothing left to remember
			continue
		}
		s.users[id] = &user{Warns: u.Warns, BannedUntil: u.BannedUntil, Forever: u.Forever}
	}
	s.saveLocked()
}

// resetsInLocked is how long until the counters clear. Requires the lock.
func (s *Store) resetsInLocked() time.Duration {
	now := s.now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	return midnight.Sub(now)
}

// saveLocked writes the counters out. A failure is not fatal: losing the file
// costs at most one day of counting, while refusing to answer over it would
// cost the user their request. Requires the lock.
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}

	snapshot := persisted{
		Day:        s.day,
		Users:      make(map[string]*user, len(s.users)),
		GlobalUsed: s.globalUsed,
	}
	for id, u := range s.users {
		snapshot.Users[strconv.FormatInt(id, 10)] = u
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".quota-*.json")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmpName, s.path)
}

func today(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

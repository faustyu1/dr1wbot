// Package quota rations the bot among people who are not on the whitelist.
//
// Guest mode makes the bot reachable from all of Telegram, so opening it to the
// public means opening the API key with it. A per-user daily allowance is what
// makes that survivable: a curious stranger costs a bounded number of requests,
// and a determined one cannot drain the day's free tier on their own.
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
	"strings"
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
	// Name is the last @username this id was seen under, lowercased and without
	// the @. Telegram offers no way to look an id up by name, so the only
	// directory that can exist is the one the bot builds from what walks past.
	Name string `json:"name,omitempty"`
	// Manual marks a ban an admin placed by hand. The flood detector's bans
	// expire and are forgiven; a hand-placed one is a decision, and the panel
	// says which is which.
	Manual bool `json:"manual,omitempty"`
}

// nameBan is a ban placed on a @username rather than on an id.
//
// It exists because /ban is useful exactly when the target is somebody an admin
// can see in a chat but whose numeric id they do not have — and because
// Telegram has no username-to-id lookup, so the ban has to wait for its target
// to show up before it can be pinned to an id. Until then it lives here and is
// applied the first moment that name sends anything.
type nameBan struct {
	Until   time.Time `json:"until,omitempty"`
	Forever bool      `json:"forever,omitempty"`
	// ID is the id the name resolved to, once it has. Zero means the name has
	// not been seen since the ban was placed.
	ID int64     `json:"id,omitempty"`
	At time.Time `json:"at,omitempty"`
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
	nameBans   map[string]*nameBan
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
	Day        string              `json:"day"`
	Users      map[string]*user    `json:"users"`
	Names      map[string]*nameBan `json:"names,omitempty"`
	GlobalUsed int                 `json:"global_used,omitempty"`
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
		nameBans:    make(map[string]*nameBan),
		path:        opts.File,
		now:         opts.Now,
	}
	s.day = today(s.now())

	// A closed bot still loads the file: bans are the operator's decisions, and
	// they must not evaporate because public access happens to be off.
	if s.path == "" {
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
	for name, nb := range loaded.Names {
		if nb == nil || !liveName(nb, s.now()) {
			continue // an expired name ban is not worth carrying forward
		}
		s.nameBans[name] = nb
	}
	if loaded.Day != s.day {
		// Yesterday's counters are stale, but an unexpired ban is not: a script
		// must not be able to wash one off by waiting for midnight.
		for key, u := range loaded.Users {
			id, err := strconv.ParseInt(key, 10, 64)
			if err != nil || u == nil || !s.bannedLocked(u, s.now()) {
				continue
			}
			s.users[id] = &user{Name: u.Name, Manual: u.Manual, Warns: u.Warns,
				BannedUntil: u.BannedUntil, Forever: u.Forever}
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

// Ban sidelines a caller by hand. A zero or negative duration means forever:
// that is what an admin who typed no time meant, and a ban that quietly expires
// because nobody named a number is worse than no ban at all.
//
// This exists because the burst detector only catches speed: somebody can be a
// nuisance at a perfectly human pace, and that is a judgement call, not a
// threshold.
func (s *Store) Ban(userID int64, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.banLocked(s.userLocked(userID), d)
	s.saveLocked()
}

// banLocked applies a hand-placed ban to one record. Requires the lock.
func (s *Store) banLocked(u *user, d time.Duration) {
	u.Manual = true
	u.Recent = nil
	if d <= 0 {
		u.Forever = true
		u.BannedUntil = time.Time{}
		return
	}
	u.Forever = false
	u.BannedUntil = s.now().Add(d)
}

// BanName sidelines a @username. It reports the id the name was pinned to, or
// zero when the bot has never seen it: Telegram has no username-to-id lookup, so
// a name nobody has spoken under yet can only be remembered and applied the
// moment it appears.
func (s *Store) BanName(username string, d time.Duration) (id int64, known bool) {
	name := normalName(username)
	if name == "" {
		return 0, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	nb := &nameBan{At: s.now()}
	if d <= 0 {
		nb.Forever = true
	} else {
		nb.Until = s.now().Add(d)
	}

	if u, uid := s.byNameLocked(name); u != nil {
		s.banLocked(u, d)
		nb.ID = uid
		id, known = uid, true
	}
	s.nameBans[name] = nb
	s.saveLocked()
	return id, known
}

// Note records the @username an id is currently using and applies any ban that
// was placed on that name before the bot knew who it belonged to.
//
// It reports whether the caller was banned by name just now, which is the one
// case where the answer to "is this person banned" changes as a side effect of
// asking who they are.
func (s *Store) Note(userID int64, username string) (bannedNow bool) {
	name := normalName(username)
	if name == "" || userID == 0 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	nb, pending := s.nameBans[name]
	if pending && !liveName(nb, s.now()) {
		delete(s.nameBans, name)
		pending = false
	}

	u, existing := s.users[userID]
	if !existing && !pending {
		// Nothing to remember about somebody with no record and no ban waiting:
		// a whitelisted regular should not grow an entry per message.
		return false
	}
	if !existing {
		u = s.userLocked(userID)
	}

	changed := u.Name != name
	u.Name = name

	if pending {
		u.Manual = true
		u.Forever = nb.Forever
		u.BannedUntil = nb.Until
		u.Recent = nil
		if nb.ID != userID {
			nb.ID = userID
			changed = true
		}
		bannedNow = true
	}
	if changed || bannedNow {
		s.saveLocked()
	}
	return bannedNow
}

// Ban describes one sidelined caller for the panel and for /bans.
type Ban struct {
	// ID is zero for a ban that is still waiting for its username to appear.
	ID       int64
	Username string
	// Until is when the ban lifts; zero with Forever set means never.
	Until   time.Time
	Forever bool
	// Manual tells a decision apart from what the flood detector did on its own.
	Manual bool
	Warns  int
}

// Active reports whether this ban is in force.
func (b Ban) Active() bool { return b.Forever || !b.Until.IsZero() }

// Blocked reports whether this caller is sidelined right now, by id or by name.
// It books nothing and changes nothing: unlike Judge, it is asked on every
// message, including from people the whitelist would otherwise wave through — a
// ban that only applies to strangers is not a ban.
func (s *Store) Blocked(userID int64, username string) (Ban, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if u, ok := s.users[userID]; ok && s.bannedLocked(u, now) {
		return Ban{
			ID: userID, Username: u.Name, Until: u.BannedUntil, Forever: u.Forever,
			Manual: u.Manual, Warns: len(pruneWarns(u.Warns, now.Add(-s.warnTTL))),
		}, true
	}
	if name := normalName(username); name != "" {
		if nb, ok := s.nameBans[name]; ok && liveName(nb, now) {
			return Ban{ID: userID, Username: name, Until: nb.Until, Forever: nb.Forever, Manual: true}, true
		}
	}
	return Ban{}, false
}

// Bans lists every ban in force, hand-placed ones first. It is what the panel
// shows and what /bans prints.
func (s *Store) Bans(max int) []Ban {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	out := make([]Ban, 0, len(s.users)+len(s.nameBans))
	seen := make(map[int64]struct{}, len(s.users))

	for id, u := range s.users {
		if !s.bannedLocked(u, now) {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, Ban{
			ID: id, Username: u.Name, Until: u.BannedUntil, Forever: u.Forever,
			Manual: u.Manual, Warns: len(pruneWarns(u.Warns, now.Add(-s.warnTTL))),
		})
	}
	for name, nb := range s.nameBans {
		if !liveName(nb, now) {
			continue
		}
		if _, already := seen[nb.ID]; already && nb.ID != 0 {
			continue // the same ban, already listed under its id
		}
		out = append(out, Ban{Username: name, Until: nb.Until, Forever: nb.Forever, Manual: true})
	}

	slices.SortFunc(out, func(a, b Ban) int {
		if manual := boolCmp(a.Manual, b.Manual); manual != 0 {
			return manual
		}
		if forever := boolCmp(a.Forever, b.Forever); forever != 0 {
			return forever
		}
		return a.Until.Compare(b.Until)
	})

	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// byNameLocked finds the caller last seen under a username. Requires the lock.
func (s *Store) byNameLocked(name string) (*user, int64) {
	for id, u := range s.users {
		if u.Name == name {
			return u, id
		}
	}
	return nil, 0
}

// liveName reports whether a name ban is still in force.
func liveName(nb *nameBan, now time.Time) bool {
	return nb != nil && (nb.Forever || now.Before(nb.Until))
}

// dropExpiredNamesLocked forgets name bans that have run out. Requires the lock.
func (s *Store) dropExpiredNamesLocked() {
	now := s.now()
	for name, nb := range s.nameBans {
		if !liveName(nb, now) {
			delete(s.nameBans, name)
		}
	}
}

// normalName turns whatever an admin typed into the key a username is stored
// under: no leading @, no case, no surrounding link.
func normalName(username string) string {
	name := strings.TrimSpace(username)
	name = strings.TrimPrefix(name, "https://t.me/")
	name = strings.TrimPrefix(name, "t.me/")
	name = strings.TrimPrefix(name, "@")
	if name == "" || strings.ContainsAny(name, " /@") {
		return ""
	}
	return strings.ToLower(name)
}

// Lookup resolves a @username to the id it was last seen under.
func (s *Store) Lookup(username string) (int64, bool) {
	name := normalName(username)
	if name == "" {
		return 0, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, id := s.byNameLocked(name); id != 0 {
		return id, true
	}
	if nb, ok := s.nameBans[name]; ok && nb.ID != 0 {
		return nb.ID, true
	}
	return 0, false
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
// It also lifts the ban on whatever username this caller is known by: leaving
// that behind would re-ban them on their very next message.
func (s *Store) Pardon(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	u := s.userLocked(userID)
	clearBan(u)
	if u.Name != "" {
		delete(s.nameBans, u.Name)
	}
	s.saveLocked()
}

// PardonName lifts a ban placed on a @username, and on the id it resolved to.
// It reports whether anything was actually lifted, so /unban can say so.
func (s *Store) PardonName(username string) bool {
	name := normalName(username)
	if name == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	nb, pending := s.nameBans[name]
	delete(s.nameBans, name)

	lifted := pending && liveName(nb, s.now())
	if u, _ := s.byNameLocked(name); u != nil {
		if s.bannedLocked(u, s.now()) {
			lifted = true
		}
		clearBan(u)
	} else if nb != nil && nb.ID != 0 {
		if u, ok := s.users[nb.ID]; ok {
			if s.bannedLocked(u, s.now()) {
				lifted = true
			}
			clearBan(u)
		}
	}
	s.saveLocked()
	return lifted
}

// clearBan wipes everything that keeps a caller sidelined.
func clearBan(u *user) {
	u.BannedUntil = time.Time{}
	u.Forever = false
	u.Manual = false
	u.Warns = nil
	u.Recent = nil
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
		if !s.bannedLocked(u, now) && len(pruneWarns(u.Warns, now.Add(-s.warnTTL))) == 0 {
			delete(s.users, id) // clean record, nothing left to remember
			continue
		}
		s.users[id] = &user{Name: u.Name, Manual: u.Manual, Warns: u.Warns,
			BannedUntil: u.BannedUntil, Forever: u.Forever}
	}
	s.dropExpiredNamesLocked()
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
		Names:      s.nameBans,
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

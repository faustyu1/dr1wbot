// Package access decides who may summon the bot and keeps that list current.
//
// Guest mode lets anyone on Telegram mention the bot in any chat, including
// chats it is not a member of, and every summon spends tokens on our API key.
// The whitelist is what keeps that from being an open tap.
//
// Entries come from two places. Those in the environment are fixed for the
// lifetime of the process; those added by an admin at runtime live in a JSON
// file so they survive a restart. Keeping them separate means editing the
// environment never silently loses runtime additions, and a runtime deletion
// never silently resurrects itself on the next deploy.
package access

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Store answers access questions and owns the mutable part of the whitelist.
// It is safe for concurrent use.
type Store struct {
	mu sync.RWMutex

	admins   map[int64]struct{}
	envUsers map[int64]struct{}
	envChats map[int64]struct{}
	dynUsers map[int64]struct{}
	dynChats map[int64]struct{}

	path string // empty disables persistence
}

// Options configures a Store.
type Options struct {
	Admins   map[int64]struct{}
	EnvUsers map[int64]struct{}
	EnvChats map[int64]struct{}
	// StateFile holds runtime additions. When empty, additions are kept in
	// memory only and lost on restart.
	StateFile string
}

// state is the on-disk format.
type state struct {
	Users []int64 `json:"users"`
	Chats []int64 `json:"chats"`
}

// ErrStatic is returned when a caller tries to remove an entry that comes from
// the environment and therefore cannot be changed at runtime.
var ErrStatic = errors.New("entry is set in the environment")

// New builds a Store and loads any previously saved runtime additions.
func New(opts Options) (*Store, error) {
	s := &Store{
		admins:   copySet(opts.Admins),
		envUsers: copySet(opts.EnvUsers),
		envChats: copySet(opts.EnvChats),
		dynUsers: make(map[int64]struct{}),
		dynChats: make(map[int64]struct{}),
		path:     opts.StateFile,
	}

	if s.path == "" {
		return s, nil
	}

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil // first run
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", s.path, err)
	}

	var loaded state
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", s.path, err)
	}
	for _, id := range loaded.Users {
		s.dynUsers[id] = struct{}{}
	}
	for _, id := range loaded.Chats {
		s.dynChats[id] = struct{}{}
	}
	return s, nil
}

// IsAdmin reports whether userID may run admin commands. Admins are always
// allowed to summon the bot regardless of the whitelist.
func (s *Store) IsAdmin(userID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.admins[userID]
	return ok
}

// HasAdmins reports whether any admin is configured.
func (s *Store) HasAdmins() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.admins) > 0
}

// Open reports whether no whitelist exists at all, in which case Allowed always
// returns true. Callers are expected to warn loudly about this at startup.
func (s *Store) Open() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.admins) == 0 &&
		len(s.envUsers) == 0 && len(s.envChats) == 0 &&
		len(s.dynUsers) == 0 && len(s.dynChats) == 0
}

// Allowed reports whether a summon from userID inside chatID may be served.
func (s *Store) Allowed(userID, chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.isOpenLocked() {
		return true
	}
	for _, set := range []map[int64]struct{}{s.admins, s.envUsers, s.dynUsers} {
		if _, ok := set[userID]; ok {
			return true
		}
	}
	for _, set := range []map[int64]struct{}{s.envChats, s.dynChats} {
		if _, ok := set[chatID]; ok {
			return true
		}
	}
	return false
}

// Add grants access to id. Positive ids are users, negative ids are group or
// channel chats — the sign is how Telegram itself distinguishes them. It
// reports whether anything changed; adding an existing entry is not an error.
func (s *Store) Add(id int64) (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.admins[id]; ok {
		return false, nil // admins are allowed unconditionally
	}
	env, target := s.targetLocked(id)
	if _, ok := env[id]; ok {
		return false, nil // already granted by the environment
	}
	if _, ok := target[id]; ok {
		return false, nil
	}
	target[id] = struct{}{}

	if err := s.saveLocked(); err != nil {
		delete(target, id) // keep memory consistent with disk
		return false, err
	}
	return true, nil
}

// Remove revokes access for id. It returns ErrStatic when the entry comes from
// the environment, since the process cannot change that.
func (s *Store) Remove(id int64) (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.admins[id]; ok {
		return false, ErrStatic
	}
	env, target := s.targetLocked(id)
	if _, ok := env[id]; ok {
		return false, ErrStatic
	}
	if _, ok := target[id]; !ok {
		return false, nil
	}
	delete(target, id)

	if err := s.saveLocked(); err != nil {
		target[id] = struct{}{}
		return false, err
	}
	return true, nil
}

// Entry describes one whitelist member for display.
type Entry struct {
	ID     int64
	IsChat bool
	Static bool // came from the environment, so it cannot be removed at runtime
	Admin  bool
}

// List returns every entry, admins first, then users, then chats, each sorted.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var admins, users, chats []Entry
	for id := range s.admins {
		admins = append(admins, Entry{ID: id, Static: true, Admin: true})
	}
	for id := range s.envUsers {
		if _, isAdmin := s.admins[id]; !isAdmin {
			users = append(users, Entry{ID: id, Static: true})
		}
	}
	for id := range s.dynUsers {
		users = append(users, Entry{ID: id})
	}
	for id := range s.envChats {
		chats = append(chats, Entry{ID: id, IsChat: true, Static: true})
	}
	for id := range s.dynChats {
		chats = append(chats, Entry{ID: id, IsChat: true})
	}

	byID := func(a, b Entry) int { return cmp.Compare(a.ID, b.ID) }
	slices.SortFunc(admins, byID)
	slices.SortFunc(users, byID)
	slices.SortFunc(chats, byID)

	return slices.Concat(admins, users, chats)
}

// isOpenLocked requires at least a read lock.
func (s *Store) isOpenLocked() bool {
	return len(s.admins) == 0 &&
		len(s.envUsers) == 0 && len(s.envChats) == 0 &&
		len(s.dynUsers) == 0 && len(s.dynChats) == 0
}

// targetLocked returns the environment and runtime sets that id belongs to.
func (s *Store) targetLocked(id int64) (env, dyn map[int64]struct{}) {
	if id < 0 {
		return s.envChats, s.dynChats
	}
	return s.envUsers, s.dynUsers
}

// saveLocked persists runtime additions. It writes to a temporary file and
// renames it, so a crash mid-write cannot leave a truncated state file behind.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}

	snapshot := state{Users: []int64{}, Chats: []int64{}}
	for id := range s.dynUsers {
		snapshot.Users = append(snapshot.Users, id)
	}
	for id := range s.dynChats {
		snapshot.Chats = append(snapshot.Chats, id)
	}
	slices.Sort(snapshot.Users)
	slices.Sort(snapshot.Chats)

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temp state in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state %s: %w", s.path, err)
	}
	return nil
}

func copySet(src map[int64]struct{}) map[int64]struct{} {
	dst := make(map[int64]struct{}, len(src))
	for id := range src {
		dst[id] = struct{}{}
	}
	return dst
}

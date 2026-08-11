package access

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func ids(list ...int64) map[int64]struct{} {
	set := make(map[int64]struct{}, len(list))
	for _, id := range list {
		set[id] = struct{}{}
	}
	return set
}

// newStore builds a Store backed by a state file inside the test's temp dir.
func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.StateFile == "" {
		opts.StateFile = filepath.Join(t.TempDir(), "state.json")
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s
}

func TestAllowed(t *testing.T) {
	tests := []struct {
		name   string
		opts   Options
		userID int64
		chatID int64
		want   bool
	}{
		{
			name:   "empty list allows everyone",
			userID: 999, chatID: -100,
			want: true,
		},
		{
			name:   "admin is allowed anywhere",
			opts:   Options{Admins: ids(111)},
			userID: 111, chatID: -999,
			want: true,
		},
		{
			name:   "whitelisted user is allowed in any chat",
			opts:   Options{EnvUsers: ids(111)},
			userID: 111, chatID: -999,
			want: true,
		},
		{
			name:   "stranger is rejected",
			opts:   Options{EnvUsers: ids(111)},
			userID: 222, chatID: -999,
			want: false,
		},
		{
			name:   "anyone is allowed inside a whitelisted chat",
			opts:   Options{EnvChats: ids(-100)},
			userID: 222, chatID: -100,
			want: true,
		},
		{
			name:   "either match is enough",
			opts:   Options{EnvUsers: ids(111), EnvChats: ids(-100)},
			userID: 222, chatID: -100,
			want: true,
		},
		{
			name:   "unknown sender is rejected when a whitelist exists",
			opts:   Options{EnvUsers: ids(111)},
			userID: 0, chatID: -200,
			want: false,
		},
		{
			name:   "an admin alone closes the open list",
			opts:   Options{Admins: ids(111)},
			userID: 222, chatID: -200,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newStore(t, tt.opts).Allowed(tt.userID, tt.chatID)
			if got != tt.want {
				t.Errorf("Allowed(%d, %d) = %v, want %v", tt.userID, tt.chatID, got, tt.want)
			}
		})
	}
}

func TestAddGrantsAccess(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111)})

	if s.Allowed(222, 333) {
		t.Fatal("Allowed(222) = true before adding, want false")
	}

	changed, err := s.Add(222)
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if !changed {
		t.Error("Add() changed = false, want true for a new entry")
	}
	if !s.Allowed(222, 333) {
		t.Error("Allowed(222) = false after Add, want true")
	}

	changed, err = s.Add(222)
	if err != nil {
		t.Fatalf("Add() second call error = %v", err)
	}
	if changed {
		t.Error("Add() changed = true for an existing entry, want false")
	}
}

func TestAddNegativeIDGrantsChatAccess(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111)})

	if _, err := s.Add(-100500); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if !s.Allowed(777, -100500) {
		t.Error("Allowed(777, -100500) = false, want a negative id to whitelist the chat")
	}
	if s.Allowed(-100500, 42) {
		t.Error("a negative id must not whitelist a user with that id")
	}
}

func TestRemove(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111)})
	if _, err := s.Add(222); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	changed, err := s.Remove(222)
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if !changed {
		t.Error("Remove() changed = false, want true")
	}
	if s.Allowed(222, 333) {
		t.Error("Allowed(222) = true after Remove, want false")
	}

	changed, err = s.Remove(222)
	if err != nil {
		t.Fatalf("Remove() of a missing entry error = %v, want nil", err)
	}
	if changed {
		t.Error("Remove() changed = true for a missing entry, want false")
	}
}

func TestRemoveRefusesEnvironmentEntries(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111), EnvUsers: ids(222), EnvChats: ids(-100)})

	for _, id := range []int64{111, 222, -100} {
		if _, err := s.Remove(id); !errors.Is(err, ErrStatic) {
			t.Errorf("Remove(%d) error = %v, want ErrStatic", id, err)
		}
		if !s.Allowed(id, id) {
			t.Errorf("Allowed(%d) = false; a refused removal must not take effect", id)
		}
	}
}

func TestAddIsANoOpForEnvironmentEntries(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111), EnvUsers: ids(222)})

	for _, id := range []int64{111, 222} {
		changed, err := s.Add(id)
		if err != nil {
			t.Fatalf("Add(%d) error = %v", id, err)
		}
		if changed {
			t.Errorf("Add(%d) changed = true, want false: it is already granted by the environment", id)
		}
	}
}

func TestAdditionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	first := newStore(t, Options{Admins: ids(111), StateFile: path})
	if _, err := first.Add(222); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := first.Add(-100500); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	second := newStore(t, Options{Admins: ids(111), StateFile: path})
	if !second.Allowed(222, 0) {
		t.Error("added user was lost across restart")
	}
	if !second.Allowed(777, -100500) {
		t.Error("added chat was lost across restart")
	}
}

func TestRemovalsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	first := newStore(t, Options{Admins: ids(111), StateFile: path})
	if _, err := first.Add(222); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := first.Remove(222); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	if second := newStore(t, Options{Admins: ids(111), StateFile: path}); second.Allowed(222, 0) {
		t.Error("removed user came back after restart")
	}
}

func TestMissingStateFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := New(Options{StateFile: path}); err != nil {
		t.Errorf("New() error = %v, want a missing state file to be treated as a first run", err)
	}
}

func TestCorruptStateFileIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Starting with a silently empty whitelist would be worse than refusing to
	// start, because it opens the bot to everyone.
	if _, err := New(Options{StateFile: path}); err == nil {
		t.Error("New() error = nil for a corrupt state file, want an error")
	}
}

func TestEmptyStateFilePathKeepsChangesInMemory(t *testing.T) {
	s, err := New(Options{Admins: ids(111), StateFile: ""})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := s.Add(222); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if !s.Allowed(222, 0) {
		t.Error("Allowed(222) = false, want in-memory additions to work without persistence")
	}
}

func TestList(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111), EnvUsers: ids(222), EnvChats: ids(-100)})
	if _, err := s.Add(333); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	got := s.List()
	want := []Entry{
		{ID: 111, Static: true, Admin: true},
		{ID: 222, Static: true},
		{ID: 333},
		{ID: -100, IsChat: true, Static: true},
	}

	if len(got) != len(want) {
		t.Fatalf("List() = %+v, want %d entries", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListDoesNotDuplicateAnAdminWhoIsAlsoWhitelisted(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111), EnvUsers: ids(111)})

	if got := s.List(); len(got) != 1 {
		t.Errorf("List() = %+v, want a single entry for an admin listed twice", got)
	}
}

func TestOpen(t *testing.T) {
	if !newStore(t, Options{}).Open() {
		t.Error("Open() = false for an empty store, want true")
	}
	if newStore(t, Options{Admins: ids(1)}).Open() {
		t.Error("Open() = true with an admin, want false")
	}

	s := newStore(t, Options{})
	if _, err := s.Add(222); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if s.Open() {
		t.Error("Open() = true after a runtime addition, want false")
	}
}

func TestNewCopiesInput(t *testing.T) {
	users := ids(111)
	s := newStore(t, Options{EnvUsers: users})

	users[222] = struct{}{} // must not widen access after construction

	if s.Allowed(222, 0) {
		t.Error("Allowed(222) = true; New() must copy its input, not alias it")
	}
}

func TestConcurrentUse(t *testing.T) {
	s := newStore(t, Options{Admins: ids(111)})

	var wg sync.WaitGroup
	for i := int64(0); i < 20; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = s.Add(1000 + i) }()
		go func() { defer wg.Done(); _ = s.Allowed(1000+i, -1) }()
		go func() { defer wg.Done(); _ = s.List() }()
	}
	wg.Wait()

	for i := int64(0); i < 20; i++ {
		if !s.Allowed(1000+i, 0) {
			t.Errorf("Allowed(%d) = false, want every concurrent Add to have landed", 1000+i)
		}
	}
}

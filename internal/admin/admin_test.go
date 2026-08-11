package admin

import (
	"errors"
	"strings"
	"testing"

	"dr1wbot/internal/access"
)

const adminID int64 = 111

// fakeStore records mutations so tests can assert on them.
type fakeStore struct {
	admins  map[int64]bool
	entries []access.Entry

	added, removed []int64
	changed        bool
	err            error
}

func newFakeStore() *fakeStore {
	return &fakeStore{admins: map[int64]bool{adminID: true}, changed: true}
}

func (f *fakeStore) IsAdmin(userID int64) bool { return f.admins[userID] }

func (f *fakeStore) Add(id int64) (bool, error) {
	f.added = append(f.added, id)
	return f.changed, f.err
}

func (f *fakeStore) Remove(id int64) (bool, error) {
	f.removed = append(f.removed, id)
	return f.changed, f.err
}

func (f *fakeStore) List() []access.Entry { return f.entries }

func TestNonCommandsAreLeftToTheModel(t *testing.T) {
	c := New(newFakeStore())

	for _, text := range []string{
		"",
		"   ",
		"что такое кворум?",
		"add 821609332",             // the mistake that started this: no slash
		"расскажи про /add в linux", // a slash that is not the first word
		"/unknown 123",
		"/",
	} {
		if reply, handled := c.Handle(text, adminID); handled {
			t.Errorf("Handle(%q) handled = true (reply %q), want it treated as a question", text, reply)
		}
	}
}

func TestNonAdminIsRefused(t *testing.T) {
	store := newFakeStore()
	c := New(store)

	reply, handled := c.Handle("/add 222", 999)
	if !handled {
		t.Fatal("Handle() handled = false, want the command recognised")
	}
	if !strings.Contains(reply, "администратор") {
		t.Errorf("reply = %q, want it to say the caller is not an admin", reply)
	}
	if len(store.added) != 0 {
		t.Errorf("added = %v, want no mutation from a non-admin", store.added)
	}
}

func TestAdd(t *testing.T) {
	store := newFakeStore()
	c := New(store)

	reply, handled := c.Handle("/add 821609332", adminID)
	if !handled {
		t.Fatal("Handle() handled = false, want true")
	}
	if len(store.added) != 1 || store.added[0] != 821609332 {
		t.Errorf("added = %v, want [821609332]", store.added)
	}
	if !strings.Contains(reply, "821609332") {
		t.Errorf("reply = %q, want it to confirm the id", reply)
	}
}

func TestRemove(t *testing.T) {
	store := newFakeStore()
	c := New(store)

	if _, handled := c.Handle("/del -100500", adminID); !handled {
		t.Fatal("Handle() handled = false, want true")
	}
	if len(store.removed) != 1 || store.removed[0] != -100500 {
		t.Errorf("removed = %v, want [-100500]", store.removed)
	}
}

func TestCommandWithBotSuffix(t *testing.T) {
	store := newFakeStore()
	c := New(store)

	// Telegram writes "/add@botname" when more than one bot is in the chat.
	if _, handled := c.Handle("/add@dr0wbot 222", adminID); !handled {
		t.Fatal("Handle() handled = false for /add@botname, want true")
	}
	if len(store.added) != 1 || store.added[0] != 222 {
		t.Errorf("added = %v, want [222]", store.added)
	}
}

func TestBadArguments(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		wantIn string
	}{
		{name: "no id", text: "/add", wantIn: "ровно один ID"},
		{name: "two ids", text: "/add 111 222", wantIn: "ровно один ID"},
		{name: "username instead of id", text: "/add @vasya", wantIn: "не числовой ID"},
		{name: "not a number", text: "/del abc", wantIn: "не числовой ID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			reply, handled := New(store).Handle(tt.text, adminID)
			if !handled {
				t.Fatal("Handle() handled = false, want the command recognised")
			}
			if !strings.Contains(reply, tt.wantIn) {
				t.Errorf("reply = %q, want it to mention %q", reply, tt.wantIn)
			}
			if len(store.added)+len(store.removed) != 0 {
				t.Error("a malformed command must not mutate the list")
			}
		})
	}
}

func TestStaticEntryExplainsItself(t *testing.T) {
	store := newFakeStore()
	store.err = access.ErrStatic
	c := New(store)

	reply, _ := c.Handle("/del 222", adminID)
	if !strings.Contains(reply, ".env") {
		t.Errorf("reply = %q, want it to point at the .env file", reply)
	}
}

func TestStorageFailureIsReported(t *testing.T) {
	store := newFakeStore()
	store.err = errors.New("disk full")
	c := New(store)

	reply, _ := c.Handle("/add 222", adminID)
	if !strings.Contains(reply, "disk full") {
		t.Errorf("reply = %q, want it to surface the storage error", reply)
	}
}

func TestNoOpMutationsSayNothingChanged(t *testing.T) {
	store := newFakeStore()
	store.changed = false
	c := New(store)

	if reply, _ := c.Handle("/add 222", adminID); !strings.Contains(reply, "уже в списке") {
		t.Errorf("reply = %q, want it to say the entry was already there", reply)
	}
	if reply, _ := c.Handle("/del 222", adminID); !strings.Contains(reply, "и не было") {
		t.Errorf("reply = %q, want it to say the entry was absent", reply)
	}
}

func TestList(t *testing.T) {
	store := newFakeStore()
	store.entries = []access.Entry{
		{ID: 111, Static: true, Admin: true},
		{ID: 222, Static: true},
		{ID: 333},
		{ID: -100, IsChat: true},
	}

	reply, handled := New(store).Handle("/list", adminID)
	if !handled {
		t.Fatal("Handle() handled = false, want true")
	}
	for _, want := range []string{"111", "админ", "222", ".env", "333", "-100", "чат"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply = %q, want it to contain %q", reply, want)
		}
	}
}

func TestEmptyListWarns(t *testing.T) {
	reply, _ := New(newFakeStore()).Handle("/list", adminID)
	if !strings.Contains(reply, "кто угодно") {
		t.Errorf("reply = %q, want an empty list to warn that the bot is open", reply)
	}
}

func TestHelp(t *testing.T) {
	reply, handled := New(newFakeStore()).Handle("/help", adminID)
	if !handled {
		t.Fatal("Handle() handled = false, want true")
	}
	for _, want := range []string{"/add", "/del", "/list"} {
		if !strings.Contains(reply, want) {
			t.Errorf("help = %q, want it to document %q", reply, want)
		}
	}
}

// fakePardoner records who was let back in.
type fakePardoner struct{ pardoned []int64 }

func (f *fakePardoner) Pardon(id int64) { f.pardoned = append(f.pardoned, id) }

func TestUnban(t *testing.T) {
	pardoner := &fakePardoner{}
	c := New(&fakeStore{admins: map[int64]bool{1: true}}).WithPardoner(pardoner)

	reply, handled := c.Handle("/unban 555", 1)
	if !handled {
		t.Fatal("/unban was not handled")
	}
	if len(pardoner.pardoned) != 1 || pardoner.pardoned[0] != 555 {
		t.Errorf("pardoned %v, want [555]", pardoner.pardoned)
	}
	if !strings.Contains(reply, "555") {
		t.Errorf("reply = %q, want it to name the id", reply)
	}
}

func TestUnbanRejectsAChatID(t *testing.T) {
	pardoner := &fakePardoner{}
	c := New(&fakeStore{admins: map[int64]bool{1: true}}).WithPardoner(pardoner)

	// Bans are per person; a chat has no allowance to abuse.
	if _, handled := c.Handle("/unban -100500", 1); !handled {
		t.Fatal("/unban was not handled")
	}
	if len(pardoner.pardoned) != 0 {
		t.Errorf("pardoned %v, want a chat id refused", pardoner.pardoned)
	}
}

func TestUnbanWithoutPublicAccess(t *testing.T) {
	c := New(&fakeStore{admins: map[int64]bool{1: true}})

	reply, handled := c.Handle("/unban 555", 1)
	if !handled {
		t.Fatal("/unban was not handled")
	}
	if !strings.Contains(reply, "выключен") {
		t.Errorf("reply = %q, want it to say there is nothing to unban", reply)
	}
}

func TestUnbanIsAdminOnly(t *testing.T) {
	pardoner := &fakePardoner{}
	c := New(&fakeStore{admins: map[int64]bool{1: true}}).WithPardoner(pardoner)

	if _, handled := c.Handle("/unban 555", 999); !handled {
		t.Fatal("/unban was not handled")
	}
	if len(pardoner.pardoned) != 0 {
		t.Errorf("pardoned %v, want a non-admin refused", pardoner.pardoned)
	}
}

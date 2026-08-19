package admin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dr1wbot/internal/access"
	"dr1wbot/internal/quota"
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
		"add 123456789",             // the mistake that started this: no slash
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

	reply, handled := c.Handle("/add 123456789", adminID)
	if !handled {
		t.Fatal("Handle() handled = false, want true")
	}
	if len(store.added) != 1 || store.added[0] != 123456789 {
		t.Errorf("added = %v, want [123456789]", store.added)
	}
	if !strings.Contains(reply, "123456789") {
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
	for _, want := range []string{"/add", "/del", "/list", "/ban", "/unban", "/bans"} {
		if !strings.Contains(reply, want) {
			t.Errorf("help = %q, want it to document %q", reply, want)
		}
	}
}

// fakeBouncer records what the ban commands did.
type fakeBouncer struct {
	pardoned  []int64
	unnamed   []string
	banned    []bannedCall
	byName    []bannedName
	directory map[string]int64
	list      []quota.Ban
	lifted    bool
}

type bannedCall struct {
	id   int64
	span time.Duration
}

type bannedName struct {
	name string
	span time.Duration
}

func (f *fakeBouncer) Pardon(id int64) { f.pardoned = append(f.pardoned, id) }

func (f *fakeBouncer) PardonName(name string) bool {
	f.unnamed = append(f.unnamed, name)
	return f.lifted
}

func (f *fakeBouncer) Ban(id int64, d time.Duration) {
	f.banned = append(f.banned, bannedCall{id: id, span: d})
}

func (f *fakeBouncer) BanName(name string, d time.Duration) (int64, bool) {
	f.byName = append(f.byName, bannedName{name: name, span: d})
	id, known := f.directory[name]
	return id, known
}

func (f *fakeBouncer) Lookup(name string) (int64, bool) {
	id, known := f.directory[name]
	return id, known
}

func (f *fakeBouncer) Bans(int) []quota.Ban { return f.list }

func adminOnly() *fakeStore { return &fakeStore{admins: map[int64]bool{1: true}} }

func TestBanWithoutATimeIsForever(t *testing.T) {
	bouncer := &fakeBouncer{}
	c := New(adminOnly()).WithBouncer(bouncer)

	reply, handled := c.Handle("/ban 555", 1)
	if !handled {
		t.Fatal("/ban was not handled")
	}
	if len(bouncer.banned) != 1 || bouncer.banned[0] != (bannedCall{id: 555, span: 0}) {
		t.Fatalf("banned = %+v, want 555 banned with no end", bouncer.banned)
	}
	if !strings.Contains(reply, "навсегда") {
		t.Errorf("reply = %q, want it to say the ban is permanent", reply)
	}
}

func TestBanForATime(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
	}{
		{"/ban 555 2ч", 2 * time.Hour},
		{"/ban 555 30м", 30 * time.Minute},
		{"/ban 555 7д", 7 * 24 * time.Hour},
		{"/ban 555 1нед", 7 * 24 * time.Hour},
		{"/ban 555 2h", 2 * time.Hour},
		{"/ban 555 90", 90 * time.Minute}, // a bare number is minutes
		{"/ban 555 1д12ч", 36 * time.Hour},
		{"/ban 555 навсегда", 0},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			bouncer := &fakeBouncer{}
			New(adminOnly()).WithBouncer(bouncer).Handle(tt.text, 1)
			if len(bouncer.banned) != 1 || bouncer.banned[0].span != tt.want {
				t.Errorf("banned = %+v, want a span of %s", bouncer.banned, tt.want)
			}
		})
	}
}

func TestBanRejectsAnUnreadableTime(t *testing.T) {
	bouncer := &fakeBouncer{}
	reply, _ := New(adminOnly()).WithBouncer(bouncer).Handle("/ban 555 вчера", 1)

	if len(bouncer.banned) != 0 {
		t.Errorf("banned = %+v, want nothing banned on a bad span", bouncer.banned)
	}
	if !strings.Contains(reply, "⚠️") {
		t.Errorf("reply = %q, want a complaint about the span", reply)
	}
}

func TestBanByUsername(t *testing.T) {
	bouncer := &fakeBouncer{directory: map[string]int64{"spammer": 777}}
	reply, _ := New(adminOnly()).WithBouncer(bouncer).Handle("/ban @spammer 1д", 1)

	if len(bouncer.byName) != 1 || bouncer.byName[0] != (bannedName{name: "spammer", span: 24 * time.Hour}) {
		t.Fatalf("byName = %+v, want @spammer banned for a day", bouncer.byName)
	}
	if !strings.Contains(reply, "777") {
		t.Errorf("reply = %q, want the resolved id shown", reply)
	}
}

func TestBanAnUnseenUsernameStillSticks(t *testing.T) {
	bouncer := &fakeBouncer{}
	reply, _ := New(adminOnly()).WithBouncer(bouncer).Handle("/ban @ghost", 1)

	if len(bouncer.byName) != 1 {
		t.Fatalf("byName = %+v, want the ban recorded against the name", bouncer.byName)
	}
	// Telegram cannot resolve a name to an id, so the ban has to wait for its
	// target; refusing to place it would only help the target.
	if !strings.Contains(reply, "как только он напишет") {
		t.Errorf("reply = %q, want it to explain when the ban takes effect", reply)
	}
}

func TestBanRefusesAnAdmin(t *testing.T) {
	bouncer := &fakeBouncer{}
	reply, _ := New(adminOnly()).WithBouncer(bouncer).Handle("/ban 1", 1)

	if len(bouncer.banned) != 0 {
		t.Errorf("banned = %+v, want an admin refused", bouncer.banned)
	}
	if !strings.Contains(reply, "админ") {
		t.Errorf("reply = %q, want it to say why", reply)
	}
}

func TestBanIsAdminOnly(t *testing.T) {
	bouncer := &fakeBouncer{}
	if _, handled := New(adminOnly()).WithBouncer(bouncer).Handle("/ban 555", 999); !handled {
		t.Fatal("/ban was not handled")
	}
	if len(bouncer.banned) != 0 {
		t.Errorf("banned = %+v, want a non-admin refused", bouncer.banned)
	}
}

func TestBansLists(t *testing.T) {
	bouncer := &fakeBouncer{list: []quota.Ban{
		{ID: 555, Username: "spammer", Forever: true, Manual: true},
		{ID: 777, Until: time.Now().Add(time.Hour), Warns: 2},
		{Username: "ghost", Forever: true, Manual: true},
	}}
	reply, handled := New(adminOnly()).WithBouncer(bouncer).Handle("/bans", 1)
	if !handled {
		t.Fatal("/bans was not handled")
	}
	for _, want := range []string{"555", "@spammer", "777", "@ghost", "вручную", "автобан"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply = %q, want it to mention %q", reply, want)
		}
	}
}

func TestBansWhenNobodyIsBanned(t *testing.T) {
	reply, _ := New(adminOnly()).WithBouncer(&fakeBouncer{}).Handle("/bans", 1)
	if !strings.Contains(reply, "нет") {
		t.Errorf("reply = %q, want it to say the list is empty", reply)
	}
}

func TestUnban(t *testing.T) {
	bouncer := &fakeBouncer{}
	c := New(adminOnly()).WithBouncer(bouncer)

	reply, handled := c.Handle("/unban 555", 1)
	if !handled {
		t.Fatal("/unban was not handled")
	}
	if len(bouncer.pardoned) != 1 || bouncer.pardoned[0] != 555 {
		t.Errorf("pardoned %v, want [555]", bouncer.pardoned)
	}
	if !strings.Contains(reply, "555") {
		t.Errorf("reply = %q, want it to name the id", reply)
	}
}

func TestUnbanByUsername(t *testing.T) {
	bouncer := &fakeBouncer{lifted: true}
	reply, _ := New(adminOnly()).WithBouncer(bouncer).Handle("/unban @spammer", 1)

	if len(bouncer.unnamed) != 1 || bouncer.unnamed[0] != "spammer" {
		t.Fatalf("unnamed = %v, want [spammer]", bouncer.unnamed)
	}
	if !strings.Contains(reply, "снят") {
		t.Errorf("reply = %q, want it to confirm the ban was lifted", reply)
	}
}

func TestUnbanRejectsAChatID(t *testing.T) {
	bouncer := &fakeBouncer{}
	c := New(adminOnly()).WithBouncer(bouncer)

	// Bans are per person; a chat has no allowance to abuse.
	if _, handled := c.Handle("/unban -100500", 1); !handled {
		t.Fatal("/unban was not handled")
	}
	if len(bouncer.pardoned) != 0 {
		t.Errorf("pardoned %v, want a chat id refused", bouncer.pardoned)
	}
}

func TestBanCommandsWithoutABouncer(t *testing.T) {
	c := New(adminOnly())

	for _, command := range []string{"/ban 555", "/unban 555", "/bans"} {
		reply, handled := c.Handle(command, 1)
		if !handled {
			t.Fatalf("%q was not handled", command)
		}
		if !strings.Contains(reply, "недоступны") {
			t.Errorf("%q = %q, want it to say bans are not available", command, reply)
		}
	}
}

func TestUnbanIsAdminOnly(t *testing.T) {
	bouncer := &fakeBouncer{}
	c := New(adminOnly()).WithBouncer(bouncer)

	if _, handled := c.Handle("/unban 555", 999); !handled {
		t.Fatal("/unban was not handled")
	}
	if len(bouncer.pardoned) != 0 {
		t.Errorf("pardoned %v, want a non-admin refused", bouncer.pardoned)
	}
}

package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clock is a settable time source for the store.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func newStore(t *testing.T, limit int, file string, c *clock) *Store {
	t.Helper()
	s, err := New(Options{Limit: limit, File: file, Now: c.now})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s
}

func TestDisabledByDefault(t *testing.T) {
	c := &clock{at: time.Now()}
	s := newStore(t, 0, "", c)

	if s.Enabled() {
		t.Error("Enabled() = true with no limit, want the bot to stay private")
	}
	if _, _, ok := s.Take(111); ok {
		t.Error("Take() granted a request with no limit configured")
	}
}

func TestTakeCountsDownAndThenRefuses(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)}
	s := newStore(t, 3, "", c)

	for i := 1; i <= 3; i++ {
		used, limit, ok := s.Take(111)
		if !ok || used != i || limit != 3 {
			t.Fatalf("Take() #%d = (%d, %d, %t), want (%d, 3, true)", i, used, limit, ok, i)
		}
	}

	used, _, ok := s.Take(111)
	if ok {
		t.Error("Take() granted a fourth request over a limit of three")
	}
	// A refused request must not count, or the number shown to the user would
	// climb every time they retried.
	if used != 3 {
		t.Errorf("used = %d after a refusal, want it to stay at the limit", used)
	}
}

func TestAllowanceIsPerUser(t *testing.T) {
	c := &clock{at: time.Now()}
	s := newStore(t, 1, "", c)

	if _, _, ok := s.Take(111); !ok {
		t.Fatal("Take() refused the first user")
	}
	if _, _, ok := s.Take(222); !ok {
		t.Error("Take() refused a different user, want one allowance each")
	}
}

func TestCountersResetAtMidnightUTC(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 23, 59, 0, 0, time.UTC)}
	s := newStore(t, 1, "", c)

	if _, _, ok := s.Take(111); !ok {
		t.Fatal("Take() refused the first request")
	}
	if _, _, ok := s.Take(111); ok {
		t.Fatal("Take() granted a second request over a limit of one")
	}

	c.at = c.at.Add(2 * time.Minute) // past midnight
	if _, _, ok := s.Take(111); !ok {
		t.Error("Take() still refused after the day turned, want a fresh allowance")
	}
}

func TestResetsInCountsToMidnight(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 22, 0, 0, 0, time.UTC)}
	s := newStore(t, 5, "", c)

	if got := s.Stats().ResetsIn; got != 2*time.Hour {
		t.Errorf("ResetsIn = %s, want 2h", got)
	}
}

func TestStatsSummariseTheDay(t *testing.T) {
	c := &clock{at: time.Now()}
	s := newStore(t, 5, "", c)

	s.Take(111)
	s.Take(111)
	s.Take(222)

	got := s.Stats()
	if got.Users != 2 || got.Requests != 3 || got.Limit != 5 {
		t.Errorf("Stats() = %+v, want 2 users, 3 requests, limit 5", got)
	}
}

func TestCountersSurviveARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "quota.json")
	c := &clock{at: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)}

	first := newStore(t, 3, file, c)
	first.Take(111)
	first.Take(111)

	// A restart must not hand everyone a fresh day, or the limit would be one
	// deploy away from meaningless.
	second := newStore(t, 3, file, c)
	if used, _ := second.Peek(111); used != 2 {
		t.Errorf("used after restart = %d, want 2 carried over from disk", used)
	}
}

func TestYesterdaysFileIsIgnored(t *testing.T) {
	file := filepath.Join(t.TempDir(), "quota.json")
	c := &clock{at: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)}

	first := newStore(t, 3, file, c)
	first.Take(111)

	c.at = c.at.Add(24 * time.Hour)
	second := newStore(t, 3, file, c)
	if used, _ := second.Peek(111); used != 0 {
		t.Errorf("used = %d on a new day, want the old file ignored", used)
	}
}

func TestCorruptFileDoesNotBlockStartup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A broken counter file is worth reporting, not worth crashing over
	// silently: the caller decides.
	if _, err := New(Options{Limit: 3, File: file}); err == nil {
		t.Error("New() error = nil for a corrupt file, want the operator told")
	}
}

func TestPeekDoesNotSpend(t *testing.T) {
	c := &clock{at: time.Now()}
	s := newStore(t, 2, "", c)

	if used, limit := s.Peek(111); used != 0 || limit != 2 {
		t.Errorf("Peek() = (%d, %d), want (0, 2)", used, limit)
	}
	if used, _ := s.Peek(111); used != 0 {
		t.Error("Peek() spent an allowance")
	}
}

func TestBurstEarnsABan(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)}
	s, err := New(Options{Limit: 100, Burst: 3, Window: time.Minute, BanFor: 15 * time.Minute, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		if v, _, _ := s.Judge(111); v != Granted {
			t.Fatalf("Judge() #%d = %v, want Granted", i, v)
		}
	}
	// The fourth inside the window is a script, not a person.
	if v, _, _ := s.Judge(111); v != Banned {
		t.Errorf("Judge() = %v, want Banned after the burst", v)
	}
	if s.BannedUntil(111).IsZero() {
		t.Error("BannedUntil() = zero, want the ban recorded")
	}
}

func TestABanDoesNotSpendTheAllowance(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 100, Burst: 2, Window: time.Minute, BanFor: time.Minute, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	s.Judge(111)
	before, _ := s.Peek(111)
	s.Judge(111) // banned
	after, _ := s.Peek(111)

	if after != before {
		t.Errorf("used went %d -> %d, want a refused request to cost nothing", before, after)
	}
}

func TestEnoughWarnsEarnAPermanentBan(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)}
	s, err := New(Options{
		Limit: 100, Burst: 2, Window: time.Minute,
		BanFor: 10 * time.Minute, WarnTTL: 30 * 24 * time.Hour, MaxWarns: 3,
		Now: c.now,
	})
	if err != nil {
		t.Fatal(err)
	}

	strike := func() {
		s.Judge(111)
		s.Judge(111)
		s.Judge(111) // the one that warns
	}

	strike()
	if got := s.BannedUntil(111).Sub(c.now()); got != 10*time.Minute {
		t.Fatalf("first ban = %s, want the fixed 10m", got)
	}

	c.at = c.at.Add(11 * time.Minute) // serve it out
	strike()
	if got := s.BannedUntil(111).Sub(c.now()); got != 10*time.Minute {
		t.Errorf("second ban = %s, want the same fixed length", got)
	}

	c.at = c.at.Add(11 * time.Minute)
	strike() // third warn
	// Three live warns is not a bad evening any more.
	if v, _, _ := s.Judge(111); v != Banned {
		t.Errorf("Judge() = %v, want Banned", v)
	}
	c.at = c.at.Add(365 * 24 * time.Hour)
	if v, _, _ := s.Judge(111); v != Banned {
		t.Errorf("Judge() = %v a year later, want the ban to be permanent", v)
	}
}

func TestWarnsExpire(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)}
	s, err := New(Options{
		Limit: 100, Burst: 2, Window: time.Minute,
		BanFor: time.Minute, WarnTTL: 30 * 24 * time.Hour, MaxWarns: 2,
		Now: c.now,
	})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	s.Judge(111)
	s.Judge(111) // warn 1

	// A month later the first warn no longer counts, so the next violation is
	// a first offence again rather than the one that bans forever.
	c.at = c.at.Add(31 * 24 * time.Hour)
	s.Judge(111)
	s.Judge(111)
	s.Judge(111) // warn 1 again, not warn 2

	if s.Callers(10)[0].Forever {
		t.Error("caller was banned forever on a warn that should have expired")
	}
}

func TestPardonClearsABan(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 100, Burst: 1, Window: time.Minute, BanFor: time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	s.Judge(111)
	if s.BannedUntil(111).IsZero() {
		t.Fatal("caller was not banned")
	}

	s.Pardon(111)
	if !s.BannedUntil(111).IsZero() {
		t.Error("BannedUntil() still set after a pardon")
	}
	if v, _, _ := s.Judge(111); v != Granted {
		t.Errorf("Judge() = %v after a pardon, want Granted", v)
	}
}

func TestABanOutlivesMidnight(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 23, 50, 0, 0, time.UTC)}
	s, err := New(Options{Limit: 100, Burst: 1, Window: time.Minute, BanFor: time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	s.Judge(111) // banned for an hour, crossing midnight

	c.at = c.at.Add(20 * time.Minute) // new day, ban still running
	// Waiting for the daily reset must not wash a ban off, or every script
	// would simply sleep until midnight.
	if v, _, _ := s.Judge(111); v != Banned {
		t.Errorf("Judge() = %v after midnight, want the ban to outlive the day", v)
	}
}

func TestABanSurvivesARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "quota.json")
	c := &clock{at: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)}
	opts := Options{Limit: 100, Burst: 1, Window: time.Minute, BanFor: time.Hour, File: file, Now: c.now}

	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	first.Judge(111)
	first.Judge(111) // banned

	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := second.Judge(111); v != Banned {
		t.Errorf("Judge() = %v after a restart, want the ban reloaded", v)
	}
}

func TestBurstWindowSlidesOpenAgain(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)}
	s, err := New(Options{Limit: 100, Burst: 2, Window: time.Minute, BanFor: time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	s.Judge(111)

	// A person who waits out the window is not a script.
	c.at = c.at.Add(2 * time.Minute)
	if v, _, _ := s.Judge(111); v != Granted {
		t.Errorf("Judge() = %v, want the window to have slid past the earlier requests", v)
	}
}

func TestStatsCountBans(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 100, Burst: 1, Window: time.Minute, BanFor: time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	s.Judge(111) // 111 is banned
	s.Judge(222)

	got := s.Stats()
	if got.Banned != 1 {
		t.Errorf("Banned = %d, want 1", got.Banned)
	}
	if got.Users != 2 {
		t.Errorf("Users = %d, want both callers counted", got.Users)
	}
}

func TestSetPolicyTakesEffectImmediately(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 1, Burst: 10, Window: time.Minute, BanFor: time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	if v, _, _ := s.Judge(111); v != Spent {
		t.Fatalf("Judge() = %v, want Spent on a limit of one", v)
	}

	s.SetPolicy(5, 0, 10, time.Minute, time.Hour, 0)
	if v, _, _ := s.Judge(111); v != Granted {
		t.Errorf("Judge() = %v after raising the limit, want Granted without a restart", v)
	}
}

func TestGlobalCapStopsEverybody(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 100, GlobalLimit: 3, Burst: 100, Window: time.Minute, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	// Three requests across three different people is still three requests:
	// per-person limits do nothing about the total.
	for _, id := range []int64{111, 222, 333} {
		if v, _, _ := s.Judge(id); v != Granted {
			t.Fatalf("Judge(%d) = %v, want Granted", id, v)
		}
	}
	if v, _, _ := s.Judge(444); v != Exhausted {
		t.Errorf("Judge() = %v, want Exhausted once the bot's own budget is gone", v)
	}
}

func TestGlobalCountResetsWithTheDay(t *testing.T) {
	c := &clock{at: time.Date(2026, 8, 12, 23, 59, 0, 0, time.UTC)}
	s, err := New(Options{Limit: 100, GlobalLimit: 1, Burst: 100, Window: time.Minute, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	if v, _, _ := s.Judge(222); v != Exhausted {
		t.Fatal("the global cap did not bite")
	}

	c.at = c.at.Add(2 * time.Minute)
	if v, _, _ := s.Judge(222); v != Granted {
		t.Errorf("Judge() = %v on a new day, want the global count reset too", v)
	}
}

func TestFreshAccountsGetHalfTheAllowance(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 10, NewAccountThreshold: 7_000_000_000, Burst: 100, Window: time.Minute, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	if _, limit := s.Peek(1_000_000); limit != 10 {
		t.Errorf("old account limit = %d, want the full 10", limit)
	}
	if _, limit := s.Peek(8_000_000_000); limit != 5 {
		t.Errorf("new account limit = %d, want half", limit)
	}
}

func TestAFreshAccountStillGetsSomething(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 1, NewAccountThreshold: 7_000_000_000, Burst: 100, Window: time.Minute, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	// A fresh account is a suspicion, not a conviction: half of one is still
	// one, never zero.
	if _, limit := s.Peek(8_000_000_000); limit != 1 {
		t.Errorf("limit = %d, want at least one request", limit)
	}
}

func TestManualBanAndCallers(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 10, Burst: 100, Window: time.Minute, BanFor: time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	s.Judge(111)
	s.Judge(222)
	s.Ban(222, 0)

	callers := s.Callers(10)
	if len(callers) != 2 {
		t.Fatalf("Callers() = %+v, want both", callers)
	}
	// Banned first: those are the ones an admin came to review.
	if callers[0].ID != 222 || !callers[0].Banned() {
		t.Errorf("Callers()[0] = %+v, want the banned one first", callers[0])
	}
	if v, _, _ := s.Judge(222); v != Banned {
		t.Errorf("Judge() = %v after a manual ban, want Banned", v)
	}
}

func TestCallersIsCapped(t *testing.T) {
	c := &clock{at: time.Now()}
	s, err := New(Options{Limit: 10, Burst: 100, Window: time.Minute, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	for id := int64(1); id <= 20; id++ {
		s.Judge(id)
	}
	// Telegram caps a keyboard's size, and a panel with sixty buttons is not a
	// panel.
	if got := len(s.Callers(8)); got != 8 {
		t.Errorf("Callers(8) returned %d", got)
	}
}

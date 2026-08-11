package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func defaults() Values {
	return Values{
		PublicDailyLimit: 30,
		Burst:            5,
		BurstWindow:      time.Minute,
		BanFor:           15 * time.Minute,
		PublicMaxTokens:  1024,
		PublicMaxRunes:   2000,
		RawFlagEnabled:   true,
	}
}

func TestLoadSeedsFromDefaultsOnFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Load(path, defaults())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := s.Get(); got != defaults() {
		t.Errorf("Get() = %+v, want the environment's values", got)
	}
}

func TestUpdatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	first, err := Load(path, defaults())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Update(func(v *Values) { v.PublicDailyLimit = 100 }); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	// A value changed from the panel must not be undone by the next restart,
	// even though the environment still says 30.
	second, err := Load(path, defaults())
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Get().PublicDailyLimit; got != 100 {
		t.Errorf("PublicDailyLimit = %d after a restart, want the saved 100", got)
	}
}

func TestUnknownFieldsKeepTheirDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	// A file written by an older version knows nothing about later fields.
	if err := os.WriteFile(path, []byte(`{"public_daily_limit":50}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path, defaults())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := s.Get()
	if got.PublicDailyLimit != 50 {
		t.Errorf("PublicDailyLimit = %d, want the saved value", got.PublicDailyLimit)
	}
	if got.Burst != 5 || got.PublicMaxTokens != 1024 {
		t.Errorf("Get() = %+v, want missing fields to keep their defaults rather than arrive as zero", got)
	}
}

func TestCorruptFileIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, defaults()); err == nil {
		t.Error("Load() error = nil for a corrupt file, want the operator told")
	}
}

func TestNoPathKeepsChangesInMemory(t *testing.T) {
	s, err := Load("", defaults())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Update(func(v *Values) { v.Burst = 20 })
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got.Burst != 20 {
		t.Errorf("Burst = %d, want the change applied even with nowhere to save it", got.Burst)
	}
}

func TestCycle(t *testing.T) {
	choices := []int{0, 10, 30}

	if got := Cycle(choices, 0); got != 10 {
		t.Errorf("Cycle() = %d, want the next value", got)
	}
	if got := Cycle(choices, 30); got != 0 {
		t.Errorf("Cycle() = %d, want it to wrap", got)
	}
	// A value that is not on the list — say, one set by hand in the file —
	// snaps to the first choice rather than sticking.
	if got := Cycle(choices, 77); got != 0 {
		t.Errorf("Cycle() = %d, want an off-list value to snap to the first choice", got)
	}
	if got := Cycle([]int{}, 5); got != 5 {
		t.Errorf("Cycle() = %d, want an empty list to change nothing", got)
	}
}

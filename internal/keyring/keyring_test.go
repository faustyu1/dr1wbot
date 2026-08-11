package keyring

import (
	"sync"
	"testing"
	"time"
)

// order is the key order a Lease hands out, for readable assertions.
func order(r *Ring) []string {
	leases := r.Lease()
	out := make([]string, len(leases))
	for i, l := range leases {
		out[i] = l.Key
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewDropsBlanks(t *testing.T) {
	r := New([]string{"one", "", "two", "   "}, time.Minute)
	// Only the empty string is dropped; trimming belongs to the caller, and a
	// key of spaces is still a key we would have to try.
	if got := r.Len(); got != 3 {
		t.Errorf("Len() = %d, want the blank entry dropped", got)
	}
}

func TestLeaseStartsAtTheCursorAndCoversEveryKey(t *testing.T) {
	r := New([]string{"one", "two", "three"}, time.Minute)
	if got := order(r); !equal(got, []string{"one", "two", "three"}) {
		t.Errorf("Lease() = %v, want every key in order", got)
	}
}

func TestLimitMovesToTheNextKey(t *testing.T) {
	now := time.Now()
	r := New([]string{"one", "two", "three"}, time.Minute)
	r.now = func() time.Time { return now }

	r.Limit(0)
	if got := order(r); !equal(got, []string{"two", "three", "one"}) {
		t.Errorf("Lease() = %v, want the parked key last and the cursor moved past it", got)
	}
}

func TestCooldownExpires(t *testing.T) {
	now := time.Now()
	r := New([]string{"one", "two"}, time.Minute)
	r.now = func() time.Time { return now }

	r.Limit(0)
	if got := order(r); !equal(got, []string{"two", "one"}) {
		t.Fatalf("Lease() = %v, want the parked key demoted", got)
	}

	now = now.Add(2 * time.Minute)
	if got := order(r); !equal(got, []string{"two", "one"}) {
		// The cursor still starts at two, but one is healthy again rather than
		// merely tolerated, which is invisible from the order alone.
		t.Fatalf("Lease() = %v, want the rotation order kept", got)
	}
	r.Limit(1)
	if got := order(r); !equal(got, []string{"one", "two"}) {
		t.Errorf("Lease() = %v, want the healed key preferred over the freshly parked one", got)
	}
}

func TestLeaseFallsBackToParkedKeys(t *testing.T) {
	now := time.Now()
	r := New([]string{"one", "two"}, time.Minute)
	r.now = func() time.Time { return now }

	r.Limit(0)
	r.Limit(1)
	// Everything is cooling, but refusing to answer is worse than spending a
	// request finding out the quota healed early.
	if got := order(r); len(got) != 2 {
		t.Errorf("Lease() = %v, want both keys offered even while parked", got)
	}
}

func TestWorksClearsTheCooldownAndSticks(t *testing.T) {
	now := time.Now()
	r := New([]string{"one", "two", "three"}, time.Minute)
	r.now = func() time.Time { return now }

	r.Limit(0)
	r.Works(2)
	// three first because it just worked, then two because it is untouched, and
	// the parked one last.
	if got := order(r); !equal(got, []string{"three", "two", "one"}) {
		t.Errorf("Lease() = %v, want the working key first and the parked one last", got)
	}

	r.Works(0)
	if got := order(r); !equal(got, []string{"one", "two", "three"}) {
		t.Errorf("Lease() = %v, want the revived key first and no longer parked", got)
	}
}

func TestEmptyRing(t *testing.T) {
	r := New(nil, time.Minute)
	if r.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", r.Len())
	}
	if got := r.Lease(); len(got) != 0 {
		t.Errorf("Lease() = %v, want nothing to try", got)
	}
	// Out-of-range reports must not panic: they can only come from a lease this
	// ring never handed out.
	r.Limit(0)
	r.Works(-1)
}

func TestConcurrentUse(t *testing.T) {
	r := New([]string{"one", "two", "three"}, time.Minute)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, l := range r.Lease() {
				if i%2 == 0 {
					r.Limit(l.Index)
					continue
				}
				r.Works(l.Index)
			}
		}()
	}
	wg.Wait()
}

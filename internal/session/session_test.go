package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeSummariser returns a fixed summary and records how many times it was called.
type fakeSummariser struct {
	mu    sync.Mutex
	calls int
	text  string
	err   error
}

func (f *fakeSummariser) summarise(ctx context.Context, turns []Turn) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}

func (f *fakeSummariser) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestStorePersistAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	s1, err := New(Options{Dir: dir, Now: func() time.Time { return time.Now() }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	s1.Remember(42, "hello", "hi there")
	s1.Remember(42, "how are you", "fine")

	hist := s1.History(42)
	if len(hist) != 4 {
		t.Fatalf("expected 4 turns, got %d", len(hist))
	}

	// Reload from disk.
	s2, err := New(Options{Dir: dir, Now: func() time.Time { return time.Now() }})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	hist2 := s2.History(42)
	if len(hist2) != 4 {
		t.Fatalf("after reload expected 4 turns, got %d", len(hist2))
	}
	if hist2[0].Content != "hello" {
		t.Fatalf("first turn content = %q, want %q", hist2[0].Content, "hello")
	}
}

func TestStoreForget(t *testing.T) {
	dir := t.TempDir()

	s, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	s.Remember(1, "q1", "a1")
	s.Remember(1, "q2", "a2")
	if len(s.History(1)) != 4 {
		t.Fatalf("expected 4 turns before forget, got %d", len(s.History(1)))
	}

	s.Forget(1)
	if len(s.History(1)) != 0 {
		t.Fatalf("expected 0 turns after forget, got %d", len(s.History(1)))
	}

	// Forget persists across reloads.
	s2, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(s2.History(1)) != 0 {
		t.Fatalf("expected 0 turns after reload+forget, got %d", len(s2.History(1)))
	}

	// The user's file should be gone.
	if _, err := os.Stat(filepath.Join(dir, "1.json")); !os.IsNotExist(err) {
		t.Fatalf("user file should be deleted after forget")
	}
}

func TestStorePerUserFiles(t *testing.T) {
	dir := t.TempDir()

	s, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	s.Remember(100, "q1", "a1")
	s.Remember(200, "q2", "a2")

	// Each user should have their own file.
	if _, err := os.Stat(filepath.Join(dir, "100.json")); err != nil {
		t.Fatalf("expected 100.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "200.json")); err != nil {
		t.Fatalf("expected 200.json: %v", err)
	}

	// Context is separate per user.
	if len(s.History(100)) != 2 || len(s.History(200)) != 2 {
		t.Fatalf("expected 2 turns each, got %d and %d", len(s.History(100)), len(s.History(200)))
	}
}

func TestStoreCompression(t *testing.T) {
	dir := t.TempDir()

	fs := &fakeSummariser{text: "Summary of old conversation"}

	s, err := New(Options{
		Dir:        dir,
		Summariser: fs.summarise,
		MaxTurns:   6,
		KeepTurns:  2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Add 4 exchanges = 8 turns, exceeding MaxTurns=6.
	for i := 0; i < 4; i++ {
		s.Remember(99, "question-"+string(rune('a'+i)), "answer-"+string(rune('a'+i)))
	}

	hist := s.History(99)
	// Summary (1) + kept turns (2) = 3 total.
	if len(hist) != 3 {
		t.Fatalf("expected 3 turns after compression, got %d", len(hist))
	}
	if hist[0].Role != "system" || hist[0].Content != "Summary of old conversation" {
		t.Fatalf("first turn should be the summary, got role=%q content=%q", hist[0].Role, hist[0].Content)
	}
	if fs.count() != 1 {
		t.Fatalf("summariser called %d times, want 1", fs.count())
	}
	if hist[2].Content != "answer-d" {
		t.Fatalf("last turn = %q, want %q", hist[2].Content, "answer-d")
	}
}

func TestStoreNoSummariserTruncates(t *testing.T) {
	dir := t.TempDir()

	s, err := New(Options{
		Dir:       dir,
		MaxTurns:  6,
		KeepTurns: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 4; i++ {
		s.Remember(5, "q", "a") // each adds 2 turns → 8 total > 6
	}

	hist := s.History(5)
	if len(hist) != 2 {
		t.Fatalf("expected 2 turns after hard truncate, got %d", len(hist))
	}
}

func TestStoreEmptyRemember(t *testing.T) {
	dir := t.TempDir()

	s, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	s.Remember(1, "", "") // should be a no-op
	if len(s.History(1)) != 0 {
		t.Fatalf("expected 0 turns after empty remember, got %d", len(s.History(1)))
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	dir := t.TempDir()

	s, err := New(Options{Dir: dir, MaxTurns: 100, KeepTurns: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			userID := int64(n % 3)
			s.Remember(userID, "q", "a")
			_ = s.History(userID)
		}(i)
	}
	wg.Wait()
}

func TestStoreSummaryAccumulation(t *testing.T) {
	dir := t.TempDir()

	callCount := 0
	summariser := func(ctx context.Context, turns []Turn) (string, error) {
		callCount++
		return "accumulated summary v" + string(rune('0'+callCount)), nil
	}

	s, err := New(Options{
		Dir:        dir,
		Summariser: summariser,
		MaxTurns:   4,
		KeepTurns:  2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 3; i++ {
		s.Remember(7, "q"+string(rune('0'+i)), "a"+string(rune('0'+i)))
	}
	hist1 := s.History(7)
	if len(hist1) != 3 {
		t.Fatalf("after first compression: expected 3, got %d", len(hist1))
	}

	for i := 0; i < 2; i++ {
		s.Remember(7, "x"+string(rune('0'+i)), "y"+string(rune('0'+i)))
	}
	hist2 := s.History(7)
	if len(hist2) != 3 {
		t.Fatalf("after second compression: expected 3, got %d", len(hist2))
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 summariser calls, got %d", callCount)
	}
}

func TestGitSync(t *testing.T) {
	dir := t.TempDir()

	s, err := New(Options{Dir: dir, GitSync: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.git {
		t.Fatal("git sync should be enabled")
	}

	s.Remember(55, "hello", "world")

	// Check that the file exists and git has history.
	if _, err := os.Stat(filepath.Join(dir, "55.json")); err != nil {
		t.Fatalf("expected 55.json: %v", err)
	}
	// The .git directory should exist.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("expected .git directory: %v", err)
	}
}

// Ensure strconv import is used.
var _ = strconv.FormatInt

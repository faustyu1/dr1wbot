// Package session keeps persistent conversations for private chats.
//
// The plain memory package stores history in RAM and forgets it on restart,
// which is fine for guest mode where conversations are short and throwaway.
// A private chat is different: it is a real ongoing conversation that should
// survive a restart, and whose context should compress rather than truncate
// when it grows too long.
//
// This store persists each conversation to its own JSON file inside a directory
// (sessions/{userID}.json) and optionally auto-commits changes to a git
// repository so the context is versioned and can be backed up to a private
// remote like GitHub.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"dr1wbot/internal/memory"
)

// Turn is an alias so the package matches the Remembrancer interface without
// its own conversion layer.
type Turn = memory.Turn

// SummariserFunc compresses a slice of turns into a short summary string. It
// is satisfied by a closure around the LLM client, which keeps this package
// free of a direct dependency on llm.
type SummariserFunc func(ctx context.Context, turns []Turn) (string, error)

func (f SummariserFunc) Summarise(ctx context.Context, turns []Turn) (string, error) {
	return f(ctx, turns)
}

const (
	// DefaultMaxTurns triggers compression once a conversation reaches this many
	// turns. Twice the in-memory limit — a private conversation earns more room.
	DefaultMaxTurns = 16
	// DefaultKeepTurns is how many of the most recent turns survive
	// compression untouched. The rest become a summary.
	DefaultKeepTurns = 6
	// SummaryRunes caps the summary so it does not grow unbounded.
	SummaryRunes = 1500
	// MaxContentRunes trims a stored message the same way the memory package does.
	MaxContentRunes = 1500
)

type conversation struct {
	Turns   []Turn `json:"turns"`
	Summary string `json:"summary,omitempty"`
	Seen    string `json:"seen"`
}

// Store is a file-backed, compression-aware conversation store. It satisfies
// the reply.Rembrancer interface (History / Remember / Forget). Each user gets
// their own JSON file inside a directory, and changes can be auto-committed to
// a git repository for versioned backup.
type Store struct {
	mu         sync.Mutex
	dir        string // directory holding per-user JSON files
	convos     map[int64]*conversation
	summariser SummariserFunc
	maxTurns   int
	keepTurns  int
	git        bool // auto-commit to git after saves
	log        *slog.Logger
	now        func() time.Time
}

// Options configures a Store.
type Options struct {
	// Dir is the directory where per-user conversation files are stored.
	// Each user gets {Dir}/{userID}.json. Empty keeps changes in memory only.
	Dir string
	// GitSync enables automatic git add+commit+push after every save. The
	// directory must be a git repo (or one will be initialised on New).
	GitSync bool
	// Summariser compresses old turns. Nil disables compression.
	Summariser SummariserFunc
	// MaxTurns triggers compression when exceeded. Zero = DefaultMaxTurns.
	MaxTurns int
	// KeepTurns is how many recent turns survive compression. Zero = DefaultKeepTurns.
	KeepTurns int
	Logger    *slog.Logger
	Now       func() time.Time
}

// New builds a Store, loading existing conversations from disk.
func New(opts Options) (*Store, error) {
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = DefaultMaxTurns
	}
	if opts.KeepTurns <= 0 {
		opts.KeepTurns = DefaultKeepTurns
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	s := &Store{
		dir:        opts.Dir,
		convos:     make(map[int64]*conversation),
		summariser: opts.Summariser,
		maxTurns:   opts.MaxTurns,
		keepTurns:  opts.KeepTurns,
		log:        opts.Logger,
		now:        opts.Now,
	}

	if opts.Dir == "" {
		return s, nil
	}

	// Ensure the directory exists.
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create sessions dir %s: %w", opts.Dir, err)
	}

	// Load all per-user files.
	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("read sessions dir %s: %w", opts.Dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".json")]
		uid, err := strconv.ParseInt(name, 10, 64)
		if err != nil {
			continue // not a user-ID file
		}
		raw, err := os.ReadFile(filepath.Join(opts.Dir, e.Name()))
		if err != nil {
			continue
		}
		var c conversation
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		s.convos[uid] = &c
	}

	// Set up git if requested.
	if opts.GitSync {
		if err := initGit(opts.Dir); err != nil {
			opts.Logger.Warn("git sync disabled", "err", err)
		} else {
			s.git = true
			opts.Logger.Info("session git sync enabled", "dir", opts.Dir)
		}
	}

	return s, nil
}

// initGit initialises a git repo if needed, sets up a basic identity, and
// configures a remote for pushing to a private GitHub repository.
func initGit(dir string) error {
	alreadyRepo := false
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		alreadyRepo = true
	}

	if !alreadyRepo {
		for _, cmd := range [][]string{
			{"git", "init"},
			{"git", "config", "user.email", "bot@dr1wbot.local"},
			{"git", "config", "user.name", "dr1wbot"},
		} {
			c := exec.Command(cmd[0], cmd[1:]...)
			c.Dir = dir
			if out, err := c.CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w (%s)", cmd[0], err, string(out))
			}
		}
	}

	// Always ensure the remote is set (idempotent).
	c := exec.Command("git", "remote", "get-url", "origin")
	c.Dir = dir
	if c.Run() != nil {
		remoteURL := os.Getenv("SESSIONS_GIT_REMOTE")
		if remoteURL == "" {
			remoteURL = "https://github.com/n3r066/dr1wbot-sessions.git"
		}
		add := exec.Command("git", "remote", "add", "origin", remoteURL)
		add.Dir = dir
		_ = add.Run()
	}

	// Ensure default branch is main.
	b := exec.Command("git", "branch", "-M", "main")
	b.Dir = dir
	_ = b.Run()

	if !alreadyRepo {
		_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.tmp\n*.bak\n"), 0o644)
	}
	return nil
}

// gitCommit stages, commits, and pushes changes to the remote.
func (s *Store) gitCommit(reason string) {
	if !s.git {
		return
	}
	msg := fmt.Sprintf("auto: %s @ %s", reason, s.now().Format(time.RFC3339))

	// Stage and commit.
	for _, cmd := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", msg},
	} {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = s.dir
		_ = c.Run() // best-effort; nothing may have changed
	}

	// Push to remote (best-effort; network may be unavailable). Force-push is
	// safe: this is a single-writer backup repo, so our local history is the
	// source of truth and any divergence on the remote is stale.
	push := exec.Command("git", "push", "-fu", "origin", "main")
	push.Dir = s.dir
	if out, err := push.CombinedOutput(); err != nil {
		s.log.Debug("git push failed", "err", err, "output", string(out))
	}
}

// History returns the remembered turns for one user, oldest first. The summary,
// if one exists, is prepended as a system message so the model knows the
// earlier context without seeing every word of it.
func (s *Store) History(userID int64) []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.convos[userID]
	if !ok || len(c.Turns) == 0 {
		return nil
	}

	var turns []Turn
	if c.Summary != "" {
		turns = append(turns, Turn{Role: "system", Content: c.Summary})
	}
	turns = append(turns, c.Turns...)
	return turns
}

// Remember appends one exchange and may compress if the conversation exceeds
// the turn limit. Compression runs synchronously because Remember is called
// after the answer is already sent — the user is not waiting on it. The lock
// is released during the LLM call so other users are not blocked.
func (s *Store) Remember(userID int64, question, answer string) {
	if question == "" && answer == "" {
		return
	}

	s.mu.Lock()

	c, ok := s.convos[userID]
	if !ok {
		c = &conversation{}
		s.convos[userID] = c
	}

	if question != "" {
		c.Turns = append(c.Turns, Turn{Role: "user", Content: trim(question)})
	}
	if answer != "" {
		c.Turns = append(c.Turns, Turn{Role: "assistant", Content: trim(answer)})
	}
	c.Seen = s.now().Format(time.RFC3339)

	if len(c.Turns) > s.maxTurns {
		splitAt := len(c.Turns) - s.keepTurns
		if splitAt < 0 {
			splitAt = 0
		}
		oldTurns := append([]Turn(nil), c.Turns[:splitAt]...)
		recent := append([]Turn(nil), c.Turns[splitAt:]...)

		if s.summariser != nil {
			var input []Turn
			if c.Summary != "" {
				input = append(input, Turn{Role: "system", Content: c.Summary})
			}
			input = append(input, oldTurns...)

			s.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			summary, err := s.summariser(ctx, input)
			cancel()

			s.mu.Lock()
			c, ok = s.convos[userID]
			if !ok {
				s.mu.Unlock()
				return
			}
			if err != nil {
				s.log.Warn("session compression failed, truncating", "user_id", userID, "err", err)
				c.Summary = ""
				c.Turns = recent
			} else {
				c.Summary = trimSummary(summary)
				c.Turns = recent
			}
		} else {
			c.Summary = ""
			c.Turns = recent
		}
	}

	if err := s.saveLocked(userID); err != nil {
		s.log.Warn("could not save session", "user_id", userID, "err", err)
	}
	s.mu.Unlock()
}

// Forget drops one user's history, starting a fresh conversation.
func (s *Store) Forget(userID int64) {
	s.mu.Lock()
	delete(s.convos, userID)
	if s.dir != "" {
		_ = os.Remove(s.userFile(userID))
	}
	if s.git {
		s.gitCommit("forget user " + strconv.FormatInt(userID, 10))
	}
	s.mu.Unlock()
}

// ArchiveEntry is one saved conversation shown in the session list.
type ArchiveEntry struct {
	Hash    string // unique id used in the restore deep link
	Created string // RFC3339 timestamp of the archive
	Preview string // first user message, truncated for display
	Turns   int    // number of turns stored
}

// archive is the on-disk format for an archived conversation.
type archive struct {
	UserID  int64        `json:"user_id"`
	Hash    string       `json:"hash"`
	Created string       `json:"created"`
	Preview string       `json:"preview"`
	Convo   conversation `json:"convo"`
}

// archiveDir returns the subdirectory holding per-user archived sessions.
func (s *Store) archiveDir(userID int64) string {
	return filepath.Join(s.dir, "archive", strconv.FormatInt(userID, 10))
}

// Archive saves the current conversation under a unique hash and clears the
// active one, so the user starts fresh but can return to the old thread via
// a restore deep link. Returns the hash or "" if there was nothing to save.
func (s *Store) Archive(userID int64) (string, error) {
	if s.dir == "" {
		return "", nil
	}

	s.mu.Lock()
	c, ok := s.convos[userID]
	if !ok || len(c.Turns) == 0 {
		s.mu.Unlock()
		return "", nil // nothing to archive
	}

	hash := randomHash()
	preview := ""
	for _, t := range c.Turns {
		if t.Role == "user" {
			preview = t.Content
			break
		}
	}
	if preview == "" && len(c.Turns) > 0 {
		preview = c.Turns[0].Content
	}
	preview = truncate(preview, 120)

	ar := archive{
		UserID:  userID,
		Hash:    hash,
		Created: s.now().Format(time.RFC3339),
		Preview: preview,
		Convo:   *c,
	}

	dir := s.archiveDir(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("create archive dir: %w", err)
	}

	data, err := json.MarshalIndent(ar, "", "  ")
	if err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("encode archive: %w", err)
	}

	path := filepath.Join(dir, hash+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("write archive: %w", err)
	}

	// Clear the active conversation so the next message starts fresh.
	delete(s.convos, userID)
	_ = os.Remove(s.userFile(userID))

	if s.git {
		s.gitCommit("archive user " + strconv.FormatInt(userID, 10))
	}
	s.mu.Unlock()
	return hash, nil
}

// Restore loads an archived conversation back as the active one for a user.
// Returns true if the archive was found and restored.
func (s *Store) Restore(userID int64, hash string) error {
	if s.dir == "" || hash == "" {
		return fmt.Errorf("archive not available")
	}

	path := filepath.Join(s.archiveDir(userID), hash+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}

	var ar archive
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("decode archive: %w", err)
	}

	s.mu.Lock()
	s.convos[userID] = &ar.Convo
	if err := s.saveLocked(userID); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.git {
		s.gitCommit("restore user " + strconv.FormatInt(userID, 10))
	}
	s.mu.Unlock()
	return nil
}

// Archives lists a user's saved conversations, newest first.
func (s *Store) Archives(userID int64) []ArchiveEntry {
	if s.dir == "" {
		return nil
	}

	dir := s.archiveDir(userID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []ArchiveEntry
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var ar archive
		if json.Unmarshal(raw, &ar) != nil {
			continue
		}
		out = append(out, ArchiveEntry{
			Hash:    ar.Hash,
			Created: ar.Created,
			Preview: ar.Preview,
			Turns:   len(ar.Convo.Turns),
		})
	}

	// Sort newest first by filename (hash is random, but Created is in the
	// JSON; sort by that).
	slicesFunc(out)
	return out
}

// PurgeArchives deletes all archived conversations for one user.
func (s *Store) PurgeArchives(userID int64) error {
	if s.dir == "" {
		return nil
	}
	dir := s.archiveDir(userID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("purge archives: %w", err)
	}
	if s.git {
		s.gitCommit("purge archives user " + strconv.FormatInt(userID, 10))
	}
	return nil
}

// HasActive reports whether a user has an ongoing conversation with turns.
func (s *Store) HasActive(userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convos[userID]
	return ok && len(c.Turns) > 0
}

// randomHash returns a 32-char hex id suitable for a deep link.
func randomHash() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// slicesFunc sorts archive entries newest-first by timestamp.
func slicesFunc(entries []ArchiveEntry) {
	// Simple insertion sort — the list is small (a handful of saved chats).
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j-1].Created < entries[j].Created; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// userFile returns the path for one user's conversation file.
func (s *Store) userFile(userID int64) string {
	return filepath.Join(s.dir, strconv.FormatInt(userID, 10)+".json")
}

// saveLocked writes one user's conversation to disk atomically.
func (s *Store) saveLocked(userID int64) error {
	if s.dir == "" {
		return nil
	}

	c, ok := s.convos[userID]
	if !ok {
		return nil
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, ".session-*.json")
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp session: %w", err)
	}
	dst := s.userFile(userID)
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("replace session %s: %w", dst, err)
	}

	if s.git {
		s.gitCommit("update user " + strconv.FormatInt(userID, 10))
	}
	return nil
}

func trim(s string) string {
	r := []rune(s)
	if len(r) <= MaxContentRunes {
		return s
	}
	return string(r[:MaxContentRunes]) + "\u2026"
}

func trimSummary(s string) string {
	r := []rune(s)
	if len(r) <= SummaryRunes {
		return s
	}
	return string(r[:SummaryRunes]) + "\u2026"
}

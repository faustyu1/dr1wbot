// Package alert pushes operational news to the admins' private chats.
//
// Without it the only way to learn that the keys ran dry or that somebody is
// being banned is to open the panel and look, which nobody does at the moment
// it starts mattering.
//
// Everything here is deliberately quiet. An alert that fires on every incident
// becomes noise within an hour of a real problem — and the incidents worth
// knowing about are exactly the ones that repeat. So each kind of alert has a
// cooldown, and a burst of the same news is delivered once.
package alert

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mymmrac/telego"
)

// Sender is the slice of the Telegram API this package needs.
type Sender interface {
	SendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error)
}

// Notifier sends alerts to every admin.
type Notifier struct {
	sender Sender
	admins []int64
	log    *slog.Logger

	cooldown time.Duration
	now      func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

// Options configures a Notifier.
type Options struct {
	Sender Sender
	// Admins receive every alert. An empty list disables alerting.
	Admins []int64
	Logger *slog.Logger
	// Cooldown is the minimum gap between two alerts of the same kind.
	Cooldown time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// New builds a Notifier.
func New(opts Options) *Notifier {
	if opts.Cooldown <= 0 {
		opts.Cooldown = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Notifier{
		sender:   opts.Sender,
		admins:   opts.Admins,
		log:      opts.Logger,
		cooldown: opts.Cooldown,
		now:      opts.Now,
		last:     make(map[string]time.Time),
	}
}

// Kinds of alert. The kind is what the cooldown is keyed on, so a hundred bans
// in a minute produce one message.
const (
	KindBan       = "ban"
	KindNoKeys    = "no-keys"
	KindExhausted = "global-exhausted"
	KindKeyError  = "key-error"
)

// Notify sends text to every admin unless this kind of alert fired recently.
// It never blocks the caller for long and never returns an error: an alert that
// failed to send must not fail the thing it was reporting on.
func (n *Notifier) Notify(ctx context.Context, kind, text string) {
	if n == nil || len(n.admins) == 0 || n.sender == nil {
		return
	}
	if !n.due(kind) {
		return
	}

	// Detached from the caller's context: the request that triggered this is
	// usually about to end, and the alert should outlive it.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	go func() {
		defer cancel()
		for _, id := range n.admins {
			_, err := n.sender.SendMessage(sendCtx, &telego.SendMessageParams{
				ChatID:             telego.ChatID{ID: id},
				Text:               text,
				ParseMode:          telego.ModeHTML,
				LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
			})
			if err != nil {
				// An admin who never started a chat with the bot cannot be
				// messaged at all; that is normal, not worth an error level.
				n.log.Debug("could not deliver an alert", "admin", id, "err", err)
			}
		}
	}()
}

// sendTimeout bounds the whole delivery round.
const sendTimeout = 20 * time.Second

// due reports whether this kind may fire now, and records that it did.
func (n *Notifier) due(kind string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.now()
	if last, ok := n.last[kind]; ok && now.Sub(last) < n.cooldown {
		return false
	}
	n.last[kind] = now
	return true
}

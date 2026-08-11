package alert

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
)

// fakeSender records deliveries.
type fakeSender struct {
	mu   sync.Mutex
	sent []int64
	err  error
}

func (f *fakeSender) SendMessage(_ context.Context, p *telego.SendMessageParams) (*telego.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p.ChatID.ID)
	if f.err != nil {
		return nil, f.err
	}
	return &telego.Message{}, nil
}

func (f *fakeSender) delivered() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.sent...)
}

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

// settle waits for the background delivery to land.
func settle(t *testing.T, f *fakeSender, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if len(f.delivered()) >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("delivered %d alerts, want %d", len(f.delivered()), want)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func newNotifier(sender Sender, admins []int64, c *clock) *Notifier {
	return New(Options{
		Sender:   sender,
		Admins:   admins,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cooldown: 15 * time.Minute,
		Now:      c.now,
	})
}

func TestEveryAdminIsTold(t *testing.T) {
	sender := &fakeSender{}
	n := newNotifier(sender, []int64{1, 2}, &clock{at: time.Now()})

	n.Notify(context.Background(), KindBan, "бан")
	settle(t, sender, 2)
}

func TestTheSameKindIsThrottled(t *testing.T) {
	sender := &fakeSender{}
	c := &clock{at: time.Now()}
	n := newNotifier(sender, []int64{1}, c)

	n.Notify(context.Background(), KindBan, "первый")
	settle(t, sender, 1)

	// A hundred bans in a minute must produce one message, or the alert that
	// matters is buried by the alerts about it.
	for range 10 {
		n.Notify(context.Background(), KindBan, "ещё")
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(sender.delivered()); got != 1 {
		t.Errorf("delivered %d, want the burst collapsed into one", got)
	}
}

func TestDifferentKindsDoNotThrottleEachOther(t *testing.T) {
	sender := &fakeSender{}
	n := newNotifier(sender, []int64{1}, &clock{at: time.Now()})

	n.Notify(context.Background(), KindBan, "бан")
	n.Notify(context.Background(), KindNoKeys, "ключи")
	settle(t, sender, 2)
}

func TestCooldownExpires(t *testing.T) {
	sender := &fakeSender{}
	c := &clock{at: time.Now()}
	n := newNotifier(sender, []int64{1}, c)

	n.Notify(context.Background(), KindBan, "первый")
	settle(t, sender, 1)

	c.at = c.at.Add(20 * time.Minute)
	n.Notify(context.Background(), KindBan, "второй")
	settle(t, sender, 2)
}

func TestNoAdminsIsNotAnError(t *testing.T) {
	sender := &fakeSender{}
	n := newNotifier(sender, nil, &clock{at: time.Now()})

	n.Notify(context.Background(), KindBan, "бан")
	time.Sleep(20 * time.Millisecond)
	if got := len(sender.delivered()); got != 0 {
		t.Errorf("delivered %d with no admins configured", got)
	}
}

func TestDeliveryOutlivesTheCallersContext(t *testing.T) {
	sender := &fakeSender{}
	n := newNotifier(sender, []int64{1}, &clock{at: time.Now()})

	ctx, cancel := context.WithCancel(context.Background())
	n.Notify(ctx, KindBan, "бан")
	// The request that triggered the alert is usually about to end; the alert
	// must not end with it.
	cancel()
	settle(t, sender, 1)
}

func TestAFailedDeliveryIsSurvivable(t *testing.T) {
	sender := &fakeSender{err: errors.New("bot was blocked by the user")}
	n := newNotifier(sender, []int64{1, 2}, &clock{at: time.Now()})

	// An admin who never started a chat with the bot cannot be messaged; the
	// others still must be.
	n.Notify(context.Background(), KindBan, "бан")
	settle(t, sender, 2)
}

func TestNilNotifierIsInert(t *testing.T) {
	var n *Notifier
	n.Notify(context.Background(), KindBan, "бан") // must not panic
}

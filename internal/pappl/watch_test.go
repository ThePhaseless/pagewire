package pappl_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThePhaseless/pagewire/internal/ippnotify"
	"github.com/ThePhaseless/pagewire/internal/pappl"
	"github.com/ThePhaseless/pagewire/internal/pappl/papplfake"
)

// watch runs c.Watch until the returned cancel, sending every changed call on
// the channel and the result on done.
func watch(t *testing.T, c *pappl.Client) (changed <-chan struct{}, done <-chan error, cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch, errc := make(chan struct{}, 16), make(chan error, 1)
	go func() {
		errc <- c.Watch(ctx, func() {
			select {
			case ch <- struct{}{}:
			default:
			}
		})
	}()
	t.Cleanup(cancel)
	return ch, errc, cancel
}

func waitChanged(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("no changed call: %s", what)
	}
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return")
		return nil
	}
}

// A queue deleted in PAPPL's web UI reaches the agent through the system
// subscription, while the Get-Notifications is held, with no clock involved.
func TestWatchQueueDeletedExternally(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
	ch, _, _ := watch(t, c)
	waitChanged(t, ch, "after subscribing")
	deadline := time.Now().Add(5 * time.Second)
	for fake.GetNotificationsCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	fake.DeleteExternally("P")
	waitChanged(t, ch, "after DeleteExternally")
}

func TestWatchUnsupported(t *testing.T) {
	_, c := newSystem(t, papplfake.Options{NoSubscriptions: true})
	_, done, _ := watch(t, c)
	if err := waitDone(t, done); !errors.Is(err, ippnotify.ErrUnsupported) {
		t.Errorf("Watch = %v, want ErrUnsupported", err)
	}
}

func TestWatchRefusedAtCap(t *testing.T) {
	_, c := newSystem(t, papplfake.Options{MaxSubscriptions: 1})
	ch, _, _ := watch(t, c)
	waitChanged(t, ch, "first watch subscribed")
	_, done, _ := watch(t, c)
	if err := waitDone(t, done); !errors.Is(err, ippnotify.ErrRefused) {
		t.Errorf("second Watch = %v, want ErrRefused", err)
	}
}

func TestWatchCancelUnsubscribes(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{})
	ch, done, cancel := watch(t, c)
	waitChanged(t, ch, "after subscribing")
	if n := fake.Subscriptions(); n != 1 {
		t.Fatalf("subscriptions = %d, want 1", n)
	}
	cancel()
	if err := waitDone(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("Watch = %v, want context.Canceled", err)
	}
	if n := fake.Subscriptions(); n != 0 {
		t.Errorf("subscriptions after cancel = %d, want 0", n)
	}
}

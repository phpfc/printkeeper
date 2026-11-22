package main

import (
	"testing"
	"time"
)

// TestNetworkChangeWatcherBlocksUntilClosed pins the property this
// watcher exists for: Wait must genuinely block in the kernel — not
// busy-loop, not return immediately, not poll — until either a real
// routing message arrives or the watcher is closed. That is what makes
// it zero scheduled wakeups in the steady state, the whole point of
// replacing a timer poll with this.
//
// A real routing-table change (an interface going up or down, an
// address changing) is not something this test can deterministically
// trigger without touching the machine's live network configuration,
// which is out of bounds for a unit test — see this task's report for
// a live, manual observation of a real message arriving instead. What
// is deterministic, and is what this test pins: Wait does not return
// on its own within a window generous enough that a busy-loop or a
// short internal poll would already have returned many times over,
// and Close reliably unblocks a Wait in progress.
func TestNetworkChangeWatcherBlocksUntilClosed(t *testing.T) {
	w, err := newNetworkChangeWatcher()
	if err != nil {
		t.Fatalf("newNetworkChangeWatcher() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- w.Wait() }()

	select {
	case err := <-done:
		t.Fatalf("Wait() returned (err=%v) with no routing change and no Close — it must block", err)
	case <-time.After(300 * time.Millisecond):
		// Still blocked, as expected: no CPU spin, no early return.
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait() returned nil after Close(); want a non-nil error — the socket was closed, not a real routing message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not unblock within 2s of Close()")
	}
}

// TestNetworkChangeWatcherCloseIsIdempotent is the regression for a
// reviewer's finding: runProbeLoop closes the watcher from two places
// on every clean shutdown — once when the watcher's own Wait loop
// exits after Close unblocked it, and once explicitly from the
// goroutine that reacted to ctx.Done() in the first place. That is a
// double-close of the same fd today: harmless only because nothing
// else happens to reuse the fd number in the brief window between the
// two calls, which is exactly the kind of footgun that stops being
// harmless the moment something does. Close must tolerate being
// called more than once.
func TestNetworkChangeWatcherCloseIsIdempotent(t *testing.T) {
	w, err := newNetworkChangeWatcher()
	if err != nil {
		t.Fatalf("newNetworkChangeWatcher() error = %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil (idempotent)", err)
	}
}

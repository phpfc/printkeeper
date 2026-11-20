package main

import (
	"testing"
	"time"
)

// TestProbeContextGivesEachCallAFullIndependentBudget pins a fix for a bug
// found in code review, not by a test that existed at the time: every
// candidate's probe context used to be derived from one context shared
// across every candidate in the inspect loop, sized for exactly one browse
// plus one probe. When discovery found more than one printer — the
// documented primary use case — a slow or unresponsive first printer could
// starve the probe window of every printer discovered after it.
//
// probeContext takes its timeout as an explicit parameter and derives
// every call from context.Background() with no parent context at all.
// That lets this test exercise the real, unmodified production function
// at a small scale (milliseconds), so a sleep that is a large fraction of
// the test's own timeout makes any shared-deadline starvation unmistakable
// — rather than a sleep that is only a negligible fraction of the real
// multi-second production probeTimeout, which would let the exact same bug
// pass silently (see the fix report for how that happened once already).
func TestProbeContextGivesEachCallAFullIndependentBudget(t *testing.T) {
	const (
		timeout   = 100 * time.Millisecond
		elapsed   = 80 * time.Millisecond // most of timeout: unmistakable if starved
		tolerance = 30 * time.Millisecond // generous for scheduler jitter
	)

	assertFreshBudget := func(t *testing.T) {
		t.Helper()

		ctx, cancel := probeContext(timeout)
		defer cancel()

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("probeContext() returned a context with no deadline")
		}

		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > timeout {
			t.Fatalf("probeContext(%s) remaining = %s, want in (0, %s]", timeout, remaining, timeout)
		}
		// A shared or already-partially-consumed parent deadline would
		// show up here as meaningfully less than the full timeout.
		if remaining < timeout-tolerance {
			t.Fatalf("probeContext(%s) remaining = %s, want within %s of the full timeout", timeout, remaining, tolerance)
		}
	}

	// First candidate's probe.
	assertFreshBudget(t)

	// Stand in for an earlier, slow or unresponsive candidate consuming
	// most of its own probe window before the next candidate is probed.
	time.Sleep(elapsed)

	// Second candidate's probe must still get a full, independent budget
	// — not one shrunk by the time that passed probing the first.
	assertFreshBudget(t)
}

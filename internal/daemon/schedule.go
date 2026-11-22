// Package daemon implements printkeeper's own judgement about *when* to
// ask a printer whether it is there — the scheduling policy spec §6.6
// describes: event-driven first (on network change, on wake from
// sleep, on daemon start), with a slow heartbeat covering silent
// failures while the network looks stable, and nothing beyond that
// heartbeat while the machine is on battery and idle. A daemon with a
// visible footprint — measurable battery drain, constant wakeups — is a
// failed daemon (spec's own framing), so this policy is deliberately
// conservative about how often it asks, not just about what it asks.
package daemon

import (
	"time"

	"github.com/phpfc/printkeeper/internal/probe"
)

// Heartbeat is the slow, steady probe interval that catches a printer
// going silently absent when nothing else — no network change, no wake
// from sleep — gave the daemon a reason to look sooner. Spec §6.6 asks
// for "order of 15 minutes"; exact tuning is the builder's call to make
// against measured wakeup cost, and 15 minutes is it.
const Heartbeat = 15 * time.Minute

// initialRetry is the shortest interval NextProbeAt ever returns for a
// printer it just found absent: short enough to notice it coming back
// quickly (someone switching it back on), without polling so fast that
// an outage lasting hours turns into thousands of wasted round trips.
const initialRetry = 30 * time.Second

// NextProbeAt decides how long to wait before probing one adopted
// printer again.
//
// last is the most recent time this printer was confirmed present on
// the current network (e.g. the matching store.Seen.At) — not the time
// of the probe that just ran. It only affects the StateAbsent branch's
// backoff; every other branch ignores it.
//
// lastResult is what that most recent probe found. A printer that
// answered — healthy (StatePresent) or not (StateErrored) — gave no
// evidence it moved or disappeared, so both get the slow Heartbeat: there
// is nothing to chase, only silent failure to keep covering. A printer
// that did not answer (StateAbsent) gets a shorter retry that grows the
// longer it has stayed silent (see backoff), capped at Heartbeat, so a
// printer missing thirty seconds and one missing three weeks are not
// probed at the same rate. A probe that could not even be attempted
// (StateUnknown — this machine itself has no usable network) schedules
// nothing at all: there is no printer-shaped question to ask yet, and
// it is the caller's own network-change watcher, not a timer, that must
// wake probing back up.
//
// onBattery silences the backoff: spec §6.6 rules out probing beyond the
// heartbeat while the machine is on battery and idle, so an absent
// printer on battery still only gets Heartbeat, never the faster retry a
// plugged-in machine would get. Idle detection itself is the caller's
// concern (see cmd/printkeeperd) — this function only ever sees the
// single bit it needs to act on.
func NextProbeAt(last time.Time, lastResult probe.State, onBattery bool) time.Duration {
	switch lastResult {
	case probe.StateUnknown:
		return 0
	case probe.StateAbsent:
		if onBattery {
			return Heartbeat
		}
		return backoff(time.Since(last))
	default: // StatePresent, StateErrored, and any future addition.
		return Heartbeat
	}
}

// backoff returns the retry interval for a printer that has been absent
// for elapsed time since it was last confirmed present. It doubles from
// initialRetry each time elapsed outgrows the previous step, capped at
// Heartbeat.
func backoff(elapsed time.Duration) time.Duration {
	interval := initialRetry
	for interval <= elapsed && interval < Heartbeat {
		interval *= 2
	}
	if interval > Heartbeat {
		interval = Heartbeat
	}
	return interval
}

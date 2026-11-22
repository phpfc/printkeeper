package daemon

import (
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/probe"
)

// TestNextProbeAtPresentAndErroredScheduleHeartbeat covers the first
// half of spec §6.6: a printer that answered — healthy or not — gave no
// reason to look again soon, so it only ever gets the slow heartbeat.
func TestNextProbeAtPresentAndErroredScheduleHeartbeat(t *testing.T) {
	now := time.Now()
	for _, state := range []probe.State{probe.StatePresent, probe.StateErrored} {
		got := NextProbeAt(now.Add(-time.Hour), state, false)
		if got != Heartbeat {
			t.Errorf("NextProbeAt(..., %s, false) = %s, want the heartbeat %s", state, got, Heartbeat)
		}
	}
}

// TestNextProbeAtUnknownSchedulesNoProbeAtAll pins the brief's StateUnknown
// case: the machine itself could not ask, so nothing is scheduled —
// recovery is the network-change watcher's job, not a timer's.
func TestNextProbeAtUnknownSchedulesNoProbeAtAll(t *testing.T) {
	got := NextProbeAt(time.Now().Add(-24*time.Hour), probe.StateUnknown, false)
	if got != 0 {
		t.Fatalf("NextProbeAt(..., StateUnknown, false) = %s, want 0 (no probe scheduled)", got)
	}
}

// TestNextProbeAtAbsentRetriesSoonerThanHeartbeat proves the absent case
// actually gets a *different*, shorter number than the heartbeat — not
// just "something less than or equal to it", which a buggy
// implementation that always returns Heartbeat would also satisfy.
func TestNextProbeAtAbsentRetriesSoonerThanHeartbeat(t *testing.T) {
	recentlyWentAbsent := time.Now()
	got := NextProbeAt(recentlyWentAbsent, probe.StateAbsent, false)
	if got <= 0 || got >= Heartbeat {
		t.Fatalf("NextProbeAt(just now, StateAbsent, false) = %s, want strictly between 0 and the heartbeat %s", got, Heartbeat)
	}
}

// TestNextProbeAtAbsentBackoffGrowsButCapsAtHeartbeat proves the retry
// interval actually grows with how long the printer has been missing
// (real backoff, not a flat constant), while never exceeding the
// heartbeat.
func TestNextProbeAtAbsentBackoffGrowsButCapsAtHeartbeat(t *testing.T) {
	now := time.Now()

	soon := NextProbeAt(now, probe.StateAbsent, false)
	later := NextProbeAt(now.Add(-5*time.Minute), probe.StateAbsent, false)
	muchLater := NextProbeAt(now.Add(-24*time.Hour), probe.StateAbsent, false)

	if !(soon < later) {
		t.Fatalf("backoff did not grow: NextProbeAt at 0 elapsed = %s, at 5m elapsed = %s", soon, later)
	}
	if muchLater != Heartbeat {
		t.Fatalf("NextProbeAt(absent for 24h) = %s, want capped at the heartbeat %s", muchLater, Heartbeat)
	}
}

// TestNextProbeAtOnBatteryDisablesBackoff is the case the brief calls out
// by name: "on battery and idle, only the heartbeat applies". The trap
// here is asserting the on-battery result merely *equals* Heartbeat,
// which a buggy implementation that ignores onBattery entirely for the
// StatePresent/StateErrored branches would also produce by coincidence.
// This instead proves onBattery actually changes the absent branch's
// behaviour, by comparing it directly against the otherwise-identical
// off-battery call, which this same test file already proved returns
// something shorter.
func TestNextProbeAtOnBatteryDisablesBackoff(t *testing.T) {
	recentlyWentAbsent := time.Now()

	onBattery := NextProbeAt(recentlyWentAbsent, probe.StateAbsent, true)
	offBattery := NextProbeAt(recentlyWentAbsent, probe.StateAbsent, false)

	if onBattery != Heartbeat {
		t.Fatalf("NextProbeAt(recently absent, onBattery=true) = %s, want the heartbeat %s", onBattery, Heartbeat)
	}
	if onBattery == offBattery {
		t.Fatalf("onBattery made no difference: both calls returned %s; expected the off-battery call to retry sooner", onBattery)
	}
}

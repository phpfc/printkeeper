//go:build integration && darwin

package probe

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/fakeprinter"
)

// TestProbeAgainstAlivePrinterReturnsPresent probes a real, healthy
// ippeveprinter instance and expects StatePresent with a real attribute
// set and a positive round-trip time.
func TestProbeAgainstAlivePrinterReturnsPresent(t *testing.T) {
	port, cleanup := fakeprinter.Start(t, "ProbeAlivePrinter")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uri := fmt.Sprintf("ipp://127.0.0.1:%d/ipp/print", port)
	r := Probe(ctx, uri, 5*time.Second)

	if r.State != StatePresent {
		t.Fatalf("Probe against live printer returned %v (err=%v); want StatePresent", r.State, r.Err)
	}
	if _, ok := r.Attributes["printer-uuid"]; !ok {
		t.Error("result is missing printer-uuid")
	}
	if r.RTT <= 0 {
		t.Error("expected a positive round-trip time")
	}
}

// TestProbeAgainstKilledPrinterReturnsAbsent kills ippeveprinter and then
// probes its now-vacated port, on a machine that does have a network.
// Review Focus 5's counterpart: with the network up, nothing answering
// means StateAbsent.
func TestProbeAgainstKilledPrinterReturnsAbsent(t *testing.T) {
	port, cleanup := fakeprinter.Start(t, "ProbeKilledPrinter")
	cleanup() // kill it immediately; the port is now unoccupied.

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uri := fmt.Sprintf("ipp://127.0.0.1:%d/ipp/print", port)
	r := Probe(ctx, uri, 2*time.Second)

	if r.State != StateAbsent {
		t.Fatalf("Probe against killed printer returned %v (err=%v); want StateAbsent", r.State, r.Err)
	}
}

// TestProbeOfStoppedPrinterReturnsErroredNotAbsent pins Review Focus 2
// against a real device's actual attribute shape.
//
// ippeveprinter has no supported way to simulate a jam or a stopped
// device: its web UI lets you drive media and supply levels to zero, but
// that only ever escalates printer-state-reasons to the "-report"
// severity (e.g. "media-empty-report") and never flips printer-state to
// stopped — confirmed by hand against a live instance via its /media and
// /supplies forms. So, as the brief's documented fallback, this test
// captures a real response from a live ippeveprinter and overrides just
// the two attributes a genuinely stopped device would differ on, then
// feeds that map into the same classify function Probe uses internally.
func TestProbeOfStoppedPrinterReturnsErroredNotAbsent(t *testing.T) {
	port, cleanup := fakeprinter.Start(t, "ProbeStoppedPrinter")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uri := fmt.Sprintf("ipp://127.0.0.1:%d/ipp/print", port)
	live := Probe(ctx, uri, 5*time.Second)
	if live.Err != nil {
		t.Fatalf("Probe against live printer failed: %v", live.Err)
	}

	stopped := make(map[string][]string, len(live.Attributes))
	for k, v := range live.Attributes {
		stopped[k] = v
	}
	stopped["printer-state"] = []string{"5"} // IPP printer-state enum: 5 = stopped
	stopped["printer-state-reasons"] = []string{"media-jam-error"}

	if got := classify(stopped); got != StateErrored {
		t.Fatalf("stopped printer classified %v; want StateErrored (the printer is present, just unhealthy)", got)
	}
}

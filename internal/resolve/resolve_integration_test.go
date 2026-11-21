//go:build integration && darwin

package resolve

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/fakeprinter"
	"github.com/phpfc/printkeeper/internal/store"
)

// TestResolveRecoversViaBonjourBrowseAfterLastKnownGoodFails drives the
// full ladder against real ippeveprinter instances: start one on port
// A, record it as last-known-good, kill it, start a fresh one on a
// different port B, and confirm Resolve falls past lastKnownGood (which
// correctly finds nothing at the now-dead port A) and recovers the
// printer via bonjourBrowse — with the Attempt list showing
// lastKnownGood as tried and failed, not skipped.
func TestResolveRecoversViaBonjourBrowseAfterLastKnownGoodFails(t *testing.T) {
	const name = "ResolveChainIntegrationTest"

	portA, _, cleanupA := fakeprinter.Start(t, name)
	uriA := fmt.Sprintf("ipp://127.0.0.1:%d/ipp/print", portA)

	adopted := store.Adopted{
		LastSeen: map[string]store.Seen{
			"test-net": {URI: uriA, At: time.Now()},
		},
	}

	cleanupA() // kill it; port A is now unoccupied, so lastKnownGood must fail.

	portB, _, cleanupB := fakeprinter.Start(t, name)
	defer cleanupB()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	strategies := DefaultStrategies(discovery.NewDarwinDiscoverer())
	candidates, attempts := Resolve(ctx, strategies, adopted, "test-net")

	if len(attempts) == 0 || attempts[0].Strategy != "lastKnownGood" {
		t.Fatalf("Resolve() attempts = %+v, want lastKnownGood tried first", attempts)
	}
	if attempts[0].Candidates != 0 {
		t.Fatalf("lastKnownGood attempt reported %d candidates against a killed printer; want 0", attempts[0].Candidates)
	}
	if errors.Is(attempts[0].Err, ErrNotAttempted) {
		t.Fatalf("lastKnownGood attempt reported ErrNotAttempted; it ran and failed, it was not skipped")
	}

	last := attempts[len(attempts)-1]
	if last.Strategy != "bonjourBrowse" {
		t.Fatalf("Resolve() attempts = %+v, want the winning strategy to be bonjourBrowse", attempts)
	}
	if last.Candidates == 0 {
		t.Fatalf("bonjourBrowse attempt reported 0 candidates; want it to have recovered the restarted printer")
	}

	found := false
	for _, c := range candidates {
		if c.Port == portB {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Resolve() candidates = %+v, want one at the restarted printer's port %d", candidates, portB)
	}
}

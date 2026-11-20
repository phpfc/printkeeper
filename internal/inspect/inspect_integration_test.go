//go:build integration && darwin

package inspect

import (
	"context"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/fakeprinter"
)

func TestBrowseAndInspectAgainstFakePrinter(t *testing.T) {
	port, _, cleanup := fakeprinter.Start(t, "SpikePrinter")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d := discovery.NewDarwinDiscoverer()

	var candidate discovery.Candidate
	found := false
	// Bonjour advertisement can take a beat to propagate; retry the browse
	// a few times rather than relying on a single fixed-length window.
	for attempt := 0; attempt < 3 && !found; attempt++ {
		candidates, err := d.Browse(ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("Browse: %v", err)
		}
		for _, c := range candidates {
			if c.ServiceName == "SpikePrinter" && c.Port == port {
				candidate = c
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("Browse did not find SpikePrinter on port %d", port)
	}

	report, err := Inspect(ctx, candidate.URI)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", candidate.URI, err)
	}

	if _, ok := report.Attributes["printer-uuid"]; !ok {
		t.Error("report is missing printer-uuid")
	}
	if _, ok := report.Attributes["printer-device-id"]; !ok {
		t.Error("report is missing printer-device-id")
	}
}

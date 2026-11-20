//go:build integration && darwin

package inspect

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
)

// startFakePrinter launches ippeveprinter advertising "SpikePrinter" over
// Bonjour on a free TCP port, waits for it to accept connections, and
// returns the chosen port and a cleanup func that kills it. ippeveprinter
// advertises immediately on start; this environment blocks foreground
// sleep, so readiness is polled instead of waited on blindly.
func startFakePrinter(t *testing.T) (port int, cleanup func()) {
	t.Helper()

	port = freePort(t)
	spoolDir := t.TempDir()

	cmd := exec.Command("/usr/bin/ippeveprinter",
		"-p", fmt.Sprintf("%d", port),
		"-d", spoolDir,
		"-D", "printkeeper integration test fixture",
		"SpikePrinter",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ippeveprinter: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("ippeveprinter did not open port %d within deadline: %v", port, err)
		}
	}

	return port, func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestBrowseAndInspectAgainstFakePrinter(t *testing.T) {
	port, cleanup := startFakePrinter(t)
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

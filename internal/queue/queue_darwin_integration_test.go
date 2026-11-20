//go:build integration && darwin

package queue

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/fakeprinter"
)

// testQueueName is deliberately namespaced so it is obviously this test's
// own queue in `lpstat -t` output, never mistaken for a printer the
// developer actually uses.
const testQueueName = "printkeeper_itest"

// TestDarwinAdminFullRepairCycle drives the darwin Admin through the full
// sequence the repair ladder will later perform: create a queue pointing
// at a device, discover that the device moved, repoint the queue to its
// new address without renaming it, and confirm a real print job follows
// the repair rather than the stale address.
//
// This test creates a real CUPS queue on the machine running it. Every
// resource it creates is registered with t.Cleanup immediately after
// creation, so a failed assertion mid-test still leaves the host as it
// found it — no leftover queue, no leftover ippeveprinter process.
func TestDarwinAdminFullRepairCycle(t *testing.T) {
	admin := NewDarwinAdmin()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Registered before Create runs: if Create itself fails partway (CUPS
	// did create the queue before some later step errors) this still
	// cleans it up. Delete on a queue that was never created just errors
	// harmlessly, which is fine for a cleanup.
	t.Cleanup(func() {
		_ = admin.Delete(context.Background(), testQueueName)
	})

	portA, spoolA, killA := fakeprinter.Start(t, "QueueAdapterItestA")
	t.Cleanup(killA)
	uriA := fmt.Sprintf("ipp://127.0.0.1:%d/ipp/print", portA)

	if err := admin.Create(ctx, testQueueName, uriA, "printkeeper integration test queue"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	queues, err := admin.List(ctx)
	if err != nil {
		t.Fatalf("List after Create: %v", err)
	}
	q := findQueue(queues, testQueueName)
	if q == nil {
		t.Fatalf("List after Create did not report %q among %+v", testQueueName, queues)
	}
	if q.DeviceURI != uriA {
		t.Fatalf("queue DeviceURI = %q, want %q", q.DeviceURI, uriA)
	}

	// The printer "moved": a second fake printer stands in for it at a
	// different address, and the queue is repaired to point there —
	// without its name changing, exactly as a real repair would do.
	portB, spoolB, killB := fakeprinter.Start(t, "QueueAdapterItestB")
	t.Cleanup(killB)
	uriB := fmt.Sprintf("ipp://127.0.0.1:%d/ipp/print", portB)

	if err := admin.SetDeviceURI(ctx, testQueueName, uriB); err != nil {
		t.Fatalf("SetDeviceURI: %v", err)
	}

	queues, err = admin.List(ctx)
	if err != nil {
		t.Fatalf("List after SetDeviceURI: %v", err)
	}
	q = findQueue(queues, testQueueName)
	if q == nil {
		t.Fatalf("List after SetDeviceURI did not report %q among %+v", testQueueName, queues)
	}
	if q.DeviceURI != uriB {
		t.Fatalf("queue DeviceURI after repair = %q, want %q (repair did not take)", q.DeviceURI, uriB)
	}
	if q.Name != testQueueName {
		t.Fatalf("queue name changed to %q across the repair; a CUPS queue name must be permanent", q.Name)
	}

	printFile := filepath.Join(t.TempDir(), "job.txt")
	if err := os.WriteFile(printFile, []byte("printkeeper integration test job\n"), 0o644); err != nil {
		t.Fatalf("write test print file: %v", err)
	}

	out, err := exec.CommandContext(ctx, "/usr/bin/lp", "-d", testQueueName, printFile).CombinedOutput()
	if err != nil {
		t.Fatalf("lp -d %s %s: %v (%s)", testQueueName, printFile, err, out)
	}

	if !waitForNonEmptyDir(spoolB, 15*time.Second) {
		entries, _ := os.ReadDir(spoolB)
		t.Fatalf("spool directory for the repaired printer received nothing within the deadline (entries: %v)", entries)
	}

	// The job must have followed the repair, not the stale address: the
	// first fake printer's spool directory must stay empty.
	entriesA, err := os.ReadDir(spoolA)
	if err != nil {
		t.Fatalf("read original printer's spool dir: %v", err)
	}
	if len(entriesA) != 0 {
		t.Errorf("original printer's spool dir received %d file(s); the job should have gone to the repaired address instead", len(entriesA))
	}
}

// findQueue returns the queue named name, or nil if List did not report
// it.
func findQueue(queues []Queue, name string) *Queue {
	for i := range queues {
		if queues[i].Name == name {
			return &queues[i]
		}
	}
	return nil
}

// waitForNonEmptyDir polls dir until it contains at least one entry or
// timeout elapses. A print job's arrival is asynchronous from lp's point
// of view — lp returns once cupsd has accepted the job, not once the
// device has received it — so this cannot be a single, unpolled check.
func waitForNonEmptyDir(dir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

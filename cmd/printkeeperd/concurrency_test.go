package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/cli"
	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/ipc"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/store"
)

// unreachableURI points nowhere a probe can reach: nothing listens on
// port 1, so the connection is refused instantly and deterministically
// — the same fixture internal/ipc's and internal/cli's own tests use
// for a reliable "absent" result without touching a real network.
const unreachableURI = "ipp://127.0.0.1:1/ipp/print"

type fakeConcurrencyStore struct {
	mu      sync.Mutex
	adopted []store.Adopted
}

func (f *fakeConcurrencyStore) Load() ([]store.Adopted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Adopted(nil), f.adopted...), nil
}

func (f *fakeConcurrencyStore) Save(a []store.Adopted) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopted = a
	return nil
}

type fakeConcurrencyAdmin struct{ queues []queue.Queue }

func (f *fakeConcurrencyAdmin) List(ctx context.Context) ([]queue.Queue, error) { return f.queues, nil }
func (f *fakeConcurrencyAdmin) Create(ctx context.Context, name, uri, description string) error {
	return nil
}
func (f *fakeConcurrencyAdmin) SetDeviceURI(ctx context.Context, name, uri string) error { return nil }
func (f *fakeConcurrencyAdmin) Delete(ctx context.Context, name string) error            { return nil }

type fakeConcurrencyDiscoverer struct{}

func (fakeConcurrencyDiscoverer) Browse(ctx context.Context, timeout time.Duration) ([]discovery.Candidate, error) {
	return nil, nil
}

// TestHealCycleAndDispatchDoNotRaceOnSharedApp is the regression test
// for ipc.RunExclusive: a reviewer's finding was that the fix had no
// test exercising the actual race it protects against, only -race
// over suites that never run Serve's dispatch and healCycle against
// the same *cli.App at the same time — a future refactor moving one
// field access outside RunExclusive would be caught by nothing.
//
// This drives exactly those two call sites concurrently against one
// shared app: a tight loop calling healCycle directly (the daemon's
// own background path), and a second tight loop dialling the socket
// and calling Status() (Serve's dispatch path, which app.Doctor -
// not app.Heal - answers; either call site writes to the same
// app.Out, which is the field RunExclusive exists to protect). Run
// under `go test -race`, a genuine data race on app.Out — Doctor's
// buffer swap racing a concurrent Heal's writes — would be reported.
// The assertion on every observed response being the complete,
// expected sentence (never truncated or interleaved with another
// writer's bytes) is the same invariant stated as a correctness
// check, independent of -race.
func TestHealCycleAndDispatchDoNotRaceOnSharedApp(t *testing.T) {
	const iterations = 40

	app := &cli.App{
		Store: &fakeConcurrencyStore{adopted: []store.Adopted{{
			QueueName:   "Reception_Printer",
			Fingerprint: identity.FromCandidate(discovery.Candidate{ServiceName: "Reception Printer"}),
		}}},
		Queues: &fakeConcurrencyAdmin{queues: []queue.Queue{{Name: "Reception_Printer", DeviceURI: unreachableURI}}},
		Disco:  fakeConcurrencyDiscoverer{},
		Out:    io.Discard,
	}

	// Short, /tmp-rooted socket dir: this task's own earlier tests
	// already tripped over t.TempDir()'s longer, test-name-nested path
	// exceeding macOS's 104-byte sockaddr_un limit.
	dir, err := os.MkdirTemp("/tmp", "pk")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- ipc.Serve(ctx, socketPath, app) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(socketPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Serve did not create socket %s in time", socketPath)
		}
		time.Sleep(10 * time.Millisecond)
	}

	const wantOutput = "Reception Printer is not responding right now.\n"

	var wg sync.WaitGroup

	// The daemon's own background path: healCycle, called directly and
	// repeatedly, exactly as runProbeLoop does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			healCycle(ctx, app, io.Discard)
		}
	}()

	// Serve's dispatch path: a fresh connection per call, same as any
	// real `printkeeper doctor` invocation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			client, dialErr := ipc.Dial(socketPath)
			if dialErr != nil {
				t.Errorf("Dial: %v", dialErr)
				return
			}
			output, statusErr := client.Status()
			client.Close()
			if statusErr != nil {
				t.Errorf("Status(): %v", statusErr)
				return
			}
			if output != wantOutput {
				t.Errorf("Status() = %q, want %q (a torn or interleaved write would show up here as something else entirely)", output, wantOutput)
				return
			}
		}
	}()

	wg.Wait()
	cancel()

	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx was cancelled")
	}
}

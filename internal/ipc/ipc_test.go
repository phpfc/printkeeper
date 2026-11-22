package ipc

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/cli"
	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/store"
)

// unreachableURI points nowhere a probe can reach: nothing listens on
// port 1, so the connection is refused instantly and deterministically —
// the same fixture internal/cli's own tests use for a reliable
// "absent" result without touching a real network.
const unreachableURI = "ipp://127.0.0.1:1/ipp/print"

// fakeStore is a store.Store double backed by an in-memory slice.
type fakeStore struct{ adopted []store.Adopted }

func (f *fakeStore) Load() ([]store.Adopted, error) { return f.adopted, nil }
func (f *fakeStore) Save(a []store.Adopted) error   { f.adopted = a; return nil }

// fakeAdmin is a queue.Admin double whose List is fixed by the test.
// Create/SetDeviceURI/Delete are never exercised by this package's
// tests — ipc only ever drives App through its own methods, never
// queue.Admin directly — so they simply succeed.
type fakeAdmin struct{ queues []queue.Queue }

func (f *fakeAdmin) List(ctx context.Context) ([]queue.Queue, error) { return f.queues, nil }
func (f *fakeAdmin) Create(ctx context.Context, name, uri, description string) error {
	return nil
}
func (f *fakeAdmin) SetDeviceURI(ctx context.Context, name, uri string) error { return nil }
func (f *fakeAdmin) Delete(ctx context.Context, name string) error            { return nil }

// fakeDiscoverer is a discovery.Discoverer double that finds nothing —
// Heal's own browse step has nothing to do with this package's tests.
type fakeDiscoverer struct{}

func (fakeDiscoverer) Browse(ctx context.Context, timeout time.Duration) ([]discovery.Candidate, error) {
	return nil, nil
}

// testApp builds a *cli.App with one adopted printer ("Reception
// Printer") whose queue points nowhere reachable, so a "status" request
// against it deterministically reports the printer as not responding —
// enough to prove the request actually reached App.Doctor rather than
// returning canned data.
func testApp() *cli.App {
	adopted := store.Adopted{
		QueueName:   "Reception_Printer",
		Fingerprint: identity.FromCandidate(discovery.Candidate{ServiceName: "Reception Printer"}),
	}
	return &cli.App{
		Store:  &fakeStore{adopted: []store.Adopted{adopted}},
		Queues: &fakeAdmin{queues: []queue.Queue{{Name: "Reception_Printer", DeviceURI: unreachableURI, Description: "Reception Printer"}}},
		Disco:  fakeDiscoverer{},
	}
}

// startServer starts Serve in the background against a short-lived
// socket, waits for the socket to exist, and returns its path plus a
// cleanup that cancels the server and waits for Serve to return.
//
// It deliberately does not build the socket path under t.TempDir():
// that nests inside a directory named after the test function, and a
// descriptive test name here is long enough that the resulting path
// exceeds macOS's 104-byte sockaddr_un limit — net.Listen then fails
// with no obvious symptom beyond "the socket never appears", which is
// exactly what happened the first time this test ran against
// t.TempDir() directly. os.MkdirTemp("", "pk") gives the same
// isolated, per-test directory with a short, fixed-length name instead.
func startServer(t *testing.T, app *cli.App) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "pk")
	if err != nil {
		t.Fatalf("create temp dir for socket: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	socketPath := filepath.Join(dir, "s")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- Serve(ctx, socketPath, app) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("Serve exited before creating socket %s: %v", socketPath, err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("Serve did not create socket %s in time", socketPath)
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after ctx was cancelled")
		}
	})

	return socketPath
}

// TestServeAnswersStatusWithAdoptedPrinter is the brief's round-trip
// test: a "status" request over the socket must reach the real App
// methods (not some stub), proven by the adopted printer's own name
// showing up in the response.
func TestServeAnswersStatusWithAdoptedPrinter(t *testing.T) {
	socketPath := startServer(t, testApp())

	client, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial(%s) error = %v", socketPath, err)
	}
	defer client.Close()

	output, err := client.Status()
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !strings.Contains(output, "Reception Printer") {
		t.Fatalf("Status() = %q, want it to name the adopted printer", output)
	}
}

// TestSocketIsCreatedWithOwnerOnlyPermissions is the whole of the
// socket's access control, so this asserts the permission the server
// itself produced on disk — not a value the test set — by stat-ing the
// file right after Serve created it, before anything else touches it.
func TestSocketIsCreatedWithOwnerOnlyPermissions(t *testing.T) {
	socketPath := startServer(t, testApp())

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat %s: %v", socketPath, err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("socket %s has permissions %04o, want 0600", socketPath, got)
	}
}

// TestMalformedLineGetsErrorResponseWithoutKillingServer is the brief's
// explicit robustness requirement. It proves two things a server that
// merely ignores all input would not: the malformed line gets back a
// response carrying a non-empty error (not silence), and the server
// keeps serving correctly afterward — both on the very same connection
// (a second, well-formed line after the bad one) and on a brand new one
// (proving the listener itself is still alive, not just one lucky
// goroutine).
func TestMalformedLineGetsErrorResponseWithoutKillingServer(t *testing.T) {
	socketPath := startServer(t, testApp())

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write malformed line: %v", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read response to malformed line: %v", err)
	}
	if !strings.Contains(line, `"error"`) {
		t.Fatalf("response to malformed line = %q, want a non-empty error field", line)
	}

	// The same connection must still work for a well-formed request
	// right after the bad one.
	if _, err := conn.Write([]byte(`{"method":"status","id":1}` + "\n")); err != nil {
		t.Fatalf("write well-formed line on same connection: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read response on same connection after malformed line: %v", err)
	}
	if !strings.Contains(line, "Reception Printer") {
		t.Fatalf("response on same connection after malformed line = %q, want the adopted printer named", line)
	}

	// A brand new connection must also still be served — the listener
	// itself, not just this one goroutine, must still be alive.
	client, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial after malformed line: %v", err)
	}
	defer client.Close()

	output, err := client.Status()
	if err != nil {
		t.Fatalf("Status() after malformed line error = %v", err)
	}
	if !strings.Contains(output, "Reception Printer") {
		t.Fatalf("Status() after malformed line = %q, want the adopted printer named", output)
	}
}

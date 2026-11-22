package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/phpfc/printkeeper/internal/ipc"
)

// fakeDaemonSocket starts a bare unix listener at the exact path
// socketPath() resolves to under a temporary HOME this test sets, and
// returns a cleanup. It does not use ipc.Serve at all — these tests
// are about cmd/printkeeper's own fallback decision, not about the
// real protocol, so a raw listener under full test control is more
// direct than standing up a real daemon.
//
// HOME is pointed at a short, /tmp-rooted directory rather than the
// default per-test t.TempDir() (which nests under a directory named
// after the test function): the resulting socket path — HOME +
// "/Library/Application Support/printkeeper/sock" — must stay under
// macOS's 104-byte sockaddr_un limit, a limit this same task's
// internal/ipc tests already tripped over once with a longer path.
func fakeDaemonSocket(t *testing.T, handle func(net.Conn)) {
	t.Helper()

	home, err := os.MkdirTemp("/tmp", "pk")
	if err != nil {
		t.Fatalf("create temp HOME: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)

	dir := filepath.Join(home, "Library", "Application Support", "printkeeper")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	sock := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}
	t.Cleanup(func() { l.Close() })

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		handle(conn)
	}()
}

// TestRunViaDaemonFallsBackOnTransportFailure is the regression for a
// reviewer's finding alongside the client-deadline fix: once a daemon
// has been dialled, a *transport*-level failure (the connection
// dropped, the read failed, nothing decodable came back — anything
// internal/ipc.TransportError covers) must still fall back to running
// the command in-process, exactly as a failed Dial already does. A
// daemon that accepted the connection and then went silent is
// indistinguishable, from the user's point of view, from no daemon
// running at all, and defeats the entire point of "prefer the daemon,
// fall back otherwise" if it instead surfaces as a hard error.
func TestRunViaDaemonFallsBackOnTransportFailure(t *testing.T) {
	fakeDaemonSocket(t, func(conn net.Conn) {
		// Accept, then go silent — close without ever writing a
		// response. Call's Read will fail quickly (EOF/connection
		// reset), not after waiting out any timeout, so this test
		// stays fast.
		conn.Close()
	})

	output, ok, err := runViaDaemon("status")
	if ok {
		t.Fatalf("runViaDaemon() ok = true, want false (fall back to in-process) for a transport-level failure; output=%q err=%v", output, err)
	}
	if err != nil {
		t.Fatalf("runViaDaemon() err = %v, want nil when falling back (the caller runs in-process instead, not reporting a half-answer as a failure)", err)
	}
}

// TestRunViaDaemonSurfacesARealDaemonError is the other half of the
// same distinction: once the daemon has genuinely answered — even
// with an application-level failure, not a transport one — that
// answer is real and must be reported, not silently discarded in
// favour of redoing the same (likely also failing) work in-process.
func TestRunViaDaemonSurfacesARealDaemonError(t *testing.T) {
	fakeDaemonSocket(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		if _, err := conn.Read(buf); err != nil {
			return
		}
		// A well-formed response carrying an application-level error —
		// what App.Doctor itself would have produced on a real
		// failure, already humanised by the time it reaches here.
		conn.Write([]byte(`{"id":1,"error":"this computer's printing service isn't responding right now"}` + "\n"))
	})

	output, ok, err := runViaDaemon("status")
	if !ok {
		t.Fatalf("runViaDaemon() ok = false, want true: the daemon did answer, just with an error, so the caller must not silently retry in-process")
	}
	if err == nil {
		t.Fatalf("runViaDaemon() err = nil, want the daemon's own error surfaced; output=%q", output)
	}
}

// TestRunViaDaemonDoesNotFallBackOnBusyResponse is the regression for
// the coordinator's ruling: a busy daemon is alive and mid-sweep, so
// running a competing in-process sweep against the same queues at the
// same time would be worse than waiting — exactly the kind of
// concurrent, uncoordinated access to printer configuration this
// slice's own RunExclusive fix exists to prevent. Busy must be
// reported, like any other real application-level answer, never
// treated as if no daemon had answered at all.
func TestRunViaDaemonDoesNotFallBackOnBusyResponse(t *testing.T) {
	fakeDaemonSocket(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		if _, err := conn.Read(buf); err != nil {
			return
		}
		resp := fmt.Sprintf(`{"id":1,"error":%q}`, ipc.BusyResponseMessage)
		conn.Write([]byte(resp + "\n"))
	})

	output, ok, err := runViaDaemon("heal")
	if !ok {
		t.Fatalf("runViaDaemon() ok = false, want true: a busy daemon answered for real and must not trigger a competing in-process sweep; output=%q", output)
	}
	if err == nil || err.Error() != ipc.BusyResponseMessage {
		t.Fatalf("runViaDaemon() err = %v, want exactly the busy message %q", err, ipc.BusyResponseMessage)
	}
}

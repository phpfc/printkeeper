package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startWedgedListener starts a bare unix listener that accepts
// connections and then goes silent on each one — read nothing, write
// nothing back — standing in for a daemon that is wedged badly enough
// not to answer at all. It returns the socket path and a channel that
// receives once per accepted connection, so a test can synchronise on
// "the server actually has this connection" before racing a deadline
// against it.
func startWedgedListener(t *testing.T) (socketPath string, accepted <-chan struct{}) {
	t.Helper()

	dir, err := os.MkdirTemp("", "pk")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath = filepath.Join(dir, "s")

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	ch := make(chan struct{}, 8)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			ch <- struct{}{}
			// Hold the connection open and silent for the rest of this
			// test's lifetime; t.Cleanup closing the listener above
			// does not close already-accepted connections, but the
			// test process exiting does.
			go func(c net.Conn) {
				buf := make([]byte, 1)
				c.Read(buf) //nolint:errcheck // deliberately discarded: just holding the conn open
			}(conn)
		}
	}()

	return socketPath, ch
}

// TestCallTimesOutRatherThanHangingForever is the regression for a
// reviewer's finding: Call previously set no deadline at all, so a
// wedged or merely slow daemon left a client (and, through it,
// `printkeeper doctor`/`heal`) blocked forever instead of failing in a
// way the caller could fall back from. statusCallTimeout is lowered
// for the duration of this test so it does not need to wait out the
// real production value to prove the point.
//
// The outer select with its own 2s bound exists so this test fails
// fast even if the fix regresses completely (Call hangs again): a test
// for "this must not hang forever" that itself has no bound would hang
// the whole suite instead of failing.
func TestCallTimesOutRatherThanHangingForever(t *testing.T) {
	saved := statusCallTimeout
	statusCallTimeout = 150 * time.Millisecond
	defer func() { statusCallTimeout = saved }()

	socketPath, accepted := startWedgedListener(t)

	client, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	<-accepted

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, callErr := client.Call("status", nil)
		done <- callErr
	}()

	select {
	case callErr := <-done:
		elapsed := time.Since(start)
		if callErr == nil {
			t.Fatal("Call() returned no error against a server that never answered")
		}
		if elapsed > 2*time.Second {
			t.Fatalf("Call() took %s to fail; want it bounded by statusCallTimeout (%s)", elapsed, statusCallTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call() did not return within 2s against a server that never answers — it set no deadline")
	}
}

// TestStatusAndHealUseDifferentTimeouts is the regression for the
// coordinator's follow-up finding: a single shared callTimeout for
// both methods was either too long for "status" (a human watching a
// terminal concludes the tool is broken long before a 70s fallback
// saves them) or too short for "heal" (which legitimately browses the
// network and probes every adopted printer). Against the same wedged
// server, a "status" call must fail close to statusCallTimeout, and a
// "heal" call on the very same connection must take noticeably longer
// — proving the two methods are actually bounded by different budgets,
// not coincidentally similar numbers.
func TestStatusAndHealUseDifferentTimeouts(t *testing.T) {
	savedStatus, savedHeal := statusCallTimeout, healCallTimeout
	statusCallTimeout = 150 * time.Millisecond
	healCallTimeout = 1500 * time.Millisecond
	defer func() { statusCallTimeout, healCallTimeout = savedStatus, savedHeal }()

	socketPath, accepted := startWedgedListener(t)

	client, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	<-accepted

	start := time.Now()
	if _, callErr := client.Call("status", nil); callErr == nil {
		t.Fatal("status Call() returned no error against a server that never answered")
	}
	statusElapsed := time.Since(start)
	if statusElapsed > 1*time.Second {
		t.Fatalf("status Call() took %s to fail, want bounded close to statusCallTimeout (%s)", statusElapsed, statusCallTimeout)
	}

	start = time.Now()
	if _, callErr := client.Call("heal", nil); callErr == nil {
		t.Fatal("heal Call() returned no error against a server that never answered")
	}
	healElapsed := time.Since(start)
	if healElapsed < 1*time.Second {
		t.Fatalf("heal Call() returned after only %s — it used the short status deadline instead of its own longer healCallTimeout (%s)", healElapsed, healCallTimeout)
	}
}

// TestRunExclusiveReturnsContextErrorWhenLockNotAcquiredInTime is the
// regression for the second half of the same finding: dispatch's own
// requestTimeout context previously bounded nothing once execution
// reached appMu.Lock(), since a plain Mutex.Lock cannot be cancelled.
// A caller — such as Serve's own dispatch — that cannot get exclusive
// access before its own deadline must get a clean error back, not an
// indefinite block that leaves a client hanging regardless of the
// deadline it set on its own connection.
func TestRunExclusiveReturnsContextErrorWhenLockNotAcquiredInTime(t *testing.T) {
	release := make(chan struct{})
	holding := make(chan struct{})

	go func() {
		_ = RunExclusive(context.Background(), func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := RunExclusive(ctx, func() error {
		t.Fatal("fn ran despite the lock being held elsewhere")
		return nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunExclusive() returned no error while the lock was held elsewhere")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("RunExclusive() took %s to give up; want it bounded by ctx's own 100ms deadline", elapsed)
	}
}

//go:build integration && darwin

// Package fakeprinter starts a real IPP printer (ippeveprinter) for
// integration tests and tears it down again. It exists so the "wait for
// the port, don't sleep" dance — and the exact ippeveprinter invocation —
// lives in exactly one place, shared by every package whose integration
// tests need a real device to talk to.
package fakeprinter

import (
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"
)

// Start launches ippeveprinter advertising name over Bonjour on a free TCP
// port, waits for it to accept connections, and returns the chosen port
// and a cleanup func that kills it. ippeveprinter advertises immediately
// on start; this environment blocks foreground sleep, so readiness is
// polled instead of waited on blindly.
func Start(t *testing.T, name string) (port int, cleanup func()) {
	t.Helper()

	port = freePort(t)
	spoolDir := t.TempDir()

	cmd := exec.Command("/usr/bin/ippeveprinter",
		"-p", fmt.Sprintf("%d", port),
		"-d", spoolDir,
		"-D", "printkeeper integration test fixture",
		name,
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

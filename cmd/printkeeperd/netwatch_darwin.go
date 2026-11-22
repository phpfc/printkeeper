package main

import (
	"fmt"
	"sync"
	"syscall"
)

// networkChangeWatcher blocks until the kernel reports a routing table
// change — any interface or address change, a default route change,
// anything at all — delivered over a PF_ROUTE socket: the BSD kernel
// mechanism SCNetworkReachability and NWPathMonitor are themselves
// built on. Reading it directly needs no cgo and no Objective-C
// framework, so this binary keeps building on the Linux CI matrix
// every other package in this project already avoids a platform
// dependency for (see admin_other.go, discoverer_other.go, and this
// file's own netwatch_other.go).
//
// It never decodes the message: arrival alone is sufficient signal
// that "the network may have changed" — internal/daemon.NextProbeAt's
// own schedule is what decides whether that is worth acting on, not
// this watcher. A routing change often arrives as a short burst of
// several messages for one real event (an interface going down, say);
// runProbeLoop is what coalesces a burst into a single re-probe, not
// this type.
type networkChangeWatcher struct {
	fd        int
	closeOnce sync.Once
	closeErr  error
}

// newNetworkChangeWatcher opens the routing socket. It costs nothing
// while idle: Wait blocks inside the kernel rather than polling, so a
// daemon running this watcher produces zero scheduled wakeups of its
// own in the steady state — confirmed by hand with a short-timeout
// Read that genuinely waited out the full timeout before returning
// EAGAIN, rather than returning immediately (see this task's report).
func newNetworkChangeWatcher() (*networkChangeWatcher, error) {
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, 0)
	if err != nil {
		return nil, fmt.Errorf("open routing socket: %w", err)
	}
	return &networkChangeWatcher{fd: fd}, nil
}

// Wait blocks until one routing message arrives and then returns nil.
// It returns a non-nil error if the socket fails, or — the shutdown
// path — if Close is called from another goroutine while a Wait is in
// flight: there is no portable way to select a raw fd alongside a
// context's Done channel, so Close unblocking a pending Read with an
// error is how runProbeLoop stops this watcher's goroutine on
// shutdown instead.
func (w *networkChangeWatcher) Wait() error {
	buf := make([]byte, 2048)
	n, err := syscall.Read(w.fd, buf)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("routing socket closed")
	}
	return nil
}

// Close closes the routing socket, unblocking any Wait currently in
// progress with an error. It is idempotent — safe to call more than
// once — because runProbeLoop's own shutdown path does exactly that:
// the goroutine running Wait in a loop closes it via its own deferred
// Close once Wait returns an error, and the goroutine reacting to
// ctx.Done() closes it independently to unblock that very Wait in the
// first place. Without sync.Once, that is a double-close of the same
// fd on every clean shutdown — harmless only by accident (nothing else
// happens to reuse the fd number in the brief window between the two
// calls), and exactly the kind of fd-reuse footgun that stops being
// harmless the moment something does.
func (w *networkChangeWatcher) Close() error {
	w.closeOnce.Do(func() {
		w.closeErr = syscall.Close(w.fd)
	})
	return w.closeErr
}

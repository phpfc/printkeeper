//go:build !darwin

package main

import "fmt"

// The daemon ships a darwin network-change watcher only: PF_ROUTE is a
// BSD kernel socket (see netwatch_darwin.go), and this project is
// "Linux is CI-only in v1" (design doc §11.6) — nothing here is ever
// shipped running on Linux. Returning an error rather than a working
// stub is deliberate: runProbeLoop's own fallback for this failure
// path is to run on the heartbeat alone, which is honestly slower to
// notice a network change but still correct, rather than silently
// pretending to watch for one and never firing.
type networkChangeWatcher struct{}

func newNetworkChangeWatcher() (*networkChangeWatcher, error) {
	return nil, fmt.Errorf("network-change watching is not implemented on this platform")
}

func (*networkChangeWatcher) Wait() error {
	return fmt.Errorf("network-change watching is not implemented on this platform")
}
func (*networkChangeWatcher) Close() error { return nil }

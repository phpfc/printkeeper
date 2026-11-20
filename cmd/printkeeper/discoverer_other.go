//go:build !darwin

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
)

// Task 1 ships the darwin discovery adapter only (see design doc §11.6:
// "Linux is CI-only in v1"). This stub keeps cmd/printkeeper building and
// vetting on every GOOS the CI matrix covers without pretending to discover
// anything on a platform that has no adapter yet.
type unsupportedDiscoverer struct{}

func newDiscoverer() discovery.Discoverer {
	return unsupportedDiscoverer{}
}

func (unsupportedDiscoverer) Browse(context.Context, time.Duration) ([]discovery.Candidate, error) {
	return nil, fmt.Errorf("printer discovery is not implemented on this platform yet")
}

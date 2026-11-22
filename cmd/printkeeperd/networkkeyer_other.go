//go:build !darwin

package main

import "github.com/phpfc/printkeeper/internal/store"

// unsupportedNetworkKeyer always reports "unknown": the daemon ships a
// darwin keyer only (design doc §11.6). A constant key means every
// network this stub ever runs on looks like the same one, which is a
// safe default (not a wrong one) for a platform with no real keying
// signal available at all. Mirrors cmd/printkeeper's own
// networkkeyer_other.go.
type unsupportedNetworkKeyer struct{}

func newNetworkKeyer() store.NetworkKeyer {
	return unsupportedNetworkKeyer{}
}

func (unsupportedNetworkKeyer) Key() string { return "unknown" }

//go:build !darwin

package main

import "github.com/phpfc/printkeeper/internal/store"

// unsupportedNetworkKeyer always reports "unknown": Task 5's keyer ships
// a darwin adapter only (design doc §11.6). A constant key means every
// network this stub ever runs on looks like the same one, which is a
// safe default (not a wrong one) for a platform with no real keying
// signal available at all.
type unsupportedNetworkKeyer struct{}

func newNetworkKeyer() store.NetworkKeyer {
	return unsupportedNetworkKeyer{}
}

func (unsupportedNetworkKeyer) Key() string { return "unknown" }

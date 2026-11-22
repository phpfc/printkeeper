//go:build !darwin

package main

// The daemon ships a darwin battery adapter only (see battery_darwin.go).
// Hardcoding "not on battery" matches the same safe default the darwin
// implementation itself falls back to for any machine it cannot read
// — see its own doc comment — so a platform with no adapter at all
// behaves identically to one whose adapter failed to read anything.
func onBattery() bool { return false }

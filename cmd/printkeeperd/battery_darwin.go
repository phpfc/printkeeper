package main

import (
	"bytes"
	"os/exec"
)

// ioregPath is the absolute path to the system's ioreg binary.
// Absolute, never looked up on PATH — matching this project's existing
// rule for shelling out (see internal/discovery's ippfindPath). A var,
// not a const, so it could be pointed at a stub if ever needed.
var ioregPath = "/usr/sbin/ioreg"

// onBattery reports whether this machine is currently running on
// battery power.
//
// It reads ioreg's IORegistry dump of the system battery rather than
// pmset -g batt's own prose ("Now drawing from 'Battery Power'"),
// which is exactly the shape of human-facing text this project's rule
// against parsing localized CLI output exists to rule out.
// ExternalConnected, by contrast, is an IORegistry property name: a
// literal key in a data structure that ioreg's -r (recursive) format
// prints as a side effect of its own fixed output grammar, not prose
// composed for a reader — the same character ippfind's own --exec
// format string already has elsewhere in this project
// (internal/discovery/discovery_darwin.go). See parseExternalConnected
// for how the value itself is read.
func onBattery() bool {
	out, err := exec.Command(ioregPath, "-rn", "AppleSmartBattery").Output()
	if err != nil {
		// Conservative default: a machine ioreg could not be asked
		// about is treated as plugged in, not on battery. The wrong
		// guess in the other direction — "on battery" on a machine
		// this can't read — would wrongly suppress the absent-printer
		// backoff down to the plain heartbeat, which risks missing a
		// printer that came back; that is the real anti-goal here, not
		// merely a cosmetic one.
		return false
	}
	return parseExternalConnected(out)
}

// parseExternalConnected reports whether an `ioreg -rn
// AppleSmartBattery` dump says external power is absent — i.e. this
// machine is running on its battery right now. A dump with no
// AppleSmartBattery service at all — any desktop with no battery,
// comfortably the most common case this function has to get right by
// default — carries no ExternalConnected key at all, and is treated
// the same as "plugged in": see onBattery's own doc comment for why
// that is the safe direction to default to.
func parseExternalConnected(out []byte) bool {
	const key = `"ExternalConnected" = `

	idx := bytes.Index(out, []byte(key))
	if idx == -1 {
		return false
	}

	rest := bytes.TrimSpace(out[idx+len(key):])
	return bytes.HasPrefix(rest, []byte("No"))
}

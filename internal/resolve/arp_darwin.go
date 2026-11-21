package resolve

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// arpPath is the absolute path to the system's arp binary. Absolute,
// never looked up on PATH, and a var rather than a const so tests can
// point it at a path that does not exist to exercise MACFor's failure
// path without touching this machine's real ARP table.
var arpPath = "/usr/sbin/arp"

// MACFor asks the system's ARP table for host's hardware address and
// returns it in canonical lower-case colon-separated form, or ""
// on any failure: no entry, host unreachable, or the arp binary itself
// missing or failing to run.
//
// identity.FromCandidate already prefers identity.MACFromUUID over this:
// it derives a printer's MAC straight from a version-1 printer-uuid's
// node field with no I/O at all, and measured directly against this
// project's real printer, that derivation is correct (its node field is
// exactly its ARP-reported MAC). MACFor exists for the fallback case —
// a printer-uuid that is not version 1 — but measured directly against
// the same printer, ARP returns nothing at all from this path: not an
// error, just an empty, successful result, when invoked from a compiled
// Go binary running outside a terminal, even though the identical
// command run from a shell on the same machine, at the same moment,
// lists the device correctly. This reproduced both inside and outside a
// sandboxed environment, so it is not this project's sandbox causing
// it: it is a macOS privacy boundary that follows the responsible
// process (a background daemon is on the wrong side of it), not the
// network state being asked about. MACFor is implemented correctly
// against arp's actual output format, but it should not be presented as
// a path that works from printkeeper's daemon in practice.
func MACFor(ctx context.Context, host string) string {
	cmd := exec.CommandContext(ctx, arpPath, "-n", host)
	out, err := cmd.Output()
	if err != nil {
		// Covers both the documented "no entry" case (exit status 1)
		// and every other failure (bad host, arp binary missing or
		// unreadable): MACFor's contract is best-effort, so all of
		// these collapse to the same empty result rather than an error
		// the caller would have to handle.
		return ""
	}
	return parseArpOutput(string(out))
}

// parseArpOutput parses the output of `arp -n <host>` and returns the
// hardware address it reports, normalised to canonical lower-case
// colon-separated two-digit-per-octet form. arp's own output omits a
// leading zero on any octet below 0x10 (observed directly against this
// network: "e0:bb:9e:0:97:3d", not "...:00:...") — that must be padded
// back so this MAC compares equal, byte for byte, with one obtained any
// other way (ARP elsewhere, or MACFromUUID). Returns "" for the
// documented "no entry" case, an "(incomplete)" entry, or any line this
// does not recognise.
func parseArpOutput(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != "at" || i+1 >= len(fields) {
				continue
			}
			if mac := normaliseMAC(fields[i+1]); mac != "" {
				return mac
			}
		}
	}
	return ""
}

// normaliseMAC validates that raw is six colon-separated hex octets (1
// or 2 hex digits each, as arp's own output varies) and returns it in
// canonical lower-case two-digit-per-octet form. Returns "" for
// anything else, including arp's literal "(incomplete)" placeholder for
// an entry still being resolved.
func normaliseMAC(raw string) string {
	parts := strings.Split(raw, ":")
	if len(parts) != 6 {
		return ""
	}
	octets := make([]string, 6)
	for i, p := range parts {
		if len(p) == 0 || len(p) > 2 {
			return ""
		}
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return ""
		}
		octets[i] = hexByte(byte(v))
	}
	return strings.Join(octets, ":")
}

// hexByte renders b as exactly two lower-case hex digits.
func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}

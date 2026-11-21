package identity

import (
	"strconv"
	"strings"
)

// MACFromUUID extracts a MAC address from uuid's node field when uuid is
// an RFC 4122 version-1 (time-based) UUID. Such a UUID's low 48 bits were
// built from the generating host's own IEEE 802 (MAC) address (RFC 4122
// §4.1.6) — measured directly against the real printer this project
// exists for: its printer-uuid,
// urn:uuid:cfe92100-67c4-11d4-a45f-e0bb9e00973d, is version 1, and its
// node field (e0bb9e00973d) is exactly the MAC its ARP entry reports
// (e0:bb:9e:00:97:3d). That makes this the primary source for a
// printer's MAC — not a fallback — because ARP itself was measured to be
// unreliable from a background process (see resolve.MACFor's doc
// comment): a version-1 printer-uuid is often the only working source
// there is.
//
// This has no I/O and nothing platform-specific — it is pure string work
// about identity — which is why it lives here rather than in
// internal/resolve (which imports this package, so the reverse import
// is not an option): resolve.MACFor remains the ARP-based fallback for a
// printer whose printer-uuid is not version 1.
//
// Returns "" when uuid is not version 1, is malformed, or its node field
// has the multicast/locally-administered bit set in its first octet.
// RFC 4122 §4.1.6 permits a randomly generated node ID in that case
// specifically because no real IEEE 802 address was available when the
// UUID was minted — returning such a value as a MAC would fabricate an
// address that was never a real piece of hardware's address, which is
// worse than having no MAC at all: it would either match nothing or,
// worse, collide with another device that was handed the same convention.
func MACFromUUID(uuid string) string {
	u := normaliseUUID(uuid)

	if len(u) != 36 || u[8] != '-' || u[13] != '-' || u[18] != '-' || u[23] != '-' {
		return ""
	}
	if u[14] != '1' { // version nibble: first hex digit of the third group
		return ""
	}

	node := u[24:36]
	if !isHexString(node) {
		return ""
	}

	firstOctet, err := strconv.ParseUint(node[0:2], 16, 8)
	if err != nil {
		return ""
	}
	if firstOctet&0x01 != 0 { // multicast/locally-administered bit set
		return ""
	}

	return formatMACFromHex(node)
}

// isHexString reports whether s consists entirely of lower-case hex
// digits.
func isHexString(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// formatMACFromHex renders a 12-hex-digit string as a canonical
// lower-case, colon-separated, two-digit-per-octet MAC address, e.g.
// "e0bb9e00973d" -> "e0:bb:9e:00:97:3d". hex must already be exactly 12
// lower-case hex digits; callers validate that before calling.
func formatMACFromHex(hex string) string {
	octets := make([]string, 6)
	for i := range 6 {
		octets[i] = hex[i*2 : i*2+2]
	}
	return strings.Join(octets, ":")
}

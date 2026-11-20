// Package identity turns the signals discovery and inspection collect about
// a printer into an identity that survives the printer's address changing:
// a Fingerprint capturing what was known about it, and a scored Match
// against the printers currently on the network.
package identity

import (
	"strings"

	"github.com/phpfc/printkeeper/internal/discovery"
)

// Fingerprint is everything known about a printer's identity, independent
// of where it currently lives on the network. Two Fingerprints describe the
// same physical printer when enough of these fields agree.
//
// MAC and DeviceID are not populated by FromCandidate: discovery.Candidate
// carries neither (MAC comes from a later task's ARP resolution, DeviceID
// from an IPP probe's printer-device-id attribute), so they stay their zero
// value here. They exist on this type for whatever builds a richer stored
// Fingerprint to compare against.
type Fingerprint struct {
	UUID         string
	MAC          string
	DeviceID     string
	ServiceName  string
	ResourcePath string
	PDLs         []string
}

// FromCandidate builds a Fingerprint from a discovery.Candidate, normalising
// the UUID so it compares equal whether it arrived bare (Bonjour TXT, as
// Candidate.UUID always does today) or prefixed (an IPP printer-uuid
// attribute's shape).
func FromCandidate(c discovery.Candidate) Fingerprint {
	return Fingerprint{
		UUID:         normaliseUUID(c.UUID),
		ServiceName:  c.ServiceName,
		ResourcePath: c.ResourcePath,
		PDLs:         c.PDLs,
	}
}

// normaliseUUID strips an optional "urn:uuid:" prefix and lowercases, so a
// bare Bonjour UUID and an IPP printer-uuid attribute's value compare equal.
func normaliseUUID(uuid string) string {
	u := strings.ToLower(strings.TrimSpace(uuid))
	return strings.TrimPrefix(u, "urn:uuid:")
}

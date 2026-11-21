package identity

import (
	"testing"

	"github.com/phpfc/printkeeper/internal/discovery"
)

// TestMACFromUUID pins the measurement taken against the real printer
// this project exists for: its printer-uuid,
// urn:uuid:cfe92100-67c4-11d4-a45f-e0bb9e00973d, is RFC 4122 version 1,
// and its node field (e0bb9e00973d) is exactly the MAC the ARP table
// reports for that device (e0:bb:9e:00:97:3d). A version-4 (random) UUID
// must yield "", not a fabricated MAC: a future change must not start
// inventing MACs out of UUIDs that were never built from one.
func TestMACFromUUID(t *testing.T) {
	cases := []struct {
		name string
		uuid string
		want string
	}{
		{
			name: "real printer's version-1 uuid yields its measured MAC",
			uuid: "urn:uuid:cfe92100-67c4-11d4-a45f-e0bb9e00973d",
			want: "e0:bb:9e:00:97:3d",
		},
		{
			name: "bare uuid without the urn:uuid: prefix",
			uuid: "cfe92100-67c4-11d4-a45f-e0bb9e00973d",
			want: "e0:bb:9e:00:97:3d",
		},
		{
			name: "uppercase uuid",
			uuid: "CFE92100-67C4-11D4-A45F-E0BB9E00973D",
			want: "e0:bb:9e:00:97:3d",
		},
		{
			name: "version 4 (random) uuid is not MAC-derived",
			uuid: "15f4c408-3a35-4057-5ab6-12023a25f895",
			want: "",
		},
		{
			name: "node field with the multicast/locally-administered bit set is not a real MAC",
			// version-1 uuid, but node field's first octet is f1
			// (binary 11110001 — low bit set), which RFC 4122 §4.1.6
			// reserves for a randomly generated node ID, never a real
			// IEEE 802 address.
			uuid: "cfe92100-67c4-11d4-a45f-f1bb9e00973d",
			want: "",
		},
		{
			name: "malformed uuid",
			uuid: "not-a-uuid",
			want: "",
		},
		{
			name: "empty string",
			uuid: "",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MACFromUUID(c.uuid); got != c.want {
				t.Fatalf("MACFromUUID(%q) = %q, want %q", c.uuid, got, c.want)
			}
		})
	}
}

// TestFromCandidatePopulatesMACFromVersion1UUID pins the ruling this
// test file exists to enforce: Fingerprint.MAC carries the
// second-heaviest weight in Match's scoring, and before this, nothing
// ever populated it — FromCandidate left it at its zero value even
// though a version-1 printer-uuid already contains a real MAC. Using
// the real printer's measured UUID/MAC pair directly, not a synthetic
// one, so this fails honestly if the derivation or its wiring into
// FromCandidate ever regresses.
func TestFromCandidatePopulatesMACFromVersion1UUID(t *testing.T) {
	f := FromCandidate(discovery.Candidate{UUID: "urn:uuid:cfe92100-67c4-11d4-a45f-e0bb9e00973d"})
	if f.MAC != "e0:bb:9e:00:97:3d" {
		t.Fatalf("FromCandidate(version-1 UUID).MAC = %q, want %q", f.MAC, "e0:bb:9e:00:97:3d")
	}
}

// TestFromCandidateLeavesMACEmptyForNonVersion1UUID pins the other half:
// a candidate whose UUID is not version 1 (the common case — most of
// today's fixtures are version 3/4/5) must leave MAC genuinely empty,
// not some sentinel or guessed value. Match's fieldScore treats an empty
// field on either side as "never matches", so an absent MAC must stay
// absent rather than becoming evidence of anything.
func TestFromCandidateLeavesMACEmptyForNonVersion1UUID(t *testing.T) {
	f := FromCandidate(discovery.Candidate{UUID: "15f4c408-3a35-4057-5ab6-12023a25f895"})
	if f.MAC != "" {
		t.Fatalf("FromCandidate(version-4 UUID).MAC = %q, want empty", f.MAC)
	}
}

package queue

import (
	"strings"
	"testing"
)

// TestSanitiseName pins Review Focus 4: a CUPS queue name is permanent
// from the moment it is created, so getting the character set and the
// collision handling right here matters more than almost anything else
// in this package.
func TestSanitiseName(t *testing.T) {
	cases := []struct {
		name       string
		advertised string
		taken      []string
		want       string
	}{
		{
			name:       "spaces and slashes are stripped",
			advertised: "HP LaserJet 4/Floor 3",
			taken:      nil,
			want:       "hp_laserjet_4_floor_3",
		},
		{
			name:       "accents are folded, not dropped",
			advertised: "Impressora Térrea",
			taken:      nil,
			want:       "impressora_terrea",
		},
		{
			name:       "a taken name gets a numeric suffix",
			advertised: "Office Printer",
			taken:      []string{"office_printer"},
			want:       "office_printer_2",
		},
		{
			name:       "two already-taken suffixes advance the counter",
			advertised: "Office Printer",
			taken:      []string{"office_printer", "office_printer_2"},
			want:       "office_printer_3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitiseName(tc.advertised, tc.taken)
			if got != tc.want {
				t.Errorf("SanitiseName(%q, %v) = %q, want %q", tc.advertised, tc.taken, got, tc.want)
			}
			assertValidCUPSName(t, got)
		})
	}
}

// TestSanitiseNameNeverCollides is the collision direction the brief
// calls out explicitly: two different advertised names whose disallowed
// characters sanitise to the same run (space, "/", and "#" all become a
// single "_") must never end up sharing a queue name. A collision here
// means the second printer's repair ladder would silently start acting
// on the first printer's queue.
func TestSanitiseNameNeverCollides(t *testing.T) {
	first := SanitiseName("HP LaserJet 4/Floor 3", nil)
	second := SanitiseName("HP LaserJet 4#Floor 3", []string{first})

	if first == second {
		t.Fatalf("two different advertised names both sanitised to %q", first)
	}
	assertValidCUPSName(t, second)
}

// TestSanitiseNameRespectsLengthCap confirms the 127-character CUPS
// ceiling is honoured even once a numeric suffix is appended, and —
// this is the case that actually matters — that the result still
// differs from the already-taken name once truncated.
//
// A regression to the classic bug (append the suffix first, then
// truncate the whole thing to 127, rather than truncating the base to
// make room for the suffix) would produce exactly the taken name back:
// truncating 127 "a"s + "_2" down to 127 characters yields 127 "a"s
// again. That result is also <= 127 characters long, so a test that
// only checked the length would still pass while the names silently
// collided. Asserting inequality against the taken name catches that;
// feeding the result back in as a second taken name and checking a
// third call still comes out distinct pins the property generally,
// rather than this one example of it.
func TestSanitiseNameRespectsLengthCap(t *testing.T) {
	long := strings.Repeat("a", 200)
	taken := strings.Repeat("a", 127)

	got := SanitiseName(long, []string{taken})

	if len(got) > 127 {
		t.Fatalf("SanitiseName returned a name of length %d, want <= 127", len(got))
	}
	if got == taken {
		t.Fatalf("SanitiseName(%q, [%q]) = %q, the taken name itself — truncation must happen before the suffix is appended, not after", long, taken, got)
	}
	assertValidCUPSName(t, got)

	second := SanitiseName(long, []string{taken, got})
	if second == taken || second == got {
		t.Fatalf("SanitiseName(%q, [%q, %q]) = %q, want a third name distinct from both already-taken names", long, taken, got, second)
	}
	assertValidCUPSName(t, second)
}

// assertValidCUPSName checks the character-set and length rules CUPS
// enforces on a queue name: printable ASCII, no space, "/", or "#", at
// most 127 characters.
func assertValidCUPSName(t *testing.T, name string) {
	t.Helper()
	if name == "" {
		t.Fatalf("got empty name")
	}
	if len(name) > 127 {
		t.Fatalf("name %q is %d characters, want <= 127", name, len(name))
	}
	for _, r := range name {
		if r == ' ' || r == '/' || r == '#' {
			t.Fatalf("name %q contains disallowed character %q", name, r)
		}
		if r < 0x21 || r > 0x7e {
			t.Fatalf("name %q contains non-printable-ASCII character %q", name, r)
		}
	}
}

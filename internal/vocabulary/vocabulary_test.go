package vocabulary

import "testing"

// TestLeaked pins every case a reviewer traced through this package,
// including two gaps a later round found in the guard itself (not in
// anything it was guarding): "daemon", "launchd", and "socket" were
// checked case-sensitively, so the capitalised prose form that
// actually caused a leak ("Daemon is busy...") slipped through
// entirely; and "ipps://" — the TLS-preferred scheme, more likely to
// appear in a leaked URI than "ipp://" is — was caught by nothing at
// all. Both are pinned here as cases that must go red against a
// vocabulary.go that does not yet handle them, so this test
// demonstrates the gap rather than merely covering its own fix.
func TestLeaked(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantLeak bool
	}{
		// The capitalised-prose-word gap (finding: case-sensitivity on
		// "daemon"/"launchd"/"socket" bought nothing and cost the
		// capitalised form).
		{"capitalised Daemon at sentence start", "Daemon is busy right now.", true},
		{"capitalised Launchd at sentence start", "Launchd restarted the service.", true},
		{"capitalised Socket at sentence start", "Socket permissions were wrong.", true},
		{"lower-case daemon still caught", "the daemon is busy", true},
		{"lower-case launchd still caught", "launchd restarted it", true},
		{"lower-case socket still caught", "a socket error occurred", true},

		// The ipps gap (finding: neither \bipp\b nor "ipp://" matches
		// "ipps", the scheme the TLS work made preferred).
		{"ipps scheme URI", "found at ipps://printer.local:631/", true},
		{"bare lower-case ipps word", "the ipps protocol answered", true},
		{"upper-case IPPS still caught (substring of IPP)", "spoke IPPS to the device", true},

		// Pre-existing behaviour that must not regress: real false
		// positives the acronym case-sensitivity and the bare-word
		// regex both exist to avoid.
		{"equipped is not a leak", "the printer is well equipped for this job", false},
		{"flipped is not a leak", "the switch flipped", false},
		{"tipped is not a leak", "the printer tipped over", false},
		{"cups of coffee is not a leak", "I had two cups of coffee", false},
		{"clean sentence", "Your printer is working fine.", false},

		// Pre-existing positives that must keep working.
		{"bare lower-case ipp word", "spoke ipp to the device", true},
		{"ipp scheme URI", "ipp://192.168.1.50:631/ipp/print", true},
		{"upper-case IPP acronym", "used IPP to print", true},
		{"CUPS acronym", "CUPS reported idle", true},
		{"mDNS acronym", "found via mDNS", true},
		{"URI acronym", "the URI was invalid", true},
		{"queue, any case", "the Queue was removed", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Leaked(tt.in)
			leaked := len(got) > 0
			if leaked != tt.wantLeak {
				t.Errorf("Leaked(%q) = %v (leaked=%v), want leaked=%v", tt.in, got, leaked, tt.wantLeak)
			}
		})
	}
}

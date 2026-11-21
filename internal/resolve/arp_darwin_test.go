package resolve

import "testing"

// These cases are recorded output of `arp -n <host>` on this machine,
// run directly from a shell (see the task report for why that
// qualification matters: the same command run from a compiled Go binary
// returns empty, with no error, for every host — a privacy boundary that
// follows the responsible process, not the network state).
func TestParseArpOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "entry found, arp omits leading zeros per octet",
			out:  "? (192.168.0.127) at e0:bb:9e:0:97:3d on en0 ifscope [ethernet]\n",
			want: "e0:bb:9e:00:97:3d",
		},
		{
			name: "entry found, every octet already two digits",
			out:  "? (192.168.0.1) at 74:3a:ef:74:6b:23 on en0 ifscope [ethernet]\n",
			want: "74:3a:ef:74:6b:23",
		},
		{
			name: "no entry for this host",
			out:  "192.168.0.250 (192.168.0.250) -- no entry\n",
			want: "",
		},
		{
			name: "incomplete entry (device not currently resolving)",
			out:  "? (192.168.0.200) at (incomplete) on en0 ifscope [ethernet]\n",
			want: "",
		},
		{
			name: "empty output",
			out:  "",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseArpOutput(c.out); got != c.want {
				t.Fatalf("parseArpOutput(%q) = %q, want %q", c.out, got, c.want)
			}
		})
	}
}

// TestMACForAnyFailureReturnsEmpty pins the documented behaviour for the
// one case that is actually reachable in production: a missing/
// unreadable arp binary (standing in for any execution failure, ARP's
// usual blindness from a daemon process included) must return "", never
// an error the caller has to handle, since MACFor's contract is "best
// effort, empty string on any failure".
func TestMACForAnyFailureReturnsEmpty(t *testing.T) {
	restore := arpPath
	arpPath = "/nonexistent/path/to/arp"
	defer func() { arpPath = restore }()

	if got := MACFor(t.Context(), "192.168.0.127"); got != "" {
		t.Fatalf("MACFor() with a missing arp binary = %q, want \"\"", got)
	}
}

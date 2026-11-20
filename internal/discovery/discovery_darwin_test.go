package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// These lines are shaped exactly as this adapter's own --exec format string
// produces them: tab-separated {service_name} {} {txt_uuid} {txt_ty}
// {txt_rp} {txt_pdl}, one line per discovered service.
func TestParseIppfindLine(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		want   Candidate
		wantOK bool
	}{
		{
			name: "well-formed line",
			line: "SpikePrinter\tipp://MacBook-Air-de-Pedro.local:8631/ipp/print\t15f4c408-3a35-3057-5ab6-12023a25f895\tExample Printer\tipp/print\timage/pwg-raster,image/urf",
			want: Candidate{
				ServiceName:  "SpikePrinter",
				URI:          "ipp://MacBook-Air-de-Pedro.local:8631/ipp/print",
				UUID:         "15f4c408-3a35-3057-5ab6-12023a25f895",
				Model:        "Example Printer",
				ResourcePath: "ipp/print",
				PDLs:         []string{"image/pwg-raster", "image/urf"},
				Host:         "MacBook-Air-de-Pedro.local",
				Port:         8631,
			},
			wantOK: true,
		},
		{
			name: "empty uuid field",
			line: "Office Printer\tipp://printer.local:631/ipp/print\t\tHP LaserJet\tipp/print\timage/pwg-raster",
			want: Candidate{
				ServiceName:  "Office Printer",
				URI:          "ipp://printer.local:631/ipp/print",
				UUID:         "",
				Model:        "HP LaserJet",
				ResourcePath: "ipp/print",
				PDLs:         []string{"image/pwg-raster"},
				Host:         "printer.local",
				Port:         631,
			},
			wantOK: true,
		},
		{
			name: "service name with a space",
			line: "Pedro's Office Printer\tipps://printer2.local:443/ipp/print\tuuid-123\tCanon\tipp/print\timage/urf",
			want: Candidate{
				ServiceName:  "Pedro's Office Printer",
				URI:          "ipps://printer2.local:443/ipp/print",
				UUID:         "uuid-123",
				Model:        "Canon",
				ResourcePath: "ipp/print",
				PDLs:         []string{"image/urf"},
				Host:         "printer2.local",
				Port:         443,
			},
			wantOK: true,
		},
		{
			name:   "malformed line is skipped, not zero-valued",
			line:   "this is not a tab-separated ippfind line at all",
			want:   Candidate{},
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseIppfindLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("parseIppfindLine(%q) ok = %v, want %v", c.line, ok, c.wantOK)
			}
			if ok && !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseIppfindLine(%q) = %+v, want %+v", c.line, got, c.want)
			}
		})
	}
}

// TestBrowseIppfindExitHandling pins the distinction Browse must make
// between ippfind's documented "found nothing" exit status and every other
// failure mode. Before this test existed, any non-zero exit from ippfind —
// a bad option, a resolution failure, a missing binary — was silently
// treated identically to "no printers on the network".
func TestBrowseIppfindExitHandling(t *testing.T) {
	cases := []struct {
		name       string
		ippfindBin string // overrides the package's ippfindPath for this case
		wantErr    bool
	}{
		{
			name:       "ippfind exit code 1 (documented: no expression matched) is not an error",
			ippfindBin: writeStubBinary(t, 1),
			wantErr:    false,
		},
		{
			name:       "ippfind exit code 2 (documented: browsing/query/resolution failed) is an error",
			ippfindBin: writeStubBinary(t, 2),
			wantErr:    true,
		},
		{
			name:       "ippfind binary missing fails to start and is an error",
			ippfindBin: nonexistentBinaryPath(t),
			wantErr:    true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore := ippfindPath
			ippfindPath = c.ippfindBin
			defer func() { ippfindPath = restore }()

			d := NewDarwinDiscoverer()
			candidates, err := d.Browse(context.Background(), time.Second)

			if c.wantErr && err == nil {
				t.Fatalf("Browse() error = nil, want a non-nil error")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("Browse() error = %v, want nil", err)
			}
			if len(candidates) != 0 {
				t.Fatalf("Browse() candidates = %+v, want none", candidates)
			}
		})
	}
}

// writeStubBinary writes a tiny shell script that ignores all arguments and
// exits with the given code, standing in for ippfind for one test case.
func writeStubBinary(t *testing.T, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ippfind-stub")
	script := fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return path
}

// nonexistentBinaryPath returns a path that is guaranteed not to exist, so
// exec fails before any process is even started (and thus before any exit
// code exists).
func nonexistentBinaryPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-such-ippfind-binary")
}

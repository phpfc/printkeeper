package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/store"
)

// fakeStrategy is a Strategy double whose outcome is fixed by the test
// and whose call count can be observed, so a test can assert that a
// strategy after the first hit was never invoked at all — not just that
// its result was ignored.
type fakeStrategy struct {
	name       string
	candidates []discovery.Candidate
	err        error
	calls      *int
}

func (f fakeStrategy) Name() string { return f.name }

func (f fakeStrategy) Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.candidates, f.err
}

// TestResolveStopsAtFirstStrategyWithCandidates pins the core ordering
// contract: Resolve tries strategies in order and stops at the first one
// that yields a candidate. A fake whose second strategy succeeds proves
// this two ways at once — the returned candidates come from strategy two,
// not any later one, and strategies three and four are never called at
// all (their call counters stay at zero), not merely "called but
// ignored". Attempt must record exactly the two strategies that actually
// ran.
func TestResolveStopsAtFirstStrategyWithCandidates(t *testing.T) {
	var thirdCalls, fourthCalls int
	found := []discovery.Candidate{{URI: "ipp://10.0.0.5:631/ipp/print"}}

	strategies := []Strategy{
		fakeStrategy{name: "first"},
		fakeStrategy{name: "second", candidates: found},
		fakeStrategy{name: "third", calls: &thirdCalls},
		fakeStrategy{name: "fourth", calls: &fourthCalls},
	}

	got, attempts := Resolve(context.Background(), strategies, store.Adopted{}, "net1")

	if !reflect.DeepEqual(got, found) {
		t.Fatalf("Resolve() candidates = %+v, want %+v", got, found)
	}
	if len(attempts) != 2 {
		t.Fatalf("Resolve() recorded %d attempts, want exactly 2 (must stop at the first hit)", len(attempts))
	}
	if attempts[0].Strategy != "first" || attempts[1].Strategy != "second" {
		t.Fatalf("Resolve() attempts = %+v, want [first, second] in order", attempts)
	}
	if thirdCalls != 0 || fourthCalls != 0 {
		t.Fatalf("strategies after the first hit were invoked (third=%d, fourth=%d); Resolve must stop calling strategies once one succeeds", thirdCalls, fourthCalls)
	}
}

// presentPrinterStub starts an HTTP server that answers every IPP request
// with a minimal, healthy Get-Printer-Attributes response, standing in
// for a real device so lastKnownGood's "probe reports present" path can
// be exercised without a network.
func presentPrinterStub(t *testing.T) string {
	t.Helper()
	msg := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 1)
	msg.Printer.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(3))) // idle
	msg.Printer.Add(goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String("none")))

	payload, err := msg.EncodeBytes()
	if err != nil {
		t.Fatalf("encode fixture response: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", goipp.ContentType)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return "ipp://" + srv.Listener.Addr().String() + "/ipp/print"
}

// TestLastKnownGoodReturnsCandidateWhenProbePresent pins strategy 1 of
// §6.4: given a stored address that still answers, lastKnownGood probes
// it directly and returns exactly that address as a candidate — a cache
// hit, not a rediscovery.
func TestLastKnownGoodReturnsCandidateWhenProbePresent(t *testing.T) {
	uri := presentPrinterStub(t)
	a := store.Adopted{
		Fingerprint: identity.Fingerprint{UUID: "abc-123", ServiceName: "Office Printer"},
		LastSeen: map[string]store.Seen{
			"home": {URI: uri, At: time.Now()},
		},
	}

	candidates, err := lastKnownGood{}.Find(context.Background(), a, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("Find() = %d candidates, want exactly 1", len(candidates))
	}
	if candidates[0].URI != uri {
		t.Fatalf("Find() candidate URI = %q, want %q", candidates[0].URI, uri)
	}
	if candidates[0].UUID != a.Fingerprint.UUID {
		t.Fatalf("Find() candidate UUID = %q, want the stored fingerprint's %q", candidates[0].UUID, a.Fingerprint.UUID)
	}
	wantHost, wantPort := discoveryHostPort(uri)
	if candidates[0].Host != wantHost || candidates[0].Port != wantPort {
		t.Fatalf("Find() candidate Host/Port = %q/%d, want %q/%d", candidates[0].Host, candidates[0].Port, wantHost, wantPort)
	}
}

// TestLastKnownGoodReturnsNoCandidatesWhenKeyAbsent is the brief's
// explicit requirement: a network the printer has never been seen on
// yields no candidates and no error — there is nothing to retry, and
// that absence is not itself a failure.
func TestLastKnownGoodReturnsNoCandidatesWhenKeyAbsent(t *testing.T) {
	a := store.Adopted{LastSeen: map[string]store.Seen{}}

	candidates, err := lastKnownGood{}.Find(context.Background(), a, "unknown-network")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates", candidates)
	}
}

// TestLastKnownGoodReturnsNoCandidatesWhenProbeFails proves the strategy
// never fabricates a candidate for an address that no longer answers: a
// dead last-known-good address must fall through to the next strategy,
// not be reported as found.
func TestLastKnownGoodReturnsNoCandidatesWhenProbeFails(t *testing.T) {
	a := store.Adopted{
		LastSeen: map[string]store.Seen{
			// Port 1 on loopback is never listening.
			"home": {URI: "ipp://127.0.0.1:1/ipp/print", At: time.Now()},
		},
	}

	candidates, err := lastKnownGood{}.Find(context.Background(), a, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil (a dead address is not a ladder-level failure)", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates for a dead address", candidates)
	}
}

// TestBonjourBrowseWrapsDiscoverer pins that the strategy is a thin
// pass-through to whatever Discoverer it was built with.
func TestBonjourBrowseWrapsDiscoverer(t *testing.T) {
	want := []discovery.Candidate{{URI: "ipp://10.0.0.9:631/ipp/print"}}
	s := bonjourBrowse{discoverer: fakeDiscoverer{candidates: want}}

	got, err := s.Find(context.Background(), store.Adopted{}, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Find() = %+v, want %+v", got, want)
	}
	if s.Name() != "bonjourBrowse" {
		t.Fatalf("Name() = %q, want %q", s.Name(), "bonjourBrowse")
	}
}

// fakeDiscoverer is a discovery.Discoverer double for bonjourBrowse's
// unit test.
type fakeDiscoverer struct {
	candidates []discovery.Candidate
	err        error
}

func (f fakeDiscoverer) Browse(ctx context.Context, timeout time.Duration) ([]discovery.Candidate, error) {
	return f.candidates, f.err
}

// TestSNMPIdentityReportsNotAttempted pins the "what matters most" rule
// from the task brief: SNMP is a stub, and a stub must say so honestly —
// ErrNotAttempted, not a silent empty result indistinguishable from
// "looked and found nothing".
func TestSNMPIdentityReportsNotAttempted(t *testing.T) {
	candidates, err := snmpIdentity{}.Find(context.Background(), store.Adopted{}, "home")
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates", candidates)
	}
	if !errors.Is(err, ErrNotAttempted) {
		t.Fatalf("Find() error = %v, want ErrNotAttempted", err)
	}
	if (snmpIdentity{}).Name() != "snmp" {
		t.Fatalf("Name() = %q, want %q", (snmpIdentity{}).Name(), "snmp")
	}
}

// fakeHostResolver is a hostResolver double so unicastHostname's test
// needs no DNS at all, real or faked over the wire.
type fakeHostResolver struct {
	addrs []net.IPAddr
	err   error
}

func (f fakeHostResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f.addrs, f.err
}

// TestUnicastHostnameReturnsResolvedAddressWithOriginalHostnamePreserved
// pins strategy 2 of §6.4 — the one that recovers a printer reachable
// only by an internal hostname. Given a stored URI naming an internal
// hostname and a resolver that maps it to a new address, the returned
// candidate's URI must carry the new address while Host keeps the
// original hostname: the hostname is the durable identity, the address
// is what changed.
func TestUnicastHostnameReturnsResolvedAddressWithOriginalHostnamePreserved(t *testing.T) {
	a := store.Adopted{
		LastSeen: map[string]store.Seen{
			"corp": {URI: "ipp://printer.internal.corp:631/ipp/print", At: time.Now()},
		},
	}
	s := unicastHostname{resolver: fakeHostResolver{
		addrs: []net.IPAddr{{IP: net.ParseIP("10.20.30.40")}},
	}}

	candidates, err := s.Find(context.Background(), a, "corp")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("Find() = %d candidates, want exactly 1", len(candidates))
	}
	got := candidates[0]
	if got.URI != "ipp://10.20.30.40:631/ipp/print" {
		t.Fatalf("Find() candidate URI = %q, want the resolved address with scheme/port/path preserved", got.URI)
	}
	if got.Host != "printer.internal.corp" {
		t.Fatalf("Find() candidate Host = %q, want the original hostname preserved", got.Host)
	}
}

// TestUnicastHostnameNXDOMAINYieldsNoCandidatesNoError pins the brief's
// explicit requirement: a hostname that no longer resolves at all is not
// a ladder-level failure, just nothing this strategy can offer.
func TestUnicastHostnameNXDOMAINYieldsNoCandidatesNoError(t *testing.T) {
	a := store.Adopted{
		LastSeen: map[string]store.Seen{
			"corp": {URI: "ipp://printer.internal.corp:631/ipp/print", At: time.Now()},
		},
	}
	s := unicastHostname{resolver: fakeHostResolver{
		err: &net.DNSError{Err: "no such host", Name: "printer.internal.corp", IsNotFound: true},
	}}

	candidates, err := s.Find(context.Background(), a, "corp")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates", candidates)
	}
}

// TestUnicastHostnameLinkLocalOnlyYieldsNoCandidates pins the IPv6 trap
// measured against the real Epson: EPSON00973D.local resolves to
// link-local IPv6 addresses only (no A record, and no global AAAA
// either in that measurement). A link-local address needs a zone
// identifier that would not survive being persisted to disk and would
// not mean the same interface on a future run — so it is not a usable
// candidate, and this must fall through to the next strategy rather than
// hand back an address that can never be dialled again.
func TestUnicastHostnameLinkLocalOnlyYieldsNoCandidates(t *testing.T) {
	a := store.Adopted{
		LastSeen: map[string]store.Seen{
			"home": {URI: "ipp://EPSON00973D.local:631/ipp/print", At: time.Now()},
		},
	}
	s := unicastHostname{resolver: fakeHostResolver{
		addrs: []net.IPAddr{{IP: net.ParseIP("fe80::e2bb:9eff:fe00:973d"), Zone: "en0"}},
	}}

	candidates, err := s.Find(context.Background(), a, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates for a link-local-only result", candidates)
	}
}

// TestUnicastHostnamePrefersGlobalAddressOverLinkLocal pins the other
// half of the same decision: when a hostname resolves to both a
// link-local address and a usable global one — exactly what
// EPSON00973D.local does today, once its IPv4 address is included — the
// strategy must pick the global address, not whichever one the resolver
// happened to list first.
func TestUnicastHostnamePrefersGlobalAddressOverLinkLocal(t *testing.T) {
	a := store.Adopted{
		LastSeen: map[string]store.Seen{
			"home": {URI: "ipp://EPSON00973D.local:631/ipp/print", At: time.Now()},
		},
	}
	s := unicastHostname{resolver: fakeHostResolver{
		addrs: []net.IPAddr{
			{IP: net.ParseIP("fe80::e2bb:9eff:fe00:973d"), Zone: "en0"},
			{IP: net.ParseIP("192.168.0.127")},
		},
	}}

	candidates, err := s.Find(context.Background(), a, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("Find() = %d candidates, want exactly 1", len(candidates))
	}
	if candidates[0].URI != "ipp://192.168.0.127:631/ipp/print" {
		t.Fatalf("Find() candidate URI = %q, want the global IPv4 address, not the link-local IPv6 one", candidates[0].URI)
	}
}

// TestUnicastHostnameNoLastSeenYieldsNoCandidates: nothing stored for
// this network means nothing to re-resolve.
func TestUnicastHostnameNoLastSeenYieldsNoCandidates(t *testing.T) {
	s := unicastHostname{resolver: fakeHostResolver{}}
	candidates, err := s.Find(context.Background(), store.Adopted{LastSeen: map[string]store.Seen{}}, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates", candidates)
	}
}

// TestParseSearchDomain pins the pure parser behind unicastDNSSD's
// "does the system have a search domain configured" check against
// recorded /etc/resolv.conf-shaped content, never a real file read in
// the test itself.
func TestParseSearchDomain(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			name: "macOS-generated resolv.conf with a search domain",
			data: "#\n# macOS Notice\n#\nsearch lan\nnameserver 8.8.8.8\n",
			want: "lan",
		},
		{
			name: "multiple search domains uses the first",
			data: "search corp.example.com eng.example.com\nnameserver 10.0.0.1\n",
			want: "corp.example.com",
		},
		{
			name: "no search line configured",
			data: "nameserver 8.8.8.8\n",
			want: "",
		},
		{
			name: "empty file",
			data: "",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseSearchDomain([]byte(c.data)); got != c.want {
				t.Fatalf("parseSearchDomain(%q) = %q, want %q", c.data, got, c.want)
			}
		})
	}
}

// TestUnicastDNSSDReportsNotAttemptedWithoutSearchDomain pins the other
// "honest not-attempted" case the task brief calls out: when the system
// has no configured search domain, there is no unicast domain to query,
// and the strategy must say so distinctly from "queried and found
// nothing".
func TestUnicastDNSSDReportsNotAttemptedWithoutSearchDomain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatalf("write fixture resolv.conf: %v", err)
	}

	s := unicastDNSSD{resolvConfPath: path}
	candidates, err := s.Find(context.Background(), store.Adopted{}, "home")
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates", candidates)
	}
	if !errors.Is(err, ErrNotAttempted) {
		t.Fatalf("Find() error = %v, want ErrNotAttempted", err)
	}
}

// TestUnicastDNSSDQueriesConfiguredDomain pins that, once a search
// domain is configured, the strategy actually runs ippfind against a
// query scoped to that domain (using the regtype.domain. syntax ippfind
// documents for unicast DNS-SD — see the package doc comment above
// unicastDNSSD for why this is not the --domain flag the brief's prose
// names) and parses whatever it prints back into candidates.
//
// This asserts two separate things on purpose: that Find parses the
// stub's output into the expected candidate, and — by recording the
// stub's own argv — that the configured domain actually reached both
// registration types ippfind was invoked with. The first assertion alone
// cannot fail against a regression that hardcoded "local.", dropped the
// domain interpolation entirely, or queried only one of _ipp._tcp/
// _ipps._tcp: a stub that ignores its arguments and always prints the
// same fixture would pass either way. Recording argv is what makes the
// test able to fail on exactly the thing its name claims to cover.
func TestUnicastDNSSDQueriesConfiguredDomain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("search corp.example.com\n"), 0o644); err != nil {
		t.Fatalf("write fixture resolv.conf: %v", err)
	}

	stub, argsPath := writeDNSSDStubBinary(t, 0, "Corp Printer\tipp://printer.corp.example.com:631/ipp/print\tuuid-1\tHP\tipp/print\timage/urf\n")

	s := unicastDNSSD{resolvConfPath: path, ippfindPath: stub}
	candidates, err := s.Find(context.Background(), store.Adopted{}, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil", err)
	}
	if len(candidates) != 1 || candidates[0].URI != "ipp://printer.corp.example.com:631/ipp/print" {
		t.Fatalf("Find() = %+v, want the one candidate from the stub's output", candidates)
	}

	gotArgs, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read recorded ippfind argv: %v", err)
	}
	args := string(gotArgs)
	if !strings.Contains(args, "_ipp._tcp.corp.example.com.") {
		t.Fatalf("ippfind was invoked with argv %q, want it to contain %q", args, "_ipp._tcp.corp.example.com.")
	}
	if !strings.Contains(args, "_ipps._tcp.corp.example.com.") {
		t.Fatalf("ippfind was invoked with argv %q, want it to contain %q", args, "_ipps._tcp.corp.example.com.")
	}
}

// TestUnicastDNSSDExitCodeOneIsNotAnError mirrors
// TestBrowseIppfindExitHandling from the discovery package: ippfind's
// documented "no expression matched" exit status is the normal shape of
// "found nothing in that domain", not a failure.
func TestUnicastDNSSDExitCodeOneIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("search corp.example.com\n"), 0o644); err != nil {
		t.Fatalf("write fixture resolv.conf: %v", err)
	}

	stub, _ := writeDNSSDStubBinary(t, 1, "")
	s := unicastDNSSD{resolvConfPath: path, ippfindPath: stub}

	candidates, err := s.Find(context.Background(), store.Adopted{}, "home")
	if err != nil {
		t.Fatalf("Find() error = %v, want nil (exit 1 means nothing matched)", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("Find() = %+v, want no candidates", candidates)
	}
}

// writeDNSSDStubBinary writes a shell script standing in for ippfind: it
// records the exact argv it was invoked with to argsPath (one argument
// per line, via "$@", so a test can assert on what the strategy actually
// passed rather than trusting that it passed the right thing), cats
// out's exact bytes (tabs and all — passing them through a shell escape
// would mangle them) to stdout, and exits with code.
func writeDNSSDStubBinary(t *testing.T, code int, out string) (scriptPath, argsPath string) {
	t.Helper()
	dir := t.TempDir()

	dataPath := filepath.Join(dir, "stub-output")
	if err := os.WriteFile(dataPath, []byte(out), 0o644); err != nil {
		t.Fatalf("write stub output fixture: %v", err)
	}

	argsPath = filepath.Join(dir, "stub-args")

	scriptPath = filepath.Join(dir, "ippfind-dnssd-stub")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat %q\nexit %d\n", argsPath, dataPath, code)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return scriptPath, argsPath
}

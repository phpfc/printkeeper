package resolve

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/store"
)

// dnsLookupTimeout bounds how long unicastHostname waits for a DNS
// answer, so a dead or unreachable resolver cannot stall the whole
// ladder.
const dnsLookupTimeout = 5 * time.Second

// hostResolver is the one method unicastHostname needs from *net.Resolver.
// *net.Resolver satisfies it already — that is what production uses — so
// this interface exists purely to let a test substitute a fake instead
// of standing up a real (or fake) DNS server; the search-domain-honouring
// behaviour the brief asks for comes for free from *net.Resolver itself
// and needs no code here to reproduce.
type hostResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// unicastHostname is strategy 2 of design doc §6.4, and the one that
// answers Pedro's actual complaint: when a printer is reachable only by
// an internal hostname, Bonjour never crosses the subnet to find it, but
// the hostname itself may still resolve through ordinary unicast DNS.
// This strategy re-resolves the hostname from the last-known-good URI
// and, if it now points somewhere new, offers that address as a
// candidate.
type unicastHostname struct {
	// resolver is injected for tests; nil means net.DefaultResolver.
	resolver hostResolver
}

func (unicastHostname) Name() string { return "unicastHostname" }

// Find resolves the hostname embedded in a.LastSeen[netKey]'s URI and, if
// it now answers at a usable address, returns a candidate built from
// that address with the scheme, port and resource path of the original
// URI preserved and Host kept as the original hostname — the hostname is
// the durable identity being re-resolved, the address is what changed.
//
// Three cases deliberately yield no candidates and no error, none of
// them a ladder-level failure:
//
//   - no stored URI for this network (nothing to re-resolve);
//   - the hostname no longer resolves at all (NXDOMAIN and friends);
//   - every address it resolves to is link-local.
//
// That last case is the trap measured against the real printer this
// project exists for: EPSON00973D.local resolves over mDNS to a
// link-local IPv6 address (and, depending on when the responder last
// announced it, a global IPv6 and/or an IPv4 address alongside it). A
// link-local address needs a zone identifier (RFC 4007, e.g. "%en0") to
// mean anything — one that is specific to a single interface on a single
// run of the machine, and that would stop meaning the same thing the
// next time printkeeper starts, let alone be useful persisted to disk.
// So a link-local-only result is treated exactly like no result at all,
// and a global address is preferred whenever one is available alongside
// a link-local one.
func (u unicastHostname) Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error) {
	seen, ok := a.LastSeen[netKey]
	if !ok {
		return nil, nil
	}

	parsed, err := url.Parse(seen.URI)
	if err != nil {
		return nil, nil
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return nil, nil
	}

	resolver := u.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	lookupCtx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()

	addrs, err := resolver.LookupIPAddr(lookupCtx, hostname)
	if err != nil {
		// NXDOMAIN, no-such-host, a resolver timeout: this strategy has
		// nothing to offer, but that is not a ladder-level failure — the
		// next strategy still gets its turn.
		return nil, nil
	}

	addr := bestUnicastAddress(addrs)
	if addr == "" {
		return nil, nil
	}

	return []discovery.Candidate{{
		URI:          rebuildURIWithAddress(parsed, addr),
		Host:         hostname,
		Port:         portOf(parsed),
		UUID:         a.Fingerprint.UUID,
		ServiceName:  a.Fingerprint.ServiceName,
		ResourcePath: a.Fingerprint.ResourcePath,
		PDLs:         a.Fingerprint.PDLs,
	}}, nil
}

// bestUnicastAddress picks the address a re-resolved hostname should be
// dialled at: the first global (non-link-local, non-loopback,
// non-multicast) IPv4 address if there is one, otherwise the first such
// IPv6 address, otherwise "" when every candidate was link-local. This
// mirrors probe.HasNetwork's own definition of "usable" (a
// 169.254.0.0/16 or fe80::/10 address is not a route to anything), kept
// as its own small copy here rather than imported: probe's version
// answers "does this machine have a network", not "is this specific
// resolved address dialable", and the two must be free to diverge.
// IPv4 is preferred over IPv6 only as a tie-break when both are global —
// this codebase and the devices it talks to are IPv4-first in practice —
// never as a reason to prefer a link-local IPv6 over the alternative.
func bestUnicastAddress(addrs []net.IPAddr) string {
	var globalV4, globalV6 string
	for _, a := range addrs {
		ip := a.IP
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		if ip.To4() != nil {
			if globalV4 == "" {
				globalV4 = ip.String()
			}
		} else if globalV6 == "" {
			globalV6 = ip.String()
		}
	}
	if globalV4 != "" {
		return globalV4
	}
	return globalV6
}

// rebuildURIWithAddress returns original's URI with its host replaced by
// addr (bracketed if addr is IPv6), keeping scheme, port and resource
// path untouched.
func rebuildURIWithAddress(original *url.URL, addr string) string {
	rebuilt := *original
	host := addr
	if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
		host = "[" + addr + "]"
	}
	if port := original.Port(); port != "" {
		host = host + ":" + port
	}
	rebuilt.Host = host
	return rebuilt.String()
}

// portOf returns u's port as an int, or 0 if absent or unparseable.
func portOf(u *url.URL) int {
	port, _ := strconv.Atoi(u.Port())
	return port
}

// unicastDNSSDIppfindPath is the absolute path to the system's ippfind
// binary, used by unicastDNSSD. A var, not a const, and distinct from
// discovery's own ippfindPath so the two strategies' tests cannot step
// on each other's overrides.
var unicastDNSSDIppfindPath = "/usr/bin/ippfind"

// defaultResolvConfPath is where unicastDNSSD looks for a configured DNS
// search domain in production.
const defaultResolvConfPath = "/etc/resolv.conf"

// dnssdIppfindFormat mirrors discovery_darwin.go's ippfindFormat
// deliberately: one tab-separated line per discovered service, in the
// same field order, so a line from either package's invocation of
// ippfind parses the same way. Duplicated rather than imported to keep
// this package decoupled from discovery's internals over a handful of
// lines of formatting.
const dnssdIppfindFormat = "{service_name}\t{}\t{txt_uuid}\t{txt_ty}\t{txt_rp}\t{txt_pdl}"

// unicastDNSSD is strategy 4 of design doc §6.4: unicast DNS-SD against
// a configured search domain, for printers advertised in a corporate
// DNS zone rather than over mDNS.
//
// The brief's prose describes this as running "ippfind --domain
// <search-domain>", but ippfind's own --domain flag is a result filter —
// "true if the [already-found] service's domain matches this regex" —
// not a way to direct ippfind to query a given domain over unicast DNS
// (confirmed against the installed ippfind(1) manual and by running it:
// `ippfind --domain lan _ipp._tcp` filters an mDNS browse of "local."
// down to nothing, it does not query "lan" at all). The syntax ippfind's
// own SYNOPSIS documents for this is appending the domain to the
// registration type itself — "_ipp._tcp.<domain>." — which is what this
// strategy actually sends.
type unicastDNSSD struct {
	// resolvConfPath is injected for tests; "" means
	// defaultResolvConfPath.
	resolvConfPath string
	// ippfindPath is injected for tests; "" means
	// unicastDNSSDIppfindPath.
	ippfindPath string
}

func (unicastDNSSD) Name() string { return "unicastDNSSD" }

// Find looks up the system's configured DNS search domain and, only if
// one is configured, queries ippfind for IPP/IPPS services under that
// domain over unicast DNS-SD. No configured domain means there is
// nothing to query, reported as ErrNotAttempted rather than a plain
// empty result — the CLI must be able to say "no search domain
// configured" rather than the misleading "searched and found nothing".
func (s unicastDNSSD) Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error) {
	path := s.resolvConfPath
	if path == "" {
		path = defaultResolvConfPath
	}

	domain := readSearchDomain(path)
	if domain == "" {
		return nil, ErrNotAttempted
	}

	ippfindPath := s.ippfindPath
	if ippfindPath == "" {
		ippfindPath = unicastDNSSDIppfindPath
	}

	out, err := runIppfindDomain(ctx, ippfindPath, domain, defaultBrowseTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isIppfindNothingFoundExitCode(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ippfind against domain %s: %w", domain, err)
	}

	var candidates []discovery.Candidate
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if c, ok := parseDNSSDLine(scanner.Text()); ok {
			candidates = append(candidates, c)
		}
	}
	return candidates, nil
}

// readSearchDomain reads path (in production, /etc/resolv.conf) and
// returns its first configured search domain, or "" if none is
// configured or the file cannot be read. /etc/resolv.conf's "search"
// line has the identical format on Linux and macOS — despite the notice
// macOS itself writes into the file claiming it is not consulted for
// resolution, it is kept in sync with the search domain actually
// configured via System Preferences/scutil, and reading it needs no
// locale-sensitive command-output parsing the way scutil's own output
// would.
func readSearchDomain(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return parseSearchDomain(data)
}

// parseSearchDomain is the pure parser behind readSearchDomain, fed
// recorded file content in tests so it needs no real file or network.
// It returns the first domain named on the first line beginning with
// "search", or "" if no such line exists.
func parseSearchDomain(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "search" {
			continue
		}
		return fields[1]
	}
	return ""
}

// runIppfindDomain shells out to ippfind, querying _ipp._tcp and
// _ipps._tcp under domain via unicast DNS-SD.
func runIppfindDomain(ctx context.Context, ippfindPath, domain string, timeout time.Duration) (string, error) {
	secs := int(timeout.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}

	cmd := exec.CommandContext(ctx, ippfindPath,
		fmt.Sprintf("_ipp._tcp.%s.", domain),
		fmt.Sprintf("_ipps._tcp.%s.", domain),
		"-T", strconv.Itoa(secs),
		"--exec", "echo", dnssdIppfindFormat, ";",
	)
	out, err := cmd.Output()
	return string(out), err
}

// isIppfindNothingFoundExitCode mirrors discovery_darwin.go's
// isIppfindNothingFound: ippfind(1) documents exit code 1 as "no
// expression matched", the normal shape of "nothing in that domain" —
// every other exit code, or an error that is not even an *exec.ExitError
// because the process never started, is a real failure.
func isIppfindNothingFoundExitCode(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// parseDNSSDLine parses one line produced by dnssdIppfindFormat into a
// Candidate, identically to discovery_darwin.go's parseIppfindLine (see
// dnssdIppfindFormat's doc comment for why this is a deliberate small
// duplication rather than a shared import).
func parseDNSSDLine(line string) (discovery.Candidate, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) != 6 {
		return discovery.Candidate{}, false
	}

	serviceName := fields[0]
	rawURI := fields[1]
	uuid := fields[2]
	model := fields[3]
	resourcePath := fields[4]
	pdl := fields[5]

	if serviceName == "" || rawURI == "" {
		return discovery.Candidate{}, false
	}

	u, err := url.Parse(rawURI)
	if err != nil || u.Hostname() == "" {
		return discovery.Candidate{}, false
	}

	var pdls []string
	if pdl != "" {
		pdls = strings.Split(pdl, ",")
	}

	return discovery.Candidate{
		ServiceName:  serviceName,
		URI:          rawURI,
		UUID:         uuid,
		Model:        model,
		ResourcePath: resourcePath,
		PDLs:         pdls,
		Host:         u.Hostname(),
		Port:         portOf(u),
	}, true
}

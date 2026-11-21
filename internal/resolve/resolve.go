// Package resolve answers the question discovery alone cannot: when an
// adopted printer has gone quiet, where is it now? discovery.Discoverer
// only ever looks on the current network segment via Bonjour — exactly
// the thing that goes silent the moment a printer sits behind a VPN, a
// different subnet, or is reachable only by an internal hostname someone
// typed into Add Printer by hand once and never again. This package
// orders every way printkeeper knows to re-find an address, cheapest and
// safest first (design doc §6.4), and runs them until one works.
package resolve

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/store"
)

// ErrNotAttempted marks a Strategy.Find call that deliberately chose not
// to run at all — its precondition was not met (no search domain
// configured, SNMP not yet implemented) — as distinct from a strategy
// that ran and simply found nothing. Resolve never treats this as a
// ladder-level failure: it is recorded on the Attempt exactly like any
// other outcome and the next strategy still gets its turn. The
// distinction exists so the CLI can say "SNMP: not yet implemented"
// rather than a misleading "SNMP: found nothing", which would read as
// "we looked and your printer truly isn't there" when in truth nobody
// looked at all.
var ErrNotAttempted = errors.New("strategy not attempted")

// defaultBrowseTimeout bounds how long bonjourBrowse and unicastDNSSD
// each wait for services to answer. It is deliberately short: these are
// two of five strategies in a ladder that must still have time left for
// the strategies after them.
const defaultBrowseTimeout = 5 * time.Second

// defaultProbeTimeout bounds how long lastKnownGood waits for a direct
// IPP round trip to the last address an adopted printer answered at.
const defaultProbeTimeout = 5 * time.Second

// Strategy is one way of trying to find an adopted printer's current
// address. Find must never block indefinitely — it is handed a context
// specifically so Resolve can bound the whole ladder — and must return no
// candidates (not a fabricated one) when it has nothing to offer.
type Strategy interface {
	// Name identifies the strategy for the Attempt list the CLI renders
	// to a human. It does not have to match the Go type name: see
	// snmpIdentity, whose Name is "snmp".
	Name() string

	// Find looks for the printer a describes, scoped to the network
	// identified by netKey (as produced by a store.NetworkKeyer). It
	// returns the candidates it found, which may be empty, and an error
	// only for a genuine operational failure — not for "found nothing"
	// and not, per ErrNotAttempted, for "chose not to run".
	Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error)
}

// Attempt records what one Strategy did when Resolve tried it, so a
// caller — ultimately the CLI — can render an honest sentence about what
// was tried even when every strategy failed. Err distinguishes three
// outcomes beyond a plain success: nil with zero Candidates means the
// strategy ran and genuinely found nothing; ErrNotAttempted (test with
// errors.Is) means it never ran at all; any other non-nil error means it
// tried and failed for an operational reason worth surfacing.
type Attempt struct {
	Strategy   string
	Duration   time.Duration
	Candidates int
	Err        error
}

// Resolve tries strategies in order against a on network netKey, and
// stops at the first one that yields at least one candidate — order is
// the design's, cheapest and safest first, so a known-good network is a
// cache hit rather than a rediscovery. It returns the winning strategy's
// candidates (nil if every strategy came up empty) and one Attempt per
// strategy that was tried, in the order they were tried.
func Resolve(ctx context.Context, strategies []Strategy, a store.Adopted, netKey string) ([]discovery.Candidate, []Attempt) {
	attempts := make([]Attempt, 0, len(strategies))

	for _, s := range strategies {
		start := time.Now()
		candidates, err := s.Find(ctx, a, netKey)
		attempts = append(attempts, Attempt{
			Strategy:   s.Name(),
			Duration:   time.Since(start),
			Candidates: len(candidates),
			Err:        err,
		})

		if len(candidates) > 0 {
			return candidates, attempts
		}
	}

	return nil, attempts
}

// DefaultStrategies returns the five strategies of design doc §6.4 in
// their specified order, wired to d for the one strategy (bonjourBrowse)
// that needs a live Bonjour browse.
func DefaultStrategies(d discovery.Discoverer) []Strategy {
	return []Strategy{
		lastKnownGood{},
		unicastHostname{},
		bonjourBrowse{discoverer: d},
		unicastDNSSD{},
		snmpIdentity{},
	}
}

// bonjourBrowse wraps a discovery.Discoverer so the same Bonjour browse
// Task 1 built for initial setup also serves as strategy 3 of the repair
// ladder: same-segment devices answer mDNS regardless of why printkeeper
// is asking.
type bonjourBrowse struct {
	discoverer discovery.Discoverer
}

func (bonjourBrowse) Name() string { return "bonjourBrowse" }

func (b bonjourBrowse) Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error) {
	return b.discoverer.Browse(ctx, defaultBrowseTimeout)
}

// snmpIdentity is a stub: SNMP identity querying is not implemented yet.
// It still takes its place in the ladder and reports ErrNotAttempted
// rather than being omitted, so the Attempt list — and the CLI sentence
// built from it — says "SNMP: not attempted" honestly instead of silently
// pretending this strategy was never part of the design.
type snmpIdentity struct{}

func (snmpIdentity) Name() string { return "snmp" }

func (snmpIdentity) Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error) {
	return nil, ErrNotAttempted
}

// discoveryHostPort pulls the hostname and port back out of a URI for
// populating a Candidate's Host/Port fields, mirroring how
// discovery_darwin.go's parseIppfindLine derives them from an ippfind
// result. An unparseable URI yields a zero value rather than an error:
// Host/Port are secondary fields a caller can live without, never worth
// failing a whole strategy over.
func discoveryHostPort(uri string) (host string, port int) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", 0
	}
	port, _ = strconv.Atoi(u.Port())
	return u.Hostname(), port
}

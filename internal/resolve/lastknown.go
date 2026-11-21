package resolve

import (
	"context"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/store"
)

// lastKnownGood is strategy 1 of design doc §6.4: retry the address this
// printer last answered at on this exact network, directly, before
// trying to rediscover anything. Returning to a known-good network
// should be a cache hit, not a rediscovery — which is also why it is
// tried first.
type lastKnownGood struct{}

func (lastKnownGood) Name() string { return "lastKnownGood" }

// Find probes a.LastSeen[netKey] directly and reports it as the single
// candidate only when the probe says the device is actually present.
// A network this printer has never been seen on (key absent) and a
// known address that no longer answers both return no candidates and no
// error: neither is a ladder-level failure, and both must fall through
// to the next strategy rather than be mistaken for "found".
func (lastKnownGood) Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error) {
	seen, ok := a.LastSeen[netKey]
	if !ok {
		return nil, nil
	}

	result := probe.Probe(ctx, seen.URI, defaultProbeTimeout)
	if result.State != probe.StatePresent {
		return nil, nil
	}

	host, port := discoveryHostPort(seen.URI)

	return []discovery.Candidate{{
		URI:          seen.URI,
		Host:         host,
		Port:         port,
		UUID:         a.Fingerprint.UUID,
		ServiceName:  a.Fingerprint.ServiceName,
		ResourcePath: a.Fingerprint.ResourcePath,
		PDLs:         a.Fingerprint.PDLs,
	}}, nil
}

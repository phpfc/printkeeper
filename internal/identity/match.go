package identity

import (
	"sort"

	"github.com/phpfc/printkeeper/internal/discovery"
)

// Weights for each field a Fingerprint comparison can agree on. A UUID
// match alone outweighs any combination of the others that discovery alone
// can currently produce (ServiceName + ResourcePath = 25), so a confirmed
// UUID always wins the ranking. Do not adjust these: later tasks' repair
// confidence ladder is calibrated against these exact values.
const (
	WeightUUID         = 100
	WeightMAC          = 60
	WeightDeviceID     = 25
	WeightServiceName  = 20
	WeightResourcePath = 5
)

// Scored pairs a discovered Candidate with how well it matched a stored
// Fingerprint.
type Scored struct {
	Candidate discovery.Candidate
	Score     int
}

// Match scores every candidate against a stored Fingerprint and returns the
// results sorted descending by score, so the best match is always first.
//
// Ties are deliberate, not a bug: two candidates that agree with stored on
// exactly the same (possibly empty) set of fields score identically. That
// is what lets a later repair confidence ladder detect "can't tell these
// two printers apart" instead of being handed a single, falsely confident
// winner.
func Match(stored Fingerprint, candidates []discovery.Candidate) []Scored {
	results := make([]Scored, len(candidates))
	for i, c := range candidates {
		results[i] = Scored{
			Candidate: c,
			Score:     score(stored, FromCandidate(c)),
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}

// score is additive over fields where both sides are non-empty and equal.
// An empty field on either side never matches, including against another
// empty field: two unknown values are not evidence of a match.
func score(a, b Fingerprint) int {
	total := 0
	total += fieldScore(normaliseUUID(a.UUID), normaliseUUID(b.UUID), WeightUUID)
	total += fieldScore(a.MAC, b.MAC, WeightMAC)
	total += fieldScore(a.DeviceID, b.DeviceID, WeightDeviceID)
	total += fieldScore(a.ServiceName, b.ServiceName, WeightServiceName)
	total += fieldScore(a.ResourcePath, b.ResourcePath, WeightResourcePath)
	return total
}

// fieldScore returns weight when both values are non-empty and equal, and
// zero otherwise.
func fieldScore(a, b string, weight int) int {
	if a == "" || b == "" {
		return 0
	}
	if a != b {
		return 0
	}
	return weight
}

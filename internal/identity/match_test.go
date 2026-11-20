package identity

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/inspect"
)

// TestWeightUUIDDominatesSumOfOtherWeights pins the invariant a later
// task's repair confidence ladder depends on for safety: ThresholdExact,
// the score at or above which the ladder repairs the queue silently with
// no user notification, is set to WeightUUID. If WeightUUID did not
// strictly exceed every other weight combined, a candidate that matched
// every non-UUID signal (MAC, DeviceID, ServiceName, ResourcePath) but
// genuinely had a different UUID — or no UUID to compare at all — could
// cross that threshold and get silently adopted as the wrong printer. That
// is the design's first anti-goal arriving through arithmetic, not a
// network failure. The sum is written out by name, not as a literal
// constant, so bumping any individual weight re-checks dominance instead
// of comparing against a number that was only ever correct at the moment
// it was typed.
func TestWeightUUIDDominatesSumOfOtherWeights(t *testing.T) {
	sumOfOtherWeights := WeightMAC + WeightDeviceID + WeightServiceName + WeightResourcePath
	if WeightUUID <= sumOfOtherWeights {
		t.Fatalf("WeightUUID (%d) must exceed the sum of every other weight (%d); otherwise a candidate matching everything except the UUID could reach ThresholdExact and be silently adopted as the wrong printer", WeightUUID, sumOfOtherWeights)
	}
}

// TestScoreIsAdditiveOverMatchingNonEmptyFields pins the weight table from
// the brief directly against the internal scoring function: each field
// contributes its weight only when both sides are non-empty and equal,
// scores for distinct fields add, and an empty field on either side never
// manufactures a match against another empty field.
func TestScoreIsAdditiveOverMatchingNonEmptyFields(t *testing.T) {
	tests := []struct {
		name  string
		a, b  Fingerprint
		score int
	}{
		{
			name:  "uuid match alone",
			a:     Fingerprint{UUID: "abc-123"},
			b:     Fingerprint{UUID: "abc-123"},
			score: WeightUUID,
		},
		{
			name:  "mac match alone",
			a:     Fingerprint{MAC: "aa:bb:cc:dd:ee:ff"},
			b:     Fingerprint{MAC: "aa:bb:cc:dd:ee:ff"},
			score: WeightMAC,
		},
		{
			name:  "device id match alone",
			a:     Fingerprint{DeviceID: "MFG:Example;MDL:Printer;"},
			b:     Fingerprint{DeviceID: "MFG:Example;MDL:Printer;"},
			score: WeightDeviceID,
		},
		{
			name:  "service name match alone",
			a:     Fingerprint{ServiceName: "Office Printer"},
			b:     Fingerprint{ServiceName: "Office Printer"},
			score: WeightServiceName,
		},
		{
			name:  "resource path match alone",
			a:     Fingerprint{ResourcePath: "ipp/print"},
			b:     Fingerprint{ResourcePath: "ipp/print"},
			score: WeightResourcePath,
		},
		{
			name:  "device id and service name add",
			a:     Fingerprint{DeviceID: "MFG:Example;", ServiceName: "Office Printer"},
			b:     Fingerprint{DeviceID: "MFG:Example;", ServiceName: "Office Printer"},
			score: WeightDeviceID + WeightServiceName,
		},
		{
			name:  "every field matching adds every weight",
			a:     Fingerprint{UUID: "abc-123", MAC: "aa:bb:cc:dd:ee:ff", DeviceID: "d", ServiceName: "s", ResourcePath: "r"},
			b:     Fingerprint{UUID: "abc-123", MAC: "aa:bb:cc:dd:ee:ff", DeviceID: "d", ServiceName: "s", ResourcePath: "r"},
			score: WeightUUID + WeightMAC + WeightDeviceID + WeightServiceName + WeightResourcePath,
		},
		{
			name:  "both empty never matches",
			a:     Fingerprint{},
			b:     Fingerprint{},
			score: 0,
		},
		{
			name:  "one side empty never matches",
			a:     Fingerprint{UUID: "abc-123"},
			b:     Fingerprint{},
			score: 0,
		},
		{
			name:  "mismatched non-empty values score nothing",
			a:     Fingerprint{UUID: "abc-123", ServiceName: "Office"},
			b:     Fingerprint{UUID: "xyz-999", ServiceName: "Lobby"},
			score: 0,
		},
		{
			name:  "uuid normalised: prefix and case differ but still matches",
			a:     Fingerprint{UUID: "urn:uuid:ABC-123"},
			b:     Fingerprint{UUID: "abc-123"},
			score: WeightUUID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := score(tt.a, tt.b)
			if got != tt.score {
				t.Fatalf("score(%+v, %+v) = %d, want %d", tt.a, tt.b, got, tt.score)
			}
		})
	}
}

// TestFromCandidateNormalisesUUID pins that FromCandidate strips a
// urn:uuid: prefix and lowercases, regardless of which form the source
// carried — Candidate.UUID arrives bare from Bonjour TXT records, but the
// function must not assume that and break the day a caller feeds it an
// IPP-shaped printer-uuid attribute value instead.
func TestFromCandidateNormalisesUUID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare lowercase", "df6c07de-b847-d68d-0f6e-83d3f5195846", "df6c07de-b847-d68d-0f6e-83d3f5195846"},
		{"bare uppercase", "DF6C07DE-B847-D68D-0F6E-83D3F5195846", "df6c07de-b847-d68d-0f6e-83d3f5195846"},
		{"urn-prefixed", "urn:uuid:df6c07de-b847-d68d-0f6e-83d3f5195846", "df6c07de-b847-d68d-0f6e-83d3f5195846"},
		{"urn-prefixed uppercase", "URN:UUID:DF6C07DE-B847-D68D-0F6E-83D3F5195846", "df6c07de-b847-d68d-0f6e-83d3f5195846"},
		{"empty stays empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := FromCandidate(discovery.Candidate{UUID: tt.in})
			if f.UUID != tt.want {
				t.Fatalf("FromCandidate(UUID=%q).UUID = %q, want %q", tt.in, f.UUID, tt.want)
			}
		})
	}
}

// TestMatchRanksUUIDAboveNonUUIDCombination asserts the property the whole
// repair ladder leans on: a candidate that matches on UUID alone must
// outrank one that matches on every other field discovery can currently
// produce (ServiceName and ResourcePath — MAC and DeviceID are never
// populated from a bare Candidate today), and Match returns results sorted
// descending by score.
func TestMatchRanksUUIDAboveNonUUIDCombination(t *testing.T) {
	stored := Fingerprint{UUID: "abc-123", ServiceName: "Office Printer", ResourcePath: "ipp/print"}

	uuidOnly := discovery.Candidate{UUID: "abc-123", ServiceName: "different-service", ResourcePath: "different/path"}
	nonUUIDCombo := discovery.Candidate{UUID: "", ServiceName: "Office Printer", ResourcePath: "ipp/print"}
	noMatch := discovery.Candidate{UUID: "zzz-999", ServiceName: "Nothing", ResourcePath: "nothing"}

	results := Match(stored, []discovery.Candidate{noMatch, nonUUIDCombo, uuidOnly})

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0].Candidate.UUID != "abc-123" {
		t.Fatalf("top result = %+v, want the uuid-matching candidate first", results[0])
	}
	if results[0].Score != WeightUUID {
		t.Fatalf("top score = %d, want %d", results[0].Score, WeightUUID)
	}
	if results[0].Score <= results[1].Score || results[1].Score <= results[2].Score {
		t.Fatalf("results not sorted descending: %+v", results)
	}
}

// TestMatchNeverSeparatesIdenticalCandidatesWithoutDistinguishingFields pins
// the property a later repair-confidence ladder depends on for safety: two
// printers of the same model, neither reporting a UUID, and nothing else
// that distinguishes them from the stored fingerprint, MUST score equally.
// If weighting let this pair resolve to a single confident winner, the
// ladder could silently repair the queue onto the wrong physical printer.
func TestMatchNeverSeparatesIdenticalCandidatesWithoutDistinguishingFields(t *testing.T) {
	// The stored fingerprint predates knowing this printer's UUID.
	stored := Fingerprint{ResourcePath: "ipp/print"}

	twinA := discovery.Candidate{
		ServiceName:  "Printer (2)",
		Model:        "Example Printer",
		ResourcePath: "ipp/print",
		Host:         "192.168.1.50",
	}
	twinB := discovery.Candidate{
		ServiceName:  "Printer (3)",
		Model:        "Example Printer",
		ResourcePath: "ipp/print",
		Host:         "192.168.1.51",
	}

	results := Match(stored, []discovery.Candidate{twinA, twinB})

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Score != results[1].Score {
		t.Fatalf("identical twins scored differently: %+v vs %+v — this would let the repair ladder silently pick a winner", results[0], results[1])
	}
	if results[0].Score != WeightResourcePath {
		t.Fatalf("score = %d, want %d (ResourcePath is the only shared, non-empty field)", results[0].Score, WeightResourcePath)
	}
}

// TestMatchFixture loads the Task 1 fixture (a captured, anonymised
// inspect.Report) and checks that a Fingerprint built from its own
// Candidate ranks that exact Candidate first — with a UUID-weighted score
// — against two unrelated candidates. This is the check that the weights
// were designed against measured data, not guessed field shapes.
func TestMatchFixture(t *testing.T) {
	const fixturePath = "../../testdata/fixtures/example-printer.json"

	data, err := os.ReadFile(fixturePath)
	if os.IsNotExist(err) {
		t.Skip("no fixture at testdata/fixtures/example-printer.json; run `go run ./cmd/printkeeper inspect --json` against a reachable printer to produce one")
	}
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var report inspect.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	stored := FromCandidate(report.Candidate)

	unrelated1 := discovery.Candidate{
		ServiceName:  "Unrelated Printer A",
		UUID:         "11111111-1111-1111-1111-111111111111",
		ResourcePath: "ipp/print",
	}
	unrelated2 := discovery.Candidate{
		ServiceName: "Unrelated Printer B",
		UUID:        "22222222-2222-2222-2222-222222222222",
	}

	results := Match(stored, []discovery.Candidate{unrelated1, unrelated2, report.Candidate})

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0].Candidate.ServiceName != report.Candidate.ServiceName || results[0].Candidate.UUID != report.Candidate.UUID {
		t.Fatalf("top result = %+v, want the fixture's own candidate first", results[0])
	}
	if results[0].Score < WeightUUID {
		t.Fatalf("top score = %d, want at least WeightUUID (%d)", results[0].Score, WeightUUID)
	}
	if results[0].Score <= results[1].Score {
		t.Fatalf("fixture candidate (%d) did not outscore the next result (%d)", results[0].Score, results[1].Score)
	}
}

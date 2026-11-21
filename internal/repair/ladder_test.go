package repair

import (
	"context"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/store"
)

// fixedNow is the reference instant every table case measures elapsed
// time against, so AbandonAfter comparisons are exact and not flaky
// against the real clock.
var fixedNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// recentlySeen is a LastSeen map whose single entry sits comfortably
// inside AbandonAfter of fixedNow, for cases that must not be mistaken
// for abandonment.
func recentlySeen() map[string]store.Seen {
	return map[string]store.Seen{
		"home": {URI: "ipp://192.168.1.50/ipp/print", At: fixedNow.Add(-time.Hour)},
	}
}

// TestDecideLadderTable is one case per row of spec §6.5 plus §11.7. Each
// case is built so it can actually fail: where the expected Action is
// ActionNothing (the ladder's own zero value, and therefore the one
// result a broken implementation could produce by accident), the fed
// matches/LastSeen data would instead demand a different, very much
// non-default action if the State/abandonment check that is supposed to
// short-circuit the case were ever skipped or reordered.
func TestDecideLadderTable(t *testing.T) {
	uuidMatch := identity.Scored{
		Candidate: discovery.Candidate{ServiceName: "Office Printer", UUID: "cfe92100-67c4-11d4-a45f-e0bb9e00973d"},
		Score:     identity.WeightUUID,
	}
	ambiguousPair := []identity.Scored{
		{Candidate: discovery.Candidate{ServiceName: "Printer (2)", Model: "Example"}, Score: 150},
		{Candidate: discovery.Candidate{ServiceName: "Printer (3)", Model: "Example"}, Score: 140},
	}
	partialMatch := identity.Scored{
		Candidate: discovery.Candidate{ServiceName: "Office Printer", Host: "192.168.1.77"},
		Score:     60, // e.g. a MAC-only match: above ThresholdPartial, nowhere near ThresholdExact
	}

	adopted := store.Adopted{
		QueueName:   "Office_Printer",
		Fingerprint: identity.Fingerprint{UUID: "cfe92100-67c4-11d4-a45f-e0bb9e00973d"},
		LastSeen:    recentlySeen(),
	}

	tests := []struct {
		name    string
		matches []identity.Scored
		probe   probe.Result
		adopted store.Adopted
		now     time.Time
		want    Action
	}{
		{
			// Present must win outright. Feeding it an exact UUID match
			// proves this: if the State check were missing or came
			// after the match-based ladder, this would wrongly come
			// back ActionRepairSilently instead of ActionNothing.
			name:    "present with an exact match available is still left alone",
			matches: []identity.Scored{uuidMatch},
			probe:   probe.Result{State: probe.StatePresent},
			adopted: adopted,
			now:     fixedNow,
			want:    ActionNothing,
		},
		{
			// A printer having a bad day has not moved. Feeding it an
			// exact UUID match proves the Errored check isn't just
			// falling through to the same default as "no matches".
			name:    "errored (out of paper, cover open, ...) is left alone, not repaired",
			matches: []identity.Scored{uuidMatch},
			probe:   probe.Result{State: probe.StateErrored},
			adopted: adopted,
			now:     fixedNow,
			want:    ActionNothing,
		},
		{
			// StateUnknown means the machine could not ask — not a
			// finding about the printer. Feeding it an ambiguous pair
			// proves this isn't reaching ActionNothing by accident of
			// the match ladder; if Unknown weren't special-cased this
			// would come back ActionAsk instead.
			name:    "unknown (no network to ask with) takes no action",
			matches: ambiguousPair,
			probe:   probe.Result{State: probe.StateUnknown},
			adopted: adopted,
			now:     fixedNow,
			want:    ActionNothing,
		},
		{
			name:    "absent with a genuine UUID match repairs silently",
			matches: []identity.Scored{uuidMatch},
			probe:   probe.Result{State: probe.StateAbsent},
			adopted: adopted,
			now:     fixedNow,
			want:    ActionRepairSilently,
		},
		{
			name:    "absent with one partial (non-UUID) match repairs and records",
			matches: []identity.Scored{partialMatch},
			probe:   probe.Result{State: probe.StateAbsent},
			adopted: adopted,
			now:     fixedNow,
			want:    ActionRepairAndRecord,
		},
		{
			name:    "absent with two close plausible matches must ask, never guess",
			matches: ambiguousPair,
			probe:   probe.Result{State: probe.StateAbsent},
			adopted: adopted,
			now:     fixedNow,
			want:    ActionAsk,
		},
		{
			name:    "absent with no candidate at all, recently seen elsewhere, reports",
			matches: nil,
			probe:   probe.Result{State: probe.StateAbsent},
			adopted: adopted, // LastSeen is recent — must not be mistaken for abandonment
			now:     fixedNow,
			want:    ActionReport,
		},
		{
			name:    "absent continuously for longer than AbandonAfter lets go",
			matches: nil,
			probe:   probe.Result{State: probe.StateAbsent},
			adopted: store.Adopted{
				QueueName:   "Office_Printer",
				Fingerprint: adopted.Fingerprint,
				LastSeen: map[string]store.Seen{
					"home": {URI: "ipp://192.168.1.50/ipp/print", At: fixedNow.Add(-(AbandonAfter + time.Hour))},
				},
			},
			now:  fixedNow,
			want: ActionLetGo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.adopted, tt.probe, tt.matches, tt.now)
			if d.Action != tt.want {
				t.Fatalf("Decide() = %v, want %v (reason: %q)", d.Action, tt.want, d.Reason)
			}
		})
	}
}

// TestTwoIdenticalModelsWithoutUUIDNeverAutoRepair is Review Focus 1,
// verbatim from the task brief: two printers of the same model, neither
// carrying a UUID, must never resolve to a silent or even a recorded
// single-candidate repair. Anti-goal 1 is printing to the wrong printer,
// and nothing in the protocol (printer-location comes back empty on real
// hardware) would let a guess beat luck.
func TestTwoIdenticalModelsWithoutUUIDNeverAutoRepair(t *testing.T) {
	adoptedWithoutUUID := store.Adopted{
		QueueName:   "Example_Printer",
		Fingerprint: identity.Fingerprint{ResourcePath: "ipp/print"},
		LastSeen:    recentlySeen(),
	}
	absent := probe.Result{State: probe.StateAbsent}
	twoSameModelMatches := []identity.Scored{
		{Candidate: discovery.Candidate{ServiceName: "Printer (2)", Model: "Example Printer", ResourcePath: "ipp/print", Host: "192.168.1.50"}, Score: 45},
		{Candidate: discovery.Candidate{ServiceName: "Printer (3)", Model: "Example Printer", ResourcePath: "ipp/print", Host: "192.168.1.51"}, Score: 45},
	}

	d := Decide(adoptedWithoutUUID, absent, twoSameModelMatches, fixedNow)
	if d.Action != ActionAsk {
		t.Fatalf("got %v; printing to the wrong printer is anti-goal 1", d.Action)
	}
	if len(d.Choices) != 2 {
		t.Fatalf("user must be offered both, got %d", len(d.Choices))
	}
}

// TestExactTierRequiresActualUUIDMatchNotJustScore pins the controller
// ruling that ThresholdExact alone is not sufficient for
// ActionRepairSilently: a Scored entry can only carry a score at or
// above ThresholdExact through an actual UUID match in real usage (no
// other weight combination reaches it), but Decide must not trust the
// number alone — it must also check the UUID itself agrees. This
// simulates what an upstream scoring bug would hand Decide: a high score
// attached to a candidate whose UUID does not actually match the stored
// fingerprint. The ladder must refuse to treat that as a confident
// match; it must fall to the partial tier (recorded, not silent) rather
// than either silently repairing or being fooled into inaction.
func TestExactTierRequiresActualUUIDMatchNotJustScore(t *testing.T) {
	adopted := store.Adopted{
		QueueName:   "Office_Printer",
		Fingerprint: identity.Fingerprint{UUID: "cfe92100-67c4-11d4-a45f-e0bb9e00973d"},
		LastSeen:    recentlySeen(),
	}
	suspicious := identity.Scored{
		Candidate: discovery.Candidate{ServiceName: "Office Printer", UUID: "totally-different-uuid"},
		Score:     identity.WeightUUID, // inflated/wrong — the UUIDs disagree
	}

	d := Decide(adopted, probe.Result{State: probe.StateAbsent}, []identity.Scored{suspicious}, fixedNow)
	if d.Action == ActionRepairSilently {
		t.Fatalf("got ActionRepairSilently for a candidate whose UUID does not actually match the stored fingerprint — score alone must never be trusted for silent repair")
	}
	if d.Action != ActionRepairAndRecord {
		t.Fatalf("got %v, want ActionRepairAndRecord (demoted from the exact tier, not silently adopted and not ignored)", d.Action)
	}
}

// TestTwoMatchesAboveThresholdButFarApartIsNotAmbiguous distinguishes
// "two or more candidates above ThresholdPartial" from the actual
// ambiguity rule: only candidates whose scores are within WeightDeviceID
// of the top score count as equally plausible. A second candidate that
// clears ThresholdPartial but trails far behind the leader must not
// block a confident repair.
func TestTwoMatchesAboveThresholdButFarApartIsNotAmbiguous(t *testing.T) {
	adopted := store.Adopted{QueueName: "Q", Fingerprint: identity.Fingerprint{}, LastSeen: recentlySeen()}
	leader := discovery.Candidate{ServiceName: "Leader"}
	trailer := discovery.Candidate{ServiceName: "Trailer"}
	matches := []identity.Scored{
		{Candidate: leader, Score: 150},
		{Candidate: trailer, Score: 100}, // diff = 50, >= WeightDeviceID (25): not close
	}

	d := Decide(adopted, probe.Result{State: probe.StateAbsent}, matches, fixedNow)
	if d.Action != ActionRepairAndRecord {
		t.Fatalf("got %v, want ActionRepairAndRecord: a distant second match must not force an Ask", d.Action)
	}
	if d.Target.ServiceName != "Leader" {
		t.Fatalf("Target = %+v, want the leading candidate", d.Target)
	}
}

// TestAmbiguityBandIsExactlyWeightDeviceIDNotThresholdPartial pins the
// exact width of the ambiguity band against the one plausible mix-up:
// using ThresholdPartial (40) instead of identity.WeightDeviceID (25)
// as the closeness cutoff. A score gap of 25 sits strictly between the
// two constants, so the two implementations disagree on it — a gap of
// 25 is not "less than 25" (not ambiguous) but is "less than 40" (would
// wrongly read as ambiguous under the mixed-up constant). A gap of 24
// is the mirror case: ambiguous under either constant, but exercising
// the band's inside edge rather than a value far from both.
func TestAmbiguityBandIsExactlyWeightDeviceIDNotThresholdPartial(t *testing.T) {
	adopted := store.Adopted{QueueName: "Q", Fingerprint: identity.Fingerprint{}, LastSeen: recentlySeen()}
	leader := discovery.Candidate{ServiceName: "Leader"}
	trailer := discovery.Candidate{ServiceName: "Trailer"}

	atTheBand := []identity.Scored{
		{Candidate: leader, Score: 125},
		{Candidate: trailer, Score: 100}, // diff = 25: not "less than" WeightDeviceID
	}
	d := Decide(adopted, probe.Result{State: probe.StateAbsent}, atTheBand, fixedNow)
	if d.Action != ActionRepairAndRecord {
		t.Fatalf("gap of exactly WeightDeviceID (25) got %v, want ActionRepairAndRecord — the band is 'less than', not 'at most'", d.Action)
	}
	if d.Target.ServiceName != "Leader" {
		t.Fatalf("Target = %+v, want the leading candidate", d.Target)
	}

	justInsideTheBand := []identity.Scored{
		{Candidate: leader, Score: 125},
		{Candidate: trailer, Score: 101}, // diff = 24: inside the band
	}
	d = Decide(adopted, probe.Result{State: probe.StateAbsent}, justInsideTheBand, fixedNow)
	if d.Action != ActionAsk {
		t.Fatalf("gap of 24 (inside WeightDeviceID) got %v, want ActionAsk", d.Action)
	}
	if len(d.Choices) != 2 {
		t.Fatalf("want both candidates offered, got %d", len(d.Choices))
	}
}

// TestAbandonmentUsesMostRecentSeenAcrossAllNetworks pins the controller
// ruling for §11.7: a printer unseen on one network for a long time but
// recently seen on another network must not be let go. Measuring from
// only one network's entry — or from an arbitrary one — would get this
// wrong in the more dangerous direction (declaring a printer abandoned
// that the user still uses elsewhere).
func TestAbandonmentUsesMostRecentSeenAcrossAllNetworks(t *testing.T) {
	adopted := store.Adopted{
		QueueName:   "Q",
		Fingerprint: identity.Fingerprint{},
		LastSeen: map[string]store.Seen{
			"office": {URI: "ipp://10.0.0.5/ipp/print", At: fixedNow.Add(-(AbandonAfter + 365*24*time.Hour))}, // ancient
			"home":   {URI: "ipp://192.168.1.50/ipp/print", At: fixedNow.Add(-time.Hour)},                     // recent
		},
	}

	d := Decide(adopted, probe.Result{State: probe.StateAbsent}, nil, fixedNow)
	if d.Action != ActionReport {
		t.Fatalf("got %v, want ActionReport: recently seen on another network must not read as abandoned", d.Action)
	}
}

// TestAbandonmentBoundaryIsStrictlyGreaterThan pins the exact boundary:
// "continuously absent for longer than AbandonAfter" means strictly
// greater than, not at-or-equal. Exactly AbandonAfter must still report;
// a hair past it must let go.
func TestAbandonmentBoundaryIsStrictlyGreaterThan(t *testing.T) {
	exactly := store.Adopted{
		QueueName:   "Q",
		Fingerprint: identity.Fingerprint{},
		LastSeen:    map[string]store.Seen{"home": {At: fixedNow.Add(-AbandonAfter)}},
	}
	justOver := store.Adopted{
		QueueName:   "Q",
		Fingerprint: identity.Fingerprint{},
		LastSeen:    map[string]store.Seen{"home": {At: fixedNow.Add(-AbandonAfter - time.Second)}},
	}

	if got := Decide(exactly, probe.Result{State: probe.StateAbsent}, nil, fixedNow).Action; got != ActionReport {
		t.Fatalf("exactly AbandonAfter = %v, want ActionReport (not yet 'longer than')", got)
	}
	if got := Decide(justOver, probe.Result{State: probe.StateAbsent}, nil, fixedNow).Action; got != ActionLetGo {
		t.Fatalf("just over AbandonAfter = %v, want ActionLetGo", got)
	}
}

// TestAbandonmentWithNoSeenHistoryAtAllDoesNotLetGo covers an adopted
// printer whose LastSeen is empty — never successfully probed on any
// network, current or otherwise. There is no timestamp to measure
// "continuously absent" from, and the safer error is to report rather
// than to declare it abandoned on no evidence.
func TestAbandonmentWithNoSeenHistoryAtAllDoesNotLetGo(t *testing.T) {
	adopted := store.Adopted{QueueName: "Q", Fingerprint: identity.Fingerprint{}, LastSeen: map[string]store.Seen{}}

	d := Decide(adopted, probe.Result{State: probe.StateAbsent}, nil, fixedNow)
	if d.Action != ActionReport {
		t.Fatalf("got %v, want ActionReport: no Seen history is not evidence of abandonment", d.Action)
	}
}

// TestSweepSkipsQueuesTheUserNeverAdopted is Review Focus 3: a machine
// can carry CUPS queues the user set up by hand, outside printkeeper
// entirely. Sweep must never produce a decision — and therefore never
// act — on one of those, even though Admin itself does not stop it.
func TestSweepSkipsQueuesTheUserNeverAdopted(t *testing.T) {
	refused := "ipp://127.0.0.1:1/ipp/print" // nothing listens on port 1: instant refusal, deterministic StateAbsent

	adoptedQueue := queue.Queue{Name: "My_Printer", DeviceURI: refused}
	handMadeA := queue.Queue{Name: "Hand_Made_A", DeviceURI: refused}
	handMadeB := queue.Queue{Name: "Hand_Made_B", DeviceURI: refused}

	adopted := []store.Adopted{
		{QueueName: "My_Printer", Fingerprint: identity.Fingerprint{}, LastSeen: recentlySeen()},
	}

	decisions := Sweep(context.Background(), []queue.Queue{adoptedQueue, handMadeA, handMadeB}, adopted, nil, fixedNow)

	if len(decisions) != 1 {
		t.Fatalf("got %d decisions for 3 queues (1 adopted, 2 hand-made); want exactly 1 — Sweep must never touch an unadopted queue", len(decisions))
	}
}

// TestSweepProducesNoDecisionForAdoptedEntryMissingItsQueue covers the
// other half of the join: an adopted printer whose queue no longer
// exists on the machine (deleted outside printkeeper) has nothing for
// Sweep to probe or act on.
func TestSweepProducesNoDecisionForAdoptedEntryMissingItsQueue(t *testing.T) {
	adopted := []store.Adopted{
		{QueueName: "Ghost_Printer", Fingerprint: identity.Fingerprint{}, LastSeen: recentlySeen()},
	}

	decisions := Sweep(context.Background(), []queue.Queue{}, adopted, nil, fixedNow)

	if len(decisions) != 0 {
		t.Fatalf("got %d decisions for an adopted printer with no matching queue; want 0", len(decisions))
	}
}

// TestDecideSetsQueueNameFromAdopted pins that a Decision is
// self-describing even from a direct Decide call, not only through
// Sweep: a Decision's QueueName must always come from the
// store.Adopted it was decided about, regardless of entry point.
func TestDecideSetsQueueNameFromAdopted(t *testing.T) {
	adopted := store.Adopted{QueueName: "Reception_Printer", Fingerprint: identity.Fingerprint{}, LastSeen: recentlySeen()}

	d := Decide(adopted, probe.Result{State: probe.StatePresent}, nil, fixedNow)
	if d.QueueName != "Reception_Printer" {
		t.Fatalf("Decision.QueueName = %q, want %q", d.QueueName, "Reception_Printer")
	}
}

// TestSweepDecisionsAreSelfDescribingByQueueName is the fix for the
// positional-association defect: a Decision's payload is an
// instruction to rewrite a specific queue's device address, so a
// caller must never have to re-associate a Decision with a queue by
// its position in Sweep's returned slice — the moment any filtering
// detail shifted that position, the tool would repair queue A using
// queue B's address, which is this project's first anti-goal arriving
// through an index.
//
// A fixture with a single queue cannot catch positional drift: index 0
// is trivially "correct" whether or not the association logic is
// right. This uses three adopted queues, each probed to a different,
// distinguishable Action, fed to Sweep in an order that does not match
// either the adopted slice's order or any other convenient ordering —
// so the only way to pass is for each returned Decision to carry the
// QueueName it is actually about.
func TestSweepDecisionsAreSelfDescribingByQueueName(t *testing.T) {
	refused := "ipp://127.0.0.1:1/ipp/print" // nothing listens on port 1: instant refusal, deterministic StateAbsent

	// Printer One: absent with a genuine UUID match -> ActionRepairSilently.
	uuidOne := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" // version 4: never MAC-derived, so it cannot accidentally score against anything below
	candidateOne := discovery.Candidate{ServiceName: "Candidate One", UUID: uuidOne}

	// Printer Two: absent with one partial (MAC-only) match -> ActionRepairAndRecord.
	// candidateTwo's UUID is version 1 (version nibble "1" in "11ec"); its node
	// field, e0bb9e00973d, is exactly the MAC printerTwo's fingerprint carries —
	// see identity.MACFromUUID. Its own UUID does not match anything stored, so
	// only WeightMAC (60) is contributed, never WeightUUID.
	candidateTwo := discovery.Candidate{ServiceName: "Candidate Two", UUID: "11111111-2222-11ec-8000-e0bb9e00973d"}

	// Printer Three: absent, nothing plausible, abandoned long past
	// AbandonAfter -> ActionLetGo. It shares no field with either
	// candidate above, so it scores 0 against both.

	queues := []queue.Queue{
		{Name: "Printer_Three", DeviceURI: refused},
		{Name: "Printer_One", DeviceURI: refused},
		{Name: "Printer_Two", DeviceURI: refused},
	}

	adopted := []store.Adopted{
		{
			QueueName:   "Printer_Two",
			Fingerprint: identity.Fingerprint{MAC: "e0:bb:9e:00:97:3d"},
			LastSeen:    recentlySeen(),
		},
		{
			QueueName:   "Printer_Three",
			Fingerprint: identity.Fingerprint{ServiceName: "Printer Three Only", ResourcePath: "printer-three/only"},
			LastSeen: map[string]store.Seen{
				"home": {URI: "ipp://192.168.1.60/ipp/print", At: fixedNow.Add(-(AbandonAfter + time.Hour))},
			},
		},
		{
			QueueName:   "Printer_One",
			Fingerprint: identity.Fingerprint{UUID: uuidOne},
			LastSeen:    recentlySeen(),
		},
	}

	candidates := []discovery.Candidate{candidateOne, candidateTwo}

	decisions := Sweep(context.Background(), queues, adopted, candidates, fixedNow)

	if len(decisions) != 3 {
		t.Fatalf("got %d decisions, want 3", len(decisions))
	}

	byName := make(map[string]Decision, len(decisions))
	for _, d := range decisions {
		if _, dup := byName[d.QueueName]; dup {
			t.Fatalf("duplicate QueueName %q among decisions: %+v", d.QueueName, decisions)
		}
		byName[d.QueueName] = d
	}

	want := map[string]Action{
		"Printer_One":   ActionRepairSilently,
		"Printer_Two":   ActionRepairAndRecord,
		"Printer_Three": ActionLetGo,
	}
	for name, wantAction := range want {
		d, ok := byName[name]
		if !ok {
			t.Fatalf("no decision carried QueueName %q; got %+v", name, decisions)
		}
		if d.Action != wantAction {
			t.Fatalf("decision for %q = %v, want %v (reason: %q) — a wrong action paired with the wrong name is exactly how this tool would repair the wrong printer", name, d.Action, wantAction, d.Reason)
		}
	}
}

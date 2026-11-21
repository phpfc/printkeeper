// Package repair is the judgement at the center of printkeeper: given
// what an adopted printer used to look like, what probing its last
// known address found, and what discovery sees on the network right
// now, decide whether — and how — to act. Everything upstream of this
// package only observes (probe) or proposes candidates (identity,
// resolve); this is where "nothing came out" and "it came out on
// another floor" are told apart.
//
// The design's first anti-goal is printing to the wrong printer: for a
// non-technical user, "nothing came out" is a comprehensible problem,
// but a document that silently printed somewhere else is one they
// cannot diagnose. The ladder below (spec §6.5) is built around that:
// it only ever repairs a queue's address in place, never its name, and
// it asks rather than guesses the moment more than one candidate is
// plausible. §11.7 adds the complementary case — a printer absent for
// so long it is more likely decommissioned than broken — without ever
// letting the daemon delete anything on its own.
package repair

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/store"
)

// Action is what the ladder decided to do about one adopted printer.
type Action int

const (
	// ActionNothing means no action is warranted: the printer is
	// present and healthy, it merely had a bad day (StateErrored), or
	// the probe could not be attempted at all (StateUnknown). None of
	// these are evidence the printer moved.
	ActionNothing Action = iota
	// ActionRepairSilently means an exact identity match was found at a
	// new address; the queue is repointed with no user notification.
	ActionRepairSilently
	// ActionRepairAndRecord means a single, merely plausible candidate
	// was found; the queue is repointed, but the change is only
	// surfaced if the user opens the app.
	ActionRepairAndRecord
	// ActionAsk means two or more candidates are plausible and the
	// ladder refuses to guess between them.
	ActionAsk
	// ActionReport means the printer is absent and nothing plausible
	// was found to repoint it to.
	ActionReport
	// ActionLetGo means the printer has been absent, with no plausible
	// candidate, continuously for longer than AbandonAfter: it is
	// likely decommissioned. The daemon stops probing it and, on next
	// user contact, offers to forget it — it never deletes the queue
	// itself.
	ActionLetGo
)

// String renders Action for log lines and test failure messages.
func (a Action) String() string {
	switch a {
	case ActionNothing:
		return "ActionNothing"
	case ActionRepairSilently:
		return "ActionRepairSilently"
	case ActionRepairAndRecord:
		return "ActionRepairAndRecord"
	case ActionAsk:
		return "ActionAsk"
	case ActionReport:
		return "ActionReport"
	case ActionLetGo:
		return "ActionLetGo"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// Decision is the ladder's verdict for one adopted printer. Target is
// the single candidate to repair onto (set for ActionRepairSilently and
// ActionRepairAndRecord only); Choices lists every candidate the user
// must be asked to pick between (ActionAsk only). Reason is a short,
// human-oriented explanation — rendering what resolve attempted, if
// anything, is the CLI's job, not this package's.
type Decision struct {
	Action  Action
	Target  discovery.Candidate
	Choices []discovery.Candidate
	Reason  string
}

// ThresholdExact, ThresholdPartial, and AbandonAfter are the ladder's
// three tunables.
//
// ThresholdExact resolves to identity.WeightUUID (200): the sum of every
// other weight (MAC 60 + DeviceID 25 + ServiceName 20 + ResourcePath 5 =
// 110) is strictly less than it, so no combination of non-UUID signals
// can ever reach it — only an actual UUID match can. Decide additionally
// verifies the UUID itself agrees (see isVerifiedUUIDMatch) rather than
// trusting the score alone: that arithmetic guarantee holds for scores
// identity.Match actually produces, but Decide is handed already-scored
// matches and must not assume they all came from that one function.
//
// ThresholdPartial is the floor above which a candidate counts as
// plausible at all.
//
// AbandonAfter (three weeks) is how long a printer must be continuously
// unreachable, with no plausible replacement found, before the ladder
// concludes it is more likely decommissioned than broken.
const (
	ThresholdExact   = identity.WeightUUID
	ThresholdPartial = 40
	AbandonAfter     = 21 * 24 * time.Hour
)

// sweepProbeTimeout bounds how long Sweep waits for each adopted
// printer's probe before moving on, mirroring resolve's own
// defaultProbeTimeout — a single slow or dead device must not stall the
// whole sweep.
const sweepProbeTimeout = 5 * time.Second

// Decide applies the repair confidence ladder to one adopted printer: a
// probe result against its last-known address, and the candidates
// discovery currently sees, scored against its stored fingerprint.
//
// A printer that answered (StatePresent) or answered unhealthily
// (StateErrored) has not moved — only StateAbsent is evidence worth
// acting on. StateUnknown means the machine itself could not ask, which
// is not a finding about the printer at all; both get ActionNothing.
func Decide(a store.Adopted, p probe.Result, matches []identity.Scored, now time.Time) Decision {
	switch p.State {
	case probe.StatePresent:
		return Decision{Action: ActionNothing, Reason: "printer answered; no repair needed"}
	case probe.StateErrored:
		return Decision{Action: ActionNothing, Reason: "printer answered but is unhealthy (e.g. out of paper, cover open); it has not moved"}
	case probe.StateUnknown:
		return Decision{Action: ActionNothing, Reason: "could not probe: this machine has no usable network right now"}
	}

	// From here, p.State == probe.StateAbsent.

	sorted := append([]identity.Scored(nil), matches...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })

	if len(sorted) > 0 {
		top := sorted[0]
		if top.Score >= ThresholdExact && isVerifiedUUIDMatch(a.Fingerprint, top.Candidate) {
			return Decision{
				Action: ActionRepairSilently,
				Target: top.Candidate,
				Reason: "exact identity match found at a new address",
			}
		}
	}

	var plausible []identity.Scored
	for _, s := range sorted {
		if s.Score >= ThresholdPartial {
			plausible = append(plausible, s)
		}
	}

	switch len(plausible) {
	case 0:
		if abandoned(a, now) {
			return Decision{
				Action: ActionLetGo,
				Reason: fmt.Sprintf("absent on every network for longer than %s with nothing plausible found; likely decommissioned", AbandonAfter),
			}
		}
		return Decision{Action: ActionReport, Reason: "printer is absent and no plausible candidate was found"}

	case 1:
		return Decision{
			Action: ActionRepairAndRecord,
			Target: plausible[0].Candidate,
			Reason: "single plausible candidate found at a new address",
		}

	default:
		top := plausible[0]
		var choices []discovery.Candidate
		for _, s := range plausible {
			if top.Score-s.Score < identity.WeightDeviceID {
				choices = append(choices, s.Candidate)
			}
		}
		if len(choices) >= 2 {
			return Decision{
				Action:  ActionAsk,
				Choices: choices,
				Reason:  "two or more candidates look equally plausible; printing to the wrong printer is worse than asking",
			}
		}
		// The leading candidate clearly dominates the rest, even though
		// more than one cleared ThresholdPartial.
		return Decision{
			Action: ActionRepairAndRecord,
			Target: top.Candidate,
			Reason: "single plausible candidate found at a new address",
		}
	}
}

// isVerifiedUUIDMatch reports whether candidate's own UUID actually
// agrees with fp's stored UUID, independent of whatever score it was
// handed. An empty stored UUID never verifies a match: two unknown
// values are not evidence of anything, matching identity.Match's own
// treatment of empty fields.
func isVerifiedUUIDMatch(fp identity.Fingerprint, candidate discovery.Candidate) bool {
	if fp.UUID == "" {
		return false
	}
	return normaliseUUID(candidate.UUID) == fp.UUID
}

// normaliseUUID mirrors identity's own (unexported) normalisation so a
// raw discovery.Candidate.UUID compares correctly against a
// Fingerprint.UUID that is already stored normalised (identity.FromCandidate
// does this on the way in). Candidate.UUID may arrive bare (Bonjour TXT)
// or "urn:uuid:"-prefixed (an IPP printer-uuid attribute's shape), and in
// either case or mixed case.
func normaliseUUID(uuid string) string {
	u := strings.ToLower(strings.TrimSpace(uuid))
	return strings.TrimPrefix(u, "urn:uuid:")
}

// abandoned reports whether a has been unreachable, on every network it
// has ever been seen on, for longer than AbandonAfter. It is measured
// from the single most recent Seen.At across the whole LastSeen map —
// never from one network in isolation — so a printer unseen on one
// network but recently seen on another is correctly not abandoned. An
// adopted printer with no Seen history at all (LastSeen empty) has no
// timestamp to measure from; reporting rather than letting go is the
// safer error in that case.
func abandoned(a store.Adopted, now time.Time) bool {
	var mostRecent time.Time
	found := false
	for _, seen := range a.LastSeen {
		if !found || seen.At.After(mostRecent) {
			mostRecent = seen.At
			found = true
		}
	}
	if !found {
		return false
	}
	return now.Sub(mostRecent) > AbandonAfter
}

// Sweep runs Decide for every queue on the host that the user actually
// adopted, probing each one's current device address and matching
// discovery's candidates against its stored fingerprint.
//
// It joins queues against adopted by name and silently skips any queue
// that join does not produce: queue.Admin trusts its caller and does not
// enforce this itself (its own documentation says so), so this is the
// only place that stands between "the user chose to protect this
// printer" and acting on a queue the user set up by hand, outside
// printkeeper entirely. An adopted printer whose queue no longer exists
// on the machine likewise produces no decision — there is nothing to
// probe or repair.
func Sweep(ctx context.Context, queues []queue.Queue, adopted []store.Adopted, candidates []discovery.Candidate, now time.Time) []Decision {
	decisions := make([]Decision, 0, len(queues))

	for _, q := range queues {
		a, ok := findAdopted(adopted, q.Name)
		if !ok {
			continue
		}

		result := probe.Probe(ctx, q.DeviceURI, sweepProbeTimeout)
		matches := identity.Match(a.Fingerprint, candidates)
		decisions = append(decisions, Decide(a, result, matches, now))
	}

	return decisions
}

// findAdopted looks up the Adopted entry whose QueueName equals
// queueName, if any.
func findAdopted(adopted []store.Adopted, queueName string) (store.Adopted, bool) {
	for _, a := range adopted {
		if a.QueueName == queueName {
			return a, true
		}
	}
	return store.Adopted{}, false
}

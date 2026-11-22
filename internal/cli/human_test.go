package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/repair"
	"github.com/phpfc/printkeeper/internal/resolve"
	"github.com/phpfc/printkeeper/internal/vocabulary"
)

// assertNoDomainVocabulary is spec §4's third anti-goal, verbatim: no
// domain vocabulary reaches the user, not even in an error message.
// It is a thin wrapper over internal/vocabulary, kept under this
// package's own established name so every existing call site in
// adopt_test.go, doctor_test.go, and heal_test.go needed no change —
// the list and the logic itself now live in one place shared by every
// package that renders user-facing text, internal/ipc included, not
// duplicated per package. See internal/vocabulary's own doc comment
// for why that package exists at all: this one, scoped to internal/cli,
// is exactly what let a leak in internal/ipc's own text go unnoticed.
func assertNoDomainVocabulary(t *testing.T, s string) {
	t.Helper()
	vocabulary.Assert(t, s)
}

// adversarialCandidate is a discovery.Candidate whose every field except
// Model carries exactly the vocabulary a careless renderer would leak: a
// real-looking address, a CUPS-ish service name, an IPP resource path.
// Model is the one field the controller ruling says a friendly-name
// renderer must prefer (it carries the Bonjour TXT "ty" field — see
// discovery_darwin.go — not the measured printer-name "ipp/print"); if
// DescribeDecision or friendlyName ever reaches past it, this candidate
// is what catches the leak.
func adversarialCandidate(model string) discovery.Candidate {
	return discovery.Candidate{
		ServiceName:  "Reception CUPS Queue Service",
		URI:          "ipp://192.168.1.50:631/ipp/print",
		ResourcePath: "ipp/print",
		Model:        model,
	}
}

// TestDescribeDecisionNeverLeaksDomainVocabulary is the brief's Step 1,
// mechanical rather than by-eye: one case per repair.Action, each fed a
// Decision whose QueueName, Target, Choices and Reason are all stuffed
// with the forbidden vocabulary. A renderer that ever touches those
// fields instead of the friendly name/attempts it was actually given
// fails here; a renderer that correctly ignores them in favour of the
// caller-supplied name and the candidates' Model fields passes for a
// real reason, not by accident of an empty fixture.
func TestDescribeDecisionNeverLeaksDomainVocabulary(t *testing.T) {
	// A stand-in for a raw, sanitised CUPS queue name — the one piece of
	// vocabulary that must never reach the user even though it is
	// genuinely present on the Decision.
	const adversarialQueueName = "reception_queue_ipp_printer"

	attempts := []resolve.Attempt{
		{Strategy: "lastKnownGood", Err: nil, Candidates: 0},
		{Strategy: "unicastHostname", Err: nil, Candidates: 0},
		{Strategy: "bonjourBrowse", Err: errors.New("ippfind against ipp://10.0.0.5:631/ipp/print failed"), Candidates: 0},
		{Strategy: "unicastDNSSD", Err: resolve.ErrNotAttempted, Candidates: 0},
		{Strategy: "snmp", Err: resolve.ErrNotAttempted, Candidates: 0},
	}

	cases := []struct {
		name string
		d    repair.Decision
	}{
		{
			name: "nothing",
			d:    repair.Decision{Action: repair.ActionNothing, QueueName: adversarialQueueName, Reason: "printer answered; no repair needed"},
		},
		{
			name: "repair silently",
			d: repair.Decision{
				Action:    repair.ActionRepairSilently,
				QueueName: adversarialQueueName,
				Target:    adversarialCandidate("Reception Printer"),
				Reason:    "exact identity match found at a new address",
			},
		},
		{
			name: "repair and record",
			d: repair.Decision{
				Action:    repair.ActionRepairAndRecord,
				QueueName: adversarialQueueName,
				Target:    adversarialCandidate("Reception Printer"),
				Reason:    "single plausible candidate found at a new address",
			},
		},
		{
			name: "ask",
			d: repair.Decision{
				Action:    repair.ActionAsk,
				QueueName: adversarialQueueName,
				Choices:   []discovery.Candidate{adversarialCandidate("Reception Printer"), adversarialCandidate("Lobby Printer")},
				Reason:    "two or more candidates look equally plausible; printing to the wrong printer is worse than asking",
			},
		},
		{
			name: "report",
			d:    repair.Decision{Action: repair.ActionReport, QueueName: adversarialQueueName, Reason: "printer is absent and no plausible candidate was found"},
		},
		{
			name: "let go",
			d:    repair.Decision{Action: repair.ActionLetGo, QueueName: adversarialQueueName, Reason: "absent on every network for longer than 504h0m0s with nothing plausible found; likely decommissioned"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DescribeDecision("Reception Printer", tc.d, attempts)
			assertNoDomainVocabulary(t, got)
			if !strings.Contains(got, "Reception Printer") {
				t.Fatalf("rendered sentence does not mention the printer by its friendly name: %q", got)
			}
		})
	}
}

// TestDescribeDecisionAskListsChoicesByFriendlyName pins that the Ask
// sentence actually enumerates the candidates by their friendly Model
// name — a renderer could pass the vocabulary scan above by omitting
// the choices entirely, which would not be an honest "ask" at all.
func TestDescribeDecisionAskListsChoicesByFriendlyName(t *testing.T) {
	d := repair.Decision{
		Action:  repair.ActionAsk,
		Choices: []discovery.Candidate{adversarialCandidate("Reception Printer"), adversarialCandidate("Lobby Printer")},
	}
	got := DescribeDecision("Office Printer", d, nil)
	if !strings.Contains(got, "Reception Printer") || !strings.Contains(got, "Lobby Printer") {
		t.Fatalf("Ask sentence does not list both candidates by friendly name: %q", got)
	}
}

// TestDescribeDecisionReportGivesAWayForward pins the controller's
// wording review: the sentence a person reads at the worst possible
// moment — the printer could not be found, something needs printing —
// must never be a dead end. The old wording ("There was nothing else we
// could try to find it.") is asserted absent, and every variant must
// name at least one thing a person can actually go check: that it is
// switched on, that it is on the same network, or that it can be set up
// again if it moved or was replaced. None of these are claims this tool
// can verify — it cannot tell "switched off" apart from "moved" apart
// from "replaced" — so the sentence must offer them as things worth
// checking, never assert which one happened.
func TestDescribeDecisionReportGivesAWayForward(t *testing.T) {
	const deadEnd = "nothing else we could try to find it"

	cases := []struct {
		name     string
		attempts []resolve.Attempt
	}{
		{
			name: "a full search that found nothing",
			attempts: []resolve.Attempt{
				{Strategy: "lastKnownGood", Candidates: 0},
				{Strategy: "unicastHostname", Candidates: 0},
				{Strategy: "bonjourBrowse", Candidates: 0},
				{Strategy: "unicastDNSSD", Err: resolve.ErrNotAttempted},
				{Strategy: "snmp", Err: resolve.ErrNotAttempted},
			},
		},
		{
			name: "a search where some checks could not even run",
			attempts: []resolve.Attempt{
				{Strategy: "lastKnownGood", Candidates: 0},
				{Strategy: "unicastHostname", Err: errors.New("resolver timed out")},
				{Strategy: "bonjourBrowse", Candidates: 0},
			},
		},
		{
			name:     "nothing to check at all",
			attempts: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := repair.Decision{Action: repair.ActionReport}
			got := DescribeDecision("Reception Printer", d, tc.attempts)

			if strings.Contains(got, deadEnd) {
				t.Fatalf("still contains the old dead-end phrasing: %q", got)
			}
			assertNoDomainVocabulary(t, got)

			hasSwitchedOn := strings.Contains(got, "switched on")
			hasSameNetwork := strings.Contains(got, "same network")
			hasSetUpAgain := strings.Contains(got, "choose it again")
			if !hasSwitchedOn || !hasSameNetwork || !hasSetUpAgain {
				t.Fatalf("sentence does not offer all three next steps (power, network, re-setup): %q", got)
			}
		})
	}
}

// TestDescribeDecisionLetGoDoesNotPromiseAnInteractionThatDoesNotExist
// is an IMPORTANT finding from code review: the old wording ("You can
// tell us to stop looking for it.") invited an action no command in
// this task's surface accepts — Heal reads no input, and there is no
// forget/release command anywhere. Inviting an action the tool cannot
// receive sends the user looking for a button that does not exist,
// which is worse than saying nothing. The sentence must state the fact
// (not seen in weeks) and stop there.
func TestDescribeDecisionLetGoDoesNotPromiseAnInteractionThatDoesNotExist(t *testing.T) {
	got := DescribeDecision("Reception Printer", repair.Decision{Action: repair.ActionLetGo}, nil)

	if strings.Contains(got, "tell us") || strings.Contains(got, "stop looking") {
		t.Fatalf("still promises an interaction nothing in this task's surface can accept: %q", got)
	}
	if !strings.Contains(got, "Reception Printer") || !strings.Contains(got, "weeks") {
		t.Fatalf("sentence lost the one fact it is allowed to state: %q", got)
	}
}

// TestDescribeDecisionAskPointsAtTheCommandRatherThanAskingAQuestion is
// the second IMPORTANT finding: Heal prints the Ask sentence and then
// reads no answer, so "Which one do you use?" was a question with no
// way to answer it — the same dead end the Report sentence was sent
// back for. The sentence must still name the candidates, and must name
// the command that can actually resolve this (adopt.go now repoints the
// existing printer in place rather than duplicating it — see
// TestAdoptResolvesAnAmbiguousMatchInPlaceRatherThanDuplicating — so
// this advice is true, not just present), the same way doctor.go's own
// "no printers protected yet" message names it rather than saying
// something vaguer like "set it up".
func TestDescribeDecisionAskPointsAtTheCommandRatherThanAskingAQuestion(t *testing.T) {
	d := repair.Decision{
		Action:  repair.ActionAsk,
		Choices: []discovery.Candidate{adversarialCandidate("Reception Printer"), adversarialCandidate("Lobby Printer")},
	}
	got := DescribeDecision("Office Printer", d, nil)

	if strings.Contains(got, "Which one do you use") {
		t.Fatalf("still asks a question Heal cannot receive an answer to: %q", got)
	}
	if !strings.Contains(got, "Reception Printer") || !strings.Contains(got, "Lobby Printer") {
		t.Fatalf("Ask sentence must still list both candidates by friendly name: %q", got)
	}
	if !strings.Contains(got, `"printkeeper adopt"`) {
		t.Fatalf("sentence does not name the command that can actually resolve this, the way doctor.go's precedent does: %q", got)
	}
}

// TestDescribeProbeNeverLeaksDomainVocabulary covers Doctor's renderer
// the same way: every probe.State, fed a Result whose own Err carries
// forbidden vocabulary (the kind of text a real IPP client error
// returns), must never surface it.
func TestDescribeProbeNeverLeaksDomainVocabulary(t *testing.T) {
	states := []probe.State{probe.StatePresent, probe.StateErrored, probe.StateAbsent, probe.StateUnknown}
	for _, s := range states {
		t.Run(s.String(), func(t *testing.T) {
			got := DescribeProbe("Reception Printer", probe.Result{
				State: s,
				Err:   errors.New("dial tcp ipp://192.168.1.50:631: connection refused (CUPS queue unreachable)"),
			})
			assertNoDomainVocabulary(t, got)
			if !strings.Contains(got, "Reception Printer") {
				t.Fatalf("rendered sentence does not mention the printer by its friendly name: %q", got)
			}
		})
	}
}

// Package cli implements the three commands a person actually runs:
// adopt, doctor, and heal. Every sentence written to an App's Out is
// read by someone who has never heard of IPP, mDNS, CUPS, a resource
// URI, or a print queue — spec §4's third anti-goal is domain
// vocabulary leaking into the UI, and that promise is only as good as
// what this file actually renders, not what it was intended to render.
// Printers are always referred to by a friendly, human-chosen name;
// never by an address.
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/repair"
	"github.com/phpfc/printkeeper/internal/resolve"
)

// friendlyName returns the human-readable name for a discovered
// printer. It prefers Model — the Bonjour TXT "ty" field, as surfaced
// by discovery.Candidate.Model — over ServiceName, because measured
// against this project's real printer, the IPP printer-name attribute
// ("ipp/print", not a name at all) is not what either of those fields
// carries; Model is what a person would actually call the device
// ("EPSON L4260 Series"). It never falls back to a URI, a resource
// path, or anything else that would read as an address.
func friendlyName(c discovery.Candidate) string {
	if c.Model != "" {
		return c.Model
	}
	if c.ServiceName != "" {
		return c.ServiceName
	}
	return "a printer"
}

// listFriendlyNames renders a numbered, one-per-line list of candidates
// by friendly name, for Adopt's selection prompt and for an Ask
// sentence's list of choices.
func listFriendlyNames(candidates []discovery.Candidate) string {
	var b strings.Builder
	for i, c := range candidates {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, friendlyName(c))
	}
	return b.String()
}

// humanizeQueueName turns a sanitised CUPS queue name back into
// something closer to prose, for the rare case nothing better is known
// about a printer than the name printkeeper itself assigned it:
// queue.SanitiseName folds spaces to underscores, so the reverse is
// undone here for display only — the stored name itself never changes.
func humanizeQueueName(name string) string {
	return strings.ReplaceAll(name, "_", " ")
}

// queueErrorMessage turns any error returned by queue.Admin — List,
// Create, SetDeviceURI, or Delete — into a sentence a non-technical
// reader can act on. queue.ErrNeedsAuthorization's own doc comment says
// it exists so the CLI can translate it into plain language instead of
// relaying a raw command failure; this is that translation, and the
// only place in this package it happens, so every command that touches
// queue.Admin goes through it rather than risking its own copy drifting
// from spec §4's "not even in an error message" promise.
//
// An error this function does not recognise is never passed through
// verbatim either — it could carry anything, including text
// classifyLpadminError (internal/queue/queue_darwin.go) did not
// classify — so the fallback is a vocabulary-free sentence that is
// honest about not knowing more, rather than a guess dressed up as one.
func queueErrorMessage(err error) string {
	switch {
	case errors.Is(err, queue.ErrNeedsAuthorization):
		return "This account can't change printer settings on this computer. Ask whoever manages it to make your account an admin, or run this as one."
	case errors.Is(err, queue.ErrServiceUnavailable):
		return "This computer's printing service isn't responding right now. Try again in a moment, or restart this computer if that keeps happening."
	default:
		return "Something went wrong there. Try again in a moment."
	}
}

// DescribeDecision turns one repair.Decision into a sentence a
// non-technical reader can act on. name identifies the adopted printer
// the decision is about, resolved by the caller from what the user
// already recognises it by — never from the Decision's own QueueName
// (CUPS's internal, sanitised identifier) or Reason (written for logs;
// repair's own documentation says rendering what was attempted is this
// package's job, not repair's). attempts, when non-nil, lets
// ActionReport explain what was tried; every other action ignores it.
func DescribeDecision(name string, d repair.Decision, attempts []resolve.Attempt) string {
	switch d.Action {
	case repair.ActionNothing:
		return fmt.Sprintf("%s is working fine.", name)

	case repair.ActionRepairSilently, repair.ActionRepairAndRecord:
		return fmt.Sprintf("%s stopped working for a moment, but it's working again now.", name)

	case repair.ActionAsk:
		return fmt.Sprintf(
			"We found more than one printer that could be %s, so we left it alone rather than guess.\n%sRun \"printkeeper adopt\" to choose the right one.",
			name, listFriendlyNames(d.Choices),
		)

	case repair.ActionReport:
		return fmt.Sprintf("We could not find your %s anywhere on the network. %s", name, attemptsSummary(attempts))

	case repair.ActionLetGo:
		return fmt.Sprintf("%s has not been seen in several weeks and may not be in use anymore.", name)

	default:
		return fmt.Sprintf("%s: nothing to report.", name)
	}
}

// DescribeProbe turns one probe.Result into a sentence for Doctor's
// plain status report. Unlike DescribeDecision, it reports a printer's
// current health directly from what the network round trip found —
// Doctor diagnoses, it does not act, so there is no repair decision to
// describe, only a state.
func DescribeProbe(name string, r probe.Result) string {
	switch r.State {
	case probe.StatePresent:
		return fmt.Sprintf("%s is working fine.", name)
	case probe.StateErrored:
		return fmt.Sprintf("%s answered, but needs attention — it may be out of paper, have a cover open, or be jammed.", name)
	case probe.StateAbsent:
		return fmt.Sprintf("%s is not responding right now.", name)
	case probe.StateUnknown:
		return fmt.Sprintf("could not check on %s right now — this computer has no network connection.", name)
	default:
		return fmt.Sprintf("%s: status unknown.", name)
	}
}

// humanStrategyNames translates each resolve.Strategy's internal Name()
// into a phrase a non-technical reader can follow: what was checked,
// never how.
var humanStrategyNames = map[string]string{
	"lastKnownGood":   "the address it used last time",
	"unicastHostname": "its network name",
	"bonjourBrowse":   "the local network",
	"unicastDNSSD":    "your network's directory",
	"snmp":            "an identifying signal from the printer itself",
}

// wayForward is the one piece every ActionReport sentence must never
// skip: this is the moment a person reads it at the worst possible
// time — something needs printing and the tool has just said it gave
// up — so "nothing else to try" is a dead end, not an honest answer.
// It names the things a person can actually check, in order of
// likelihood, without claiming a diagnosis this tool cannot make: it
// cannot tell "switched off" apart from "unplugged from the network"
// apart from "replaced by a different printer", so it offers all three
// as things worth checking rather than asserting which one happened.
const wayForward = "It's worth checking that it's switched on and connected to the same network as this computer. If you've moved it or set up a replacement, you can choose it again next time you run this."

// attemptsSummary renders what Resolve tried, honestly, and always ends
// with wayForward so the sentence leaves the reader able to act. A
// strategy that never ran at all (resolve.ErrNotAttempted) is omitted
// rather than reported as "found nothing" — nobody looked, so there is
// nothing honest to say about it. A strategy's own error text is never
// surfaced, only whether something was tried and whether every attempt
// came back empty.
//
// A genuine, available signal — whether any attempted strategy could
// not even run (anyFailed) — gets its own caveat, because it changes
// how complete the search actually was. What does not get its own
// branch is the distinction between "likely switched off or
// disconnected" and "likely moved or replaced": unicastHostname cannot
// currently tell an operational lookup failure apart from a hostname
// that genuinely no longer exists (see resolve/unicast.go), and in
// practice every strategy this function sees has already run and come
// back empty by the time ActionReport fires, so there is no reliable
// signal in this data to pick one explanation over the other. Both stay
// in wayForward as equally plausible things to check, rather than this
// function pretending to a diagnosis the data does not support.
func attemptsSummary(attempts []resolve.Attempt) string {
	var tried []string
	anyFailed := false
	for _, a := range attempts {
		if errors.Is(a.Err, resolve.ErrNotAttempted) {
			continue
		}
		phrase, ok := humanStrategyNames[a.Strategy]
		if !ok {
			phrase = "another way of finding it"
		}
		tried = append(tried, phrase)
		if a.Err != nil {
			anyFailed = true
		}
	}

	if len(tried) == 0 {
		return "There was nothing else we could check from here. " + wayForward
	}

	sentence := "We checked " + joinHuman(tried) + ", but nothing answered. "
	if anyFailed {
		sentence += "A few of those checks could not even run, which can happen when this computer's own network connection is having trouble. "
	}
	return sentence + wayForward
}

// joinHuman joins items as an English list with an Oxford comma:
// "a", "a and b", or "a, b, and c".
func joinHuman(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

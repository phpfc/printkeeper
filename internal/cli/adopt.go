package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/repair"
	"github.com/phpfc/printkeeper/internal/store"
)

// adoptBrowseTimeout bounds the Bonjour browse Adopt performs to build
// its numbered list.
const adoptBrowseTimeout = 5 * time.Second

// adoptProbeTimeout bounds the one IPP round trip fingerprintForExisting
// makes when protecting a queue printkeeper did not create.
const adoptProbeTimeout = 5 * time.Second

// Adopt is the only command that ever asks the user to type anything,
// and the only thing it asks for is a small number: spec goal 2 is zero
// address typing, ever, and the numbered list this prints, built from
// friendly names, is what replaces macOS's own Add Printer flow — type
// an internal hostname by hand and hope — which is the error-prone path
// this project exists to delete.
//
// On a first run — nothing adopted yet, but this computer already has
// printers configured outside printkeeper — it offers to protect those
// first (spec §11.2): the likeliest way someone installs this tool is
// because a printer they already had just broke.
func (a *App) Adopt(ctx context.Context, in io.Reader) error {
	adopted, err := a.Store.Load()
	if err != nil {
		return fmt.Errorf("load adopted printers: %w", err)
	}
	queues, err := a.Queues.List(ctx)
	if err != nil {
		return errors.New(queueErrorMessage(err))
	}

	reader := bufio.NewReader(in)

	if len(adopted) == 0 {
		unprotected := queuesNotAdopted(queues, adopted)
		if len(unprotected) > 0 {
			adopted, err = a.offerToProtect(ctx, reader, adopted, unprotected)
			if err != nil {
				return err
			}
		}
	}

	candidates, err := a.Disco.Browse(ctx, adoptBrowseTimeout)
	if err != nil {
		return fmt.Errorf("look for printers on the network: %w", err)
	}
	candidates = dedupeCandidates(candidates)

	fresh := excludeAlreadyAdopted(candidates, adopted)
	if len(fresh) == 0 {
		fmt.Fprintln(a.Out, "No new printers were found on the network.")
		return nil
	}

	fmt.Fprintln(a.Out, "Choose a printer to set up:")
	fmt.Fprint(a.Out, listFriendlyNames(fresh))

	choice, err := readSelection(reader, a.Out, len(fresh))
	if err != nil {
		return fmt.Errorf("read your choice: %w", err)
	}
	if choice == 0 {
		fmt.Fprintln(a.Out, "No problem — nothing was set up. Run this again whenever you want to look, and the printers you already protected will stay exactly as they are.")
		return nil
	}
	chosen := fresh[choice-1]

	// A candidate that plausibly (but not exactly — an exact match was
	// already excluded above) matches a printer already protected is
	// not a new printer at all: it is the same ambiguous situation
	// ActionAsk left alone rather than guess about. The person just
	// made the explicit choice the ladder refused to make on its own,
	// which is stronger evidence than any automated score — so this
	// repoints the existing queue in place rather than creating a
	// second one for the same physical device. Without this, following
	// the Ask sentence's own advice to "set it up again" would create
	// a duplicate queue while leaving the original entry unresolved —
	// found by tracing this exact path, not by reasoning about it.
	if existing, ok := plausibleMatch(chosen, adopted); ok {
		if err := a.resolveAmbiguousMatch(ctx, existing, chosen, &adopted); err != nil {
			return err
		}
	} else if err := a.createAndStore(ctx, chosen, queueNames(queues), &adopted); err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "%s is set up and ready to use.\n", friendlyName(chosen))
	return nil
}

// plausibleMatch reports whether chosen plausibly describes a printer
// already protected — the same scoring threshold (repair.ThresholdPartial)
// the repair ladder itself uses to decide a candidate is worth asking
// about rather than ignoring outright. By the time this is consulted,
// anything scoring at the ladder's exact-match bar has already been
// filtered out of the candidate list entirely (see
// excludeAlreadyAdopted), so a match found here is always the
// ambiguous, not-yet-resolved case.
//
// It is only safe to resolve automatically because of an invariant it
// does not itself enforce: identity.FromCandidate never populates
// DeviceID from a bare discovery.Candidate, so two printers sharing
// only model-derived fields (ServiceName, ResourcePath) can score at
// most WeightServiceName+WeightResourcePath — below ThresholdPartial —
// without a MAC or UUID match. TestPlausibleMatchCannotBeReachedByModelSimilarityAlone
// (adopt_test.go) pins that arithmetic directly so a future rebalance
// of those weights fails a test instead of silently letting two
// different physical printers of the same model cross this bar.
//
// It also returns the first adopted entry that crosses the bar, not
// the best-scoring one across every adopted entry. Immaterial at
// today's threshold and weights (see above), but worth knowing if the
// threshold is ever loosened: a chosen candidate that happens to be
// plausible for more than one adopted printer resolves against
// whichever one appears first in adopted, not whichever one scores
// highest.
func plausibleMatch(chosen discovery.Candidate, adopted []store.Adopted) (store.Adopted, bool) {
	for _, ad := range adopted {
		scored := identity.Match(ad.Fingerprint, []discovery.Candidate{chosen})
		if len(scored) == 1 && scored[0].Score >= repair.ThresholdPartial {
			return ad, true
		}
	}
	return store.Adopted{}, false
}

// resolveAmbiguousMatch repoints an already-adopted printer's existing
// queue at chosen rather than creating a new one — the explicit choice
// a person just made resolving the ambiguity the ladder itself refused
// to guess at. The queue's name, and therefore what the person sees in
// every print dialog, never changes. Unlike repair's own automatic,
// unverified partial-match tier (see heal.go's recordRepair), this does
// refresh the stored Fingerprint: a human's deliberate selection from a
// list is not a guess the way an algorithm's sub-exact score is, and
// spec §6.5's whole premise is that an explicit human choice is the one
// thing this project always trusts.
func (a *App) resolveAmbiguousMatch(ctx context.Context, existing store.Adopted, chosen discovery.Candidate, adopted *[]store.Adopted) error {
	if err := a.Queues.SetDeviceURI(ctx, existing.QueueName, chosen.URI); err != nil {
		return fmt.Errorf("couldn't point %s at the one you picked. %s", friendlyName(chosen), queueErrorMessage(err))
	}

	now := time.Now()
	for i := range *adopted {
		if (*adopted)[i].QueueName != existing.QueueName {
			continue
		}
		(*adopted)[i].Fingerprint = identity.FromCandidate(chosen)
		if (*adopted)[i].LastSeen == nil {
			(*adopted)[i].LastSeen = map[string]store.Seen{}
		}
		(*adopted)[i].LastSeen[a.networkKey()] = store.Seen{URI: chosen.URI, At: now}
		break
	}

	return a.Store.Save(*adopted)
}

// offerToProtect lists queues the user configured outside printkeeper
// and asks, one at a time, whether to protect each (spec §11.2). A
// queue the user declines is left completely alone — Adopt only ever
// acts on a queue it was explicitly told to protect.
func (a *App) offerToProtect(ctx context.Context, reader *bufio.Reader, adopted []store.Adopted, unprotected []queue.Queue) ([]store.Adopted, error) {
	fmt.Fprintln(a.Out, "This computer already has printers set up. Protect them so they can be fixed automatically if they stop working?")

	changed := false
	for _, q := range unprotected {
		name := q.Description
		if name == "" {
			name = humanizeQueueName(q.Name)
		}
		fmt.Fprintf(a.Out, "Protect %s? [y/N] ", name)

		line, err := readLine(reader)
		if err != nil {
			return adopted, fmt.Errorf("read your answer: %w", err)
		}
		if !isYes(line) {
			continue
		}

		fp := a.fingerprintForExistingQueue(ctx, q)
		now := time.Now()
		adopted = append(adopted, store.Adopted{
			QueueName:   q.Name,
			Fingerprint: fp,
			AdoptedAt:   now,
			LastSeen: map[string]store.Seen{
				a.networkKey(): {URI: q.DeviceURI, At: now},
			},
		})
		changed = true
	}

	if changed {
		if err := a.Store.Save(adopted); err != nil {
			return adopted, fmt.Errorf("save adopted printers: %w", err)
		}
	}
	return adopted, nil
}

// fingerprintForExistingQueue builds a Fingerprint for a queue
// printkeeper did not create: it probes the queue's current address
// directly for the identity attributes an IPP round trip carries
// (printer-uuid, printer-device-id) and funnels them through
// identity.FromCandidate — the one place UUID normalisation happens —
// rather than assembling a Fingerprint by hand, per the controller
// ruling binding this package. A printer that does not answer right now
// is still protected; it just gets a thinner Fingerprint, with less for
// future resolution to go on.
func (a *App) fingerprintForExistingQueue(ctx context.Context, q queue.Queue) identity.Fingerprint {
	result := probe.Probe(ctx, q.DeviceURI, adoptProbeTimeout)

	candidate := discovery.Candidate{
		ServiceName: q.Description,
		URI:         q.DeviceURI,
		UUID:        firstAttr(result.Attributes, "printer-uuid"),
	}

	fp := identity.FromCandidate(candidate)
	fp.DeviceID = firstAttr(result.Attributes, "printer-device-id")
	return fp
}

// firstAttr returns the first value reported for name, or "" if absent.
func firstAttr(attrs map[string][]string, name string) string {
	if v := attrs[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// createAndStore sanitises chosen's friendly name into a permanent
// queue name, creates the queue, and only then persists the adoption —
// in that order, so a failure creating the queue never leaves a
// phantom entry in the store describing a printer nothing was actually
// set up for.
func (a *App) createAndStore(ctx context.Context, chosen discovery.Candidate, taken []string, adopted *[]store.Adopted) error {
	name := queue.SanitiseName(friendlyName(chosen), taken)
	if err := a.Queues.Create(ctx, name, chosen.URI, friendlyName(chosen)); err != nil {
		return fmt.Errorf("couldn't set up %s. %s", friendlyName(chosen), queueErrorMessage(err))
	}

	now := time.Now()
	*adopted = append(*adopted, store.Adopted{
		QueueName:   name,
		Fingerprint: identity.FromCandidate(chosen),
		AdoptedAt:   now,
		LastSeen: map[string]store.Seen{
			a.networkKey(): {URI: chosen.URI, At: now},
		},
	})

	if err := a.Store.Save(*adopted); err != nil {
		return fmt.Errorf("save adopted printers: %w", err)
	}
	return nil
}

// queuesNotAdopted returns every queue configured on this machine that
// no entry in adopted already names.
func queuesNotAdopted(queues []queue.Queue, adopted []store.Adopted) []queue.Queue {
	adoptedNames := make(map[string]struct{}, len(adopted))
	for _, ad := range adopted {
		adoptedNames[ad.QueueName] = struct{}{}
	}
	var out []queue.Queue
	for _, q := range queues {
		if _, ok := adoptedNames[q.Name]; !ok {
			out = append(out, q)
		}
	}
	return out
}

// excludeAlreadyAdopted drops every candidate whose identity already
// matches a protected printer, so Adopt's list only ever offers
// printers genuinely new to this computer. Matching uses the same
// exact-identity threshold the repair ladder trusts for a silent
// repair (identity.WeightUUID): nothing weaker is enough to call two
// printers the same one here either.
func excludeAlreadyAdopted(candidates []discovery.Candidate, adopted []store.Adopted) []discovery.Candidate {
	var fresh []discovery.Candidate
	for _, c := range candidates {
		if isAlreadyAdopted(c, adopted) {
			continue
		}
		fresh = append(fresh, c)
	}
	return fresh
}

func isAlreadyAdopted(c discovery.Candidate, adopted []store.Adopted) bool {
	for _, ad := range adopted {
		scored := identity.Match(ad.Fingerprint, []discovery.Candidate{c})
		if len(scored) == 1 && scored[0].Score >= identity.WeightUUID {
			return true
		}
	}
	return false
}

// dedupeCandidates collapses candidates that describe the same physical
// printer into one entry, in first-seen order. The real printer this
// project exists for advertises both an "_ipp._tcp" and an
// "_ipps._tcp" Bonjour service record — confirmed by hand — which
// discovery surfaces as two separate Candidates with the same UUID, the
// same friendly name, and nothing to tell a user apart about them.
// Presenting both as if they were two choices hands a non-technical
// person a decision that does not matter, which is exactly the kind of
// decision spec goal 2 exists to spare them. When two candidates
// describe the same printer, the one whose address uses IPP-over-TLS
// ("ipps") is kept, since an earlier slice already taught this project
// how to speak it.
func dedupeCandidates(candidates []discovery.Candidate) []discovery.Candidate {
	best := make(map[string]discovery.Candidate, len(candidates))
	var order []string

	for _, c := range candidates {
		key := candidateIdentityKey(c)
		existing, seen := best[key]
		if !seen {
			best[key] = c
			order = append(order, key)
			continue
		}
		if isIPPS(c.URI) && !isIPPS(existing.URI) {
			best[key] = c
		}
	}

	deduped := make([]discovery.Candidate, 0, len(order))
	for _, key := range order {
		deduped = append(deduped, best[key])
	}
	return deduped
}

// candidateIdentityKey returns a string that is equal for two
// Candidates describing the same physical printer: identity.FromCandidate's
// normalised UUID when c has one, since nothing weaker is enough to
// call two printers the same one elsewhere in this package either; a
// fingerprint built from the rest of what FromCandidate carries when it
// does not, so two distinct printers of the same model with no UUID —
// the case the repair ladder itself refuses to guess between — still
// get distinct keys rather than being collapsed into one by accident.
func candidateIdentityKey(c discovery.Candidate) string {
	fp := identity.FromCandidate(c)
	if fp.UUID != "" {
		return "uuid:" + fp.UUID
	}
	return strings.Join([]string{"fp", fp.MAC, fp.ServiceName, fp.ResourcePath, strings.Join(fp.PDLs, ",")}, "|")
}

// isIPPS reports whether uri's scheme is IPP-over-TLS. An unparseable
// uri is treated as not-ipps rather than an error — dedupeCandidates
// only uses this to break a tie between two candidates that already
// matched by identity, so the worst case of a malformed URI here is
// keeping whichever candidate was seen first, not losing one.
func isIPPS(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "ipps")
}

// queueNames returns the "taken" list SanitiseName must avoid colliding
// with: every existing queue's name, exactly as cupsd reports it.
// SanitiseName's own collision check is case-insensitive (it has to be —
// cupsd itself folds printer names that way), so this package does not
// need to normalise case itself before calling in.
func queueNames(queues []queue.Queue) []string {
	names := make([]string, len(queues))
	for i, q := range queues {
		names[i] = q.Name
	}
	return names
}

// readSelection prompts out for a 1-based choice among n options, or 0
// if none of them is right — a non-technical user staring at a list
// that does not contain their printer must have a way out that is not
// killing the process, which looks exactly like the tool being broken.
// It re-prompts on anything else that does not parse as a number in
// range. The only typing Adopt ever asks for is a small integer, and
// even that gets a forgiving retry loop rather than a hard failure.
// readSelection returns 0, nil (not an error) when the user declines
// every choice; the caller decides what that means.
func readSelection(reader *bufio.Reader, out io.Writer, n int) (int, error) {
	for {
		fmt.Fprint(out, "Enter a number, or 0 if none of these is right: ")
		line, err := readLine(reader)
		if err != nil {
			return 0, err
		}
		choice, convErr := strconv.Atoi(line)
		if convErr == nil && choice == 0 {
			return 0, nil
		}
		if convErr == nil && choice >= 1 && choice <= n {
			return choice, nil
		}
		fmt.Fprintf(out, "Please enter a number from 1 to %d, or 0 if none of these is right.\n", n)
	}
}

// readLine reads one line from reader, trimmed of surrounding
// whitespace. A line with content followed immediately by EOF (no
// trailing newline) is returned successfully; EOF with no content at
// all is reported as an error, so a caller in a retry loop does not
// spin forever re-prompting against a closed input stream.
func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		if err == io.EOF && line != "" {
			return strings.TrimSpace(line), nil
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// isYes reports whether s is an affirmative answer to a yes/no prompt.
func isYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

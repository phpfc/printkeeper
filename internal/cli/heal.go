package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/repair"
	"github.com/phpfc/printkeeper/internal/resolve"
	"github.com/phpfc/printkeeper/internal/store"
)

// healBrowseTimeout bounds the single Bonjour browse Heal performs to
// feed repair.Sweep's identity matching against every adopted printer
// at once — one browse per Heal run, not one per printer, so a crowded
// network is only asked once.
const healBrowseTimeout = 5 * time.Second

// Heal probes every adopted printer's current address and repairs any
// queue the repair confidence ladder is confident enough to touch.
//
// Per spec §2 goal 3, a successful repair — ActionRepairSilently or
// ActionRepairAndRecord — writes nothing to Out. "Surfaced only if the
// user opens the app" (spec's description of the recorded tier) has no
// app to surface into yet in this slice; both tiers stay silent here
// and the distinction is preserved only in what gets persisted; the
// surfacing layer is a later task's to build (see internal/cli's report
// for this call named explicitly). Only a situation the ladder refuses
// to resolve on its own — ActionAsk, ActionReport, ActionLetGo — is
// written out, in plain language built by DescribeDecision.
func (a *App) Heal(ctx context.Context) error {
	adopted, err := a.Store.Load()
	if err != nil {
		return fmt.Errorf("load adopted printers: %w", err)
	}
	queues, err := a.Queues.List(ctx)
	if err != nil {
		return errors.New(queueErrorMessage(err))
	}
	candidates, err := a.Disco.Browse(ctx, healBrowseTimeout)
	if err != nil {
		return fmt.Errorf("look for printers on the network: %w", err)
	}

	now := time.Now()
	decisions := repair.Sweep(ctx, queues, adopted, candidates, now)

	changed := false
	for _, d := range decisions {
		switch d.Action {
		case repair.ActionNothing:
			// Healthy, unhealthy, or unable to check — none of these
			// are evidence the printer moved. Nothing to do, nothing
			// to say.

		case repair.ActionRepairSilently, repair.ActionRepairAndRecord:
			if err := a.Queues.SetDeviceURI(ctx, d.QueueName, d.Target.URI); err != nil {
				return fmt.Errorf("couldn't fix %s automatically. %s", a.nameForQueue(adopted, d.QueueName), queueErrorMessage(err))
			}
			recordRepair(adopted, d, a.networkKey(), now)
			changed = true

		case repair.ActionAsk:
			fmt.Fprintln(a.Out, DescribeDecision(a.nameForQueue(adopted, d.QueueName), d, nil))

		case repair.ActionReport:
			name := a.nameForQueue(adopted, d.QueueName)
			attempts := a.attemptsFor(ctx, adopted, d.QueueName)
			fmt.Fprintln(a.Out, DescribeDecision(name, d, attempts))

		case repair.ActionLetGo:
			fmt.Fprintln(a.Out, DescribeDecision(a.nameForQueue(adopted, d.QueueName), d, nil))

		default:
			// An action this switch does not yet recognise must still
			// be surfaced, not silently skipped: a future Action added
			// to internal/repair without updating this switch would
			// otherwise read as "nothing happened" rather than "this
			// build does not know what to do yet". DescribeDecision's
			// own default case renders something for any Action it
			// does not recognise either, so this still produces a
			// sentence rather than an empty line.
			fmt.Fprintln(a.Out, DescribeDecision(a.nameForQueue(adopted, d.QueueName), d, nil))
		}
	}

	if changed {
		if err := a.Store.Save(adopted); err != nil {
			return fmt.Errorf("save adopted printers: %w", err)
		}
	}
	return nil
}

// networkKey calls a.Keyer.Key() once, guarding against a nil Keyer so
// a caller that only cares about the parts of Heal that never need a
// network key (e.g. the global "never touch an unadopted queue" test)
// does not have to supply one.
func (a *App) networkKey() string {
	if a.Keyer == nil {
		return ""
	}
	return a.Keyer.Key()
}

// recordRepair updates adopted in place after a successful repair.
//
// Only ActionRepairSilently refreshes the stored Fingerprint — via
// identity.FromCandidate, never assembled by hand, per the controller
// ruling binding this package: store.Adopted's persisted Fingerprint
// must carry the same normalised UUID identity.FromCandidate already
// produces, because repair.isVerifiedUUIDMatch compares a stored
// Fingerprint's UUID as-is against a freshly normalised candidate UUID,
// and this package becomes a second producer of that field the moment
// it persists one. ActionRepairAndRecord is a merely plausible match —
// not UUID-verified — so its Fingerprint is left exactly as it was;
// only LastSeen moves, to the address the queue was just pointed at.
// Degrading future identity comparisons with a lower-confidence guess
// would be the same anti-goal (printing to the wrong printer) arriving
// one Heal run later.
func recordRepair(adopted []store.Adopted, d repair.Decision, netKey string, now time.Time) {
	for i := range adopted {
		if adopted[i].QueueName != d.QueueName {
			continue
		}
		if d.Action == repair.ActionRepairSilently {
			adopted[i].Fingerprint = identity.FromCandidate(d.Target)
		}
		if adopted[i].LastSeen == nil {
			adopted[i].LastSeen = map[string]store.Seen{}
		}
		if netKey != "" {
			adopted[i].LastSeen[netKey] = store.Seen{URI: d.Target.URI, At: now}
		}
		return
	}
}

// findAdoptedByQueueName looks up the Adopted entry whose QueueName
// equals queueName, if any.
func findAdoptedByQueueName(adopted []store.Adopted, queueName string) (store.Adopted, bool) {
	for _, ad := range adopted {
		if ad.QueueName == queueName {
			return ad, true
		}
	}
	return store.Adopted{}, false
}

// nameForQueue resolves the friendly name DescribeDecision should use
// for queueName: the printer's own self-reported Bonjour name, carried
// in its stored Fingerprint — never the sanitised CUPS queue name
// itself, which is derived for the daemon's own bookkeeping and may not
// read well in a sentence (queue.SanitiseName folds spaces to
// underscores and may append a numeric suffix).
func (a *App) nameForQueue(adopted []store.Adopted, queueName string) string {
	if ad, ok := findAdoptedByQueueName(adopted, queueName); ok && ad.Fingerprint.ServiceName != "" {
		return ad.Fingerprint.ServiceName
	}
	return humanizeQueueName(queueName)
}

// attemptsFor runs the full resolve ladder for the adopted printer named
// queueName, so ActionReport can explain what was tried beyond the
// single Bonjour browse repair.Sweep itself used. A queueName with no
// matching Adopted entry (should not happen — Sweep only ever decides
// about queues it joined against adopted) yields no attempts rather
// than a panic.
func (a *App) attemptsFor(ctx context.Context, adopted []store.Adopted, queueName string) []resolve.Attempt {
	ad, ok := findAdoptedByQueueName(adopted, queueName)
	if !ok {
		return nil
	}
	_, attempts := resolve.Resolve(ctx, a.Strategies, ad, a.networkKey())
	return attempts
}

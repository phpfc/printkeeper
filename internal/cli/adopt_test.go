package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/repair"
	"github.com/phpfc/printkeeper/internal/store"
)

// TestAdoptListsByFriendlyNameAndCreatesTheChosenOne is the heart of
// spec goal 2, zero address typing: the ruling that an adopt list built
// from printer-name would show the user "ipp/print" means the list
// Adopt prints and the queue Adopt creates must both come from the
// candidate's Model, never its ServiceName, URI, or resource path. The
// second candidate is deliberately chosen (selection "2"), so this test
// cannot pass by accident of always picking the first/only option —
// if Adopt ever mapped the typed number to the wrong slice index, this
// would create a queue for "Lobby Printer" address data under a
// "Front Desk Printer" name instead.
func TestAdoptListsByFriendlyNameAndCreatesTheChosenOne(t *testing.T) {
	candidates := []discovery.Candidate{
		{ServiceName: "svc-a", Model: "Front Desk Printer", URI: "ipp://10.0.0.5:631/ipp/print", ResourcePath: "ipp/print"},
		{ServiceName: "svc-b", Model: "Lobby Printer", URI: "ipp://10.0.0.6:631/ipp/print", ResourcePath: "ipp/print"},
	}

	admin := &fakeAdmin{}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: candidates}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("2\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if !strings.Contains(out.String(), "Front Desk Printer") || !strings.Contains(out.String(), "Lobby Printer") {
		t.Fatalf("listed output does not name both candidates by friendly name: %q", out.String())
	}
	assertNoDomainVocabulary(t, out.String())

	if len(admin.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1: %+v", len(admin.createCalls), admin.createCalls)
	}
	if admin.createCalls[0].uri != candidates[1].URI {
		t.Fatalf("Create was called with uri %q, want the second candidate's uri %q", admin.createCalls[0].uri, candidates[1].URI)
	}

	if len(st.adopted) != 1 {
		t.Fatalf("store has %d adopted entries, want 1", len(st.adopted))
	}
	if st.adopted[0].Fingerprint.ServiceName != "svc-b" {
		t.Fatalf("stored Fingerprint.ServiceName = %q, want the chosen candidate's (%q)", st.adopted[0].Fingerprint.ServiceName, "svc-b")
	}
	if st.adopted[0].LastSeen["home"].URI != candidates[1].URI {
		t.Fatalf("stored LastSeen[home].URI = %q, want %q", st.adopted[0].LastSeen["home"].URI, candidates[1].URI)
	}
}

// TestAdoptExcludesAlreadyProtectedPrinters ensures a printer already
// adopted (exact UUID match) never reappears in the numbered list —
// offering it again would invite a second, duplicate queue for the same
// physical device.
func TestAdoptExcludesAlreadyProtectedPrinters(t *testing.T) {
	const uuid = "cfe92100-67c4-11d4-a45f-e0bb9e00973d"

	already := store.Adopted{
		QueueName:   "Office_Printer",
		Fingerprint: identity.FromCandidate(discovery.Candidate{UUID: uuid, Model: "Office Printer"}),
	}

	candidates := []discovery.Candidate{
		{UUID: uuid, Model: "Office Printer", URI: "ipp://10.0.0.5:631/ipp/print"},
		{Model: "New Printer", URI: "ipp://10.0.0.7:631/ipp/print"},
	}

	admin := &fakeAdmin{}
	st := &fakeStore{adopted: []store.Adopted{already}}
	disco := fakeDiscoverer{candidates: candidates}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("1\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if strings.Contains(out.String(), "Office Printer") {
		t.Fatalf("already-adopted printer was offered again: %q", out.String())
	}
	if len(admin.createCalls) != 1 || admin.createCalls[0].uri != candidates[1].URI {
		t.Fatalf("Create calls = %+v, want exactly one Create for the new printer's uri %q", admin.createCalls, candidates[1].URI)
	}
}

// TestAdoptNoneOfTheseExitsCleanlyWithoutCreating is spec goal 2's
// other failure mode: a non-technical user who does not recognise any
// listed printer must have a way out of the prompt. Before this test,
// readSelection only ever accepted 1..n, so declining was impossible —
// the only way out of a wrong list was to kill the process, which looks
// exactly like the tool being broken. Choosing "0" must create nothing,
// store nothing, and say something useful in the same plain register as
// every other message.
func TestAdoptNoneOfTheseExitsCleanlyWithoutCreating(t *testing.T) {
	candidates := []discovery.Candidate{
		{ServiceName: "svc-a", Model: "Front Desk Printer", URI: "ipp://10.0.0.5:631/ipp/print"},
	}

	admin := &fakeAdmin{}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: candidates}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("0\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if len(admin.createCalls) != 0 {
		t.Fatalf("Create was called (%v) after declining every choice", admin.createCalls)
	}
	if len(st.adopted) != 0 {
		t.Fatalf("store has %d adopted entries, want 0 after declining", len(st.adopted))
	}
	if out.Len() == 0 {
		t.Fatalf("Adopt wrote nothing after the user declined every choice; it must say something")
	}
	assertNoDomainVocabulary(t, out.String())
}

// TestAdoptListsOnePrinterOncePerPhysicalDevice is the real shape found
// against the real printer this project exists for: it advertises both
// an _ipp._tcp and an _ipps._tcp Bonjour service record, which discovery
// surfaces as two separate candidates — same UUID, same friendly name,
// different URI scheme. Showing both as if they were two choices hands
// a non-technical user a decision that does not matter (which is
// exactly the kind of decision they should never be handed): the list
// must show the printer exactly once, and the queue actually created
// must point at the TLS (ipps) record when one is available, since an
// earlier slice already taught this project how to talk to it.
func TestAdoptListsOnePrinterOncePerPhysicalDevice(t *testing.T) {
	const uuid = "cfe92100-67c4-11d4-a45f-e0bb9e00973d"
	candidates := []discovery.Candidate{
		{ServiceName: "EPSON L4260 Series", Model: "EPSON L4260 Series", UUID: uuid, ResourcePath: "ipp/print", URI: "ipp://EPSON00973D.local:631/ipp/print"},
		{ServiceName: "EPSON L4260 Series", Model: "EPSON L4260 Series", UUID: uuid, ResourcePath: "ipp/print", URI: "ipps://EPSON00973D.local:631/ipp/print"},
	}

	admin := &fakeAdmin{}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: candidates}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("1\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if got := strings.Count(out.String(), "EPSON L4260 Series"); got != 2 {
		// Once in the numbered list, once in the final confirmation —
		// never a second line in the list itself for the same printer.
		t.Fatalf("printer's friendly name appears %d times in output, want exactly 2 (one list entry, one confirmation): %q", got, out.String())
	}
	if len(admin.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1: %+v", len(admin.createCalls), admin.createCalls)
	}
	if !strings.HasPrefix(admin.createCalls[0].uri, "ipps://") {
		t.Fatalf("Create was called with uri %q, want the ipps (TLS) record preferred over ipp", admin.createCalls[0].uri)
	}
}

// TestAdoptNeverCollapsesTwoDistinctUnidentifiedPrinters is the mirror
// case of the dedupe fix: two real printers of the same model, neither
// carrying a UUID, must still be offered as two separate choices. The
// repair ladder refuses to guess between exactly this shape of pair
// (see internal/repair's own test of the same name in spirit); the
// dedupe step added for the _ipp/_ipps duplicate must not accidentally
// re-introduce that confusion from the other direction by collapsing
// them into one.
func TestAdoptNeverCollapsesTwoDistinctUnidentifiedPrinters(t *testing.T) {
	candidates := []discovery.Candidate{
		{ServiceName: "Printer (2)", Model: "Example Printer", ResourcePath: "ipp/print", URI: "ipp://192.168.1.50:631/ipp/print"},
		{ServiceName: "Printer (3)", Model: "Example Printer", ResourcePath: "ipp/print", URI: "ipp://192.168.1.51:631/ipp/print"},
	}

	admin := &fakeAdmin{}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: candidates}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("1\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if got := strings.Count(out.String(), "1. Example Printer\n") + strings.Count(out.String(), "2. Example Printer\n"); got != 2 {
		t.Fatalf("want both distinct printers listed, got output: %q", out.String())
	}
}

// TestAdoptNeverCollidesWithAnExistingQueueByCase is an end-to-end
// confirmation of a real bug found against this machine's actual
// cupsd: printer names fold case-insensitively (confirmed by hand —
// "lpstat -p epson_l4260_series" resolves the exact same queue as
// "EPSON_L4260_Series"). The fix lives in queue.SanitiseName itself
// (internal/queue/name_test.go pins the contract directly); this test
// exercises the whole Adopt flow against that fixed contract, so a
// future change that routes around SanitiseName — or a regression in
// it — still gets caught from the one place a user actually
// experiences this.
func TestAdoptNeverCollidesWithAnExistingQueueByCase(t *testing.T) {
	admin := &fakeAdmin{
		queues: []queue.Queue{
			{Name: "EPSON_L4260_Series", DeviceURI: "dnssd://EPSON%20L4260%20Series._ipps._tcp.local./?uuid=cfe92100", Description: ""},
		},
	}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: []discovery.Candidate{
		{Model: "EPSON L4260 Series", URI: "ipp://EPSON00973D.local:631/ipp/print"},
	}}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	// Decline protecting the existing queue, then pick the one
	// freshly-discovered candidate.
	in := strings.NewReader("n\n1\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if len(admin.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1: %+v", len(admin.createCalls), admin.createCalls)
	}
	if len(st.adopted) != 1 {
		t.Fatalf("store has %d adopted entries, want 1", len(st.adopted))
	}
	gotName := st.adopted[0].QueueName
	if strings.EqualFold(gotName, "EPSON_L4260_Series") {
		t.Fatalf("new queue name %q folds to the same cupsd printer name as the untouched existing queue %q", gotName, "EPSON_L4260_Series")
	}
}

// TestAdoptCreateAuthorizationErrorIsHumanized is the CRITICAL finding
// from code review: queue.ErrNeedsAuthorization's own doc comment says
// it exists so the CLI can translate it into plain language instead of
// relaying a raw command failure, but nothing called errors.Is against
// it. A non-technical user whose account is not a CUPS admin would have
// read "queue administration requires authorization: this account is
// not a CUPS admin" on their very first run — leaking "CUPS" at the
// exact moment the product is supposed to be invisible. The error
// Adopt returns here must say what is actually true (this account
// can't do this) and what to do about it, never the mechanism.
func TestAdoptCreateAuthorizationErrorIsHumanized(t *testing.T) {
	admin := &fakeAdmin{createErr: queue.ErrNeedsAuthorization}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: []discovery.Candidate{
		{Model: "Front Desk Printer", URI: "ipp://10.0.0.5:631/ipp/print"},
	}}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	err := app.Adopt(context.Background(), strings.NewReader("1\n"))
	if err == nil {
		t.Fatalf("Adopt() returned no error for a failed Create")
	}
	if strings.Contains(err.Error(), queue.ErrNeedsAuthorization.Error()) {
		t.Fatalf("error leaked the raw sentinel text verbatim: %q", err.Error())
	}
	assertNoDomainVocabulary(t, err.Error())
	if !strings.Contains(err.Error(), "admin") {
		t.Fatalf("error does not say what the person can actually do: %q", err.Error())
	}
}

// TestAdoptCreateUnclassifiedErrorIsStillHumanized covers
// queueErrorMessage's fallback: an lpadmin failure classifyLpadminError
// did not recognise still carries raw command/stderr text, and that
// text must never reach the user either, not just the two named
// sentinels.
func TestAdoptCreateUnclassifiedErrorIsStillHumanized(t *testing.T) {
	rawErr := errors.New("lpadmin -p x -v ipp://10.0.0.5:631/ipp/print: exit status 1: client-error-not-possible")
	admin := &fakeAdmin{createErr: rawErr}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: []discovery.Candidate{
		{Model: "Front Desk Printer", URI: "ipp://10.0.0.5:631/ipp/print"},
	}}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	err := app.Adopt(context.Background(), strings.NewReader("1\n"))
	if err == nil {
		t.Fatalf("Adopt() returned no error for a failed Create")
	}
	if strings.Contains(err.Error(), "lpadmin") || strings.Contains(err.Error(), "exit status") {
		t.Fatalf("error leaked the raw, unclassified command failure text: %q", err.Error())
	}
	assertNoDomainVocabulary(t, err.Error())
}

// TestAdoptListServiceUnavailableErrorIsHumanized covers the other
// sentinel at the other queue.Admin call site this command uses.
func TestAdoptListServiceUnavailableErrorIsHumanized(t *testing.T) {
	admin := &fakeAdmin{listErr: queue.ErrServiceUnavailable}
	st := &fakeStore{}
	disco := fakeDiscoverer{}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	err := app.Adopt(context.Background(), strings.NewReader(""))
	if err == nil {
		t.Fatalf("Adopt() returned no error for a failed List")
	}
	if strings.Contains(err.Error(), queue.ErrServiceUnavailable.Error()) {
		t.Fatalf("error leaked the raw sentinel text verbatim: %q", err.Error())
	}
	assertNoDomainVocabulary(t, err.Error())
}

// TestPlausibleMatchCannotBeReachedByModelSimilarityAlone pins an
// invariant plausibleMatch depends on but does not enforce: two
// candidates built through identity.FromCandidate can only score as
// high as WeightServiceName + WeightResourcePath without a MAC or UUID
// match contributing anything, because FromCandidate never populates
// DeviceID from a bare discovery.Candidate (see its own doc comment in
// internal/identity/fingerprint.go). That sum must stay below
// repair.ThresholdPartial — if it did not, two genuinely different
// physical printers of the same model, sharing nothing but
// model-derived fields, could cross plausibleMatch's bar on model
// similarity alone, and Adopt would repoint a user's existing printer
// configuration onto the wrong device on nothing stronger than "same
// model name".
//
// This mirrors internal/identity's own
// TestWeightUUIDDominatesSumOfOtherWeights for the exact tier: summing
// the named constants directly means a future rebalance of these
// weights — or a change that starts populating DeviceID from browse
// data — fails this test immediately, rather than silently reopening a
// gap nobody making that change would have any reason to think about.
func TestPlausibleMatchCannotBeReachedByModelSimilarityAlone(t *testing.T) {
	reachableWithoutMACOrUUID := identity.WeightServiceName + identity.WeightResourcePath
	if reachableWithoutMACOrUUID >= repair.ThresholdPartial {
		t.Fatalf(
			"WeightServiceName (%d) + WeightResourcePath (%d) = %d, which is >= repair.ThresholdPartial (%d): "+
				"two printers sharing only model-derived fields could now cross plausibleMatch's bar and get "+
				"Adopt to repoint an existing printer onto the wrong physical device",
			identity.WeightServiceName, identity.WeightResourcePath, reachableWithoutMACOrUUID, repair.ThresholdPartial,
		)
	}
}

// TestAdoptCreatesASecondEntryForGenuinelyDifferentSameModelPrinters is
// the behavioural pair to the arithmetic test above, driving the real
// path rather than just the numbers: two printers of the same model,
// sharing the same ServiceName and ResourcePath (the maximum score
// reachable without a MAC or UUID match — the exact worst case the
// arithmetic test bounds) but no MAC or UUID at all, is the shape
// repair's own TestTwoIdenticalModelsWithoutUUIDNeverAutoRepair pins at
// the ladder level. Adopt must create a second, independent
// store.Adopted entry for the new one — never repoint the first
// printer's queue onto what might be a different physical device.
func TestAdoptCreatesASecondEntryForGenuinelyDifferentSameModelPrinters(t *testing.T) {
	adopted := store.Adopted{
		QueueName:   "Example_Printer",
		Fingerprint: identity.Fingerprint{ServiceName: "Example Printer", ResourcePath: "ipp/print"},
	}
	admin := &fakeAdmin{
		queues: []queue.Queue{
			{Name: "Example_Printer", DeviceURI: unreachableURI, Description: "Example Printer"},
		},
	}
	st := &fakeStore{adopted: []store.Adopted{adopted}}
	// Shares ServiceName and ResourcePath with the stored fingerprint —
	// the maximum this candidate can score without a MAC or UUID in
	// common — but is otherwise a different device entirely (different
	// host, no UUID).
	candidate := discovery.Candidate{ServiceName: "Example Printer", Model: "Example Printer", ResourcePath: "ipp/print", Host: "192.168.1.60", URI: "ipp://192.168.1.60:631/ipp/print"}
	disco := fakeDiscoverer{candidates: []discovery.Candidate{candidate}}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("1\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if len(st.adopted) != 2 {
		t.Fatalf("store has %d adopted entries, want 2: a genuinely different same-model printer must get its own entry, not repoint the first: %+v", len(st.adopted), st.adopted)
	}
	if len(admin.setCalls) != 0 {
		t.Fatalf("SetDeviceURI was called (%v); the original printer's queue must not be touched by an unrelated same-model printer", admin.setCalls)
	}
	if len(admin.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1: %+v", len(admin.createCalls), admin.createCalls)
	}
}

// TestAdoptResolvesAnAmbiguousMatchInPlaceRatherThanDuplicating is the
// fix for a finding from code review: ActionAsk's advice ("set it up
// again") is only true if running Adopt afterward actually resolves the
// ambiguity rather than creating a second queue for the same physical
// printer. Traced directly (not reasoned from the code) before this fix
// existed: picking either of two Ask-tier candidates created a second
// store.Adopted entry and a second queue, leaving the original,
// unresolved entry sitting there untouched — worse than the question it
// replaced. Picking a candidate that plausibly (but not exactly) matches
// an already-adopted printer must now repoint that printer's existing
// queue instead of creating a new one, and must leave exactly one
// store.Adopted entry for it afterward.
func TestAdoptResolvesAnAmbiguousMatchInPlaceRatherThanDuplicating(t *testing.T) {
	// Fingerprint carries a MAC, matching what identity.MACFromUUID
	// derives from each candidate's version-1 printer-uuid node field
	// below (e0bb9e00973d) — WeightMAC (60) alone clears
	// repair.ThresholdPartial (40) without clearing identity.WeightUUID
	// (200), which is exactly the plausible-but-not-exact shape
	// ActionAsk fires on. Two different candidates deliberately share
	// that same node field, so neither one's UUID agrees with the
	// other — genuine ambiguity, not a fixture that happens to look
	// ambiguous.
	adopted := store.Adopted{
		QueueName:   "Example_Printer",
		Fingerprint: identity.Fingerprint{MAC: "e0:bb:9e:00:97:3d"},
	}
	admin := &fakeAdmin{
		queues: []queue.Queue{
			{Name: "Example_Printer", DeviceURI: unreachableURI, Description: "Example Printer"},
		},
	}
	st := &fakeStore{adopted: []store.Adopted{adopted}}
	candidateA := discovery.Candidate{ServiceName: "Printer (2)", Model: "Example Printer", UUID: "11111111-2222-11ec-8000-e0bb9e00973d", Host: "192.168.1.50", URI: "ipp://192.168.1.50:631/ipp/print"}
	candidateB := discovery.Candidate{ServiceName: "Printer (3)", Model: "Example Printer", UUID: "22222222-3333-11ec-8000-e0bb9e00973d", Host: "192.168.1.51", URI: "ipp://192.168.1.51:631/ipp/print"}
	disco := fakeDiscoverer{candidates: []discovery.Candidate{candidateA, candidateB}}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("1\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if len(st.adopted) != 1 {
		t.Fatalf("store has %d adopted entries after resolving an ambiguous match, want exactly 1 (no duplicate): %+v", len(st.adopted), st.adopted)
	}
	if st.adopted[0].QueueName != "Example_Printer" {
		t.Fatalf("resolved entry's QueueName = %q, want the original %q (preserved, never renamed)", st.adopted[0].QueueName, "Example_Printer")
	}
	if len(admin.createCalls) != 0 {
		t.Fatalf("Create was called (%v); resolving an ambiguous match must repoint the existing queue, never create a second one", admin.createCalls)
	}
	if len(admin.setCalls) != 1 || admin.setCalls[0].uri != candidateA.URI {
		t.Fatalf("SetDeviceURI calls = %+v, want exactly one call pointing the existing queue at %q", admin.setCalls, candidateA.URI)
	}
}

// TestAdoptOffersToProtectExistingQueuesOnFirstRun is spec §11.2: a
// fresh install with no adopted printers, but a queue already
// configured on the machine, must offer to protect it before (or
// instead of, if declined) offering newly discovered printers.
func TestAdoptOffersToProtectExistingQueuesOnFirstRun(t *testing.T) {
	admin := &fakeAdmin{
		queues: []queue.Queue{{Name: "Existing_Printer", DeviceURI: "ipp://127.0.0.1:1/ipp/print", Description: "Existing Printer"}},
	}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: nil}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("y\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if !strings.Contains(out.String(), "Existing Printer") {
		t.Fatalf("did not offer to protect the existing queue: %q", out.String())
	}
	assertNoDomainVocabulary(t, out.String())

	if len(st.adopted) != 1 || st.adopted[0].QueueName != "Existing_Printer" {
		t.Fatalf("store.adopted = %+v, want exactly the existing queue protected", st.adopted)
	}
}

// TestAdoptDeclinedExistingQueueIsNotProtected confirms the other half:
// declining the offer leaves the existing queue completely untouched —
// the global constraint "never act on a queue the user did not adopt".
func TestAdoptDeclinedExistingQueueIsNotProtected(t *testing.T) {
	admin := &fakeAdmin{
		queues: []queue.Queue{{Name: "Existing_Printer", DeviceURI: "ipp://127.0.0.1:1/ipp/print", Description: "Existing Printer"}},
	}
	st := &fakeStore{}
	disco := fakeDiscoverer{candidates: nil}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	in := strings.NewReader("n\n")
	if err := app.Adopt(context.Background(), in); err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	if len(st.adopted) != 0 {
		t.Fatalf("store.adopted = %+v, want nothing protected after declining", st.adopted)
	}
	if len(admin.setCalls) != 0 || len(admin.deleteCalls) != 0 {
		t.Fatalf("Adopt touched the declined queue: set=%v delete=%v", admin.setCalls, admin.deleteCalls)
	}
}

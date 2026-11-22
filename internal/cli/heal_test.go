package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/store"
)

// unreachableURI points nowhere a probe can reach: nothing listens on
// port 1, so the connection is refused instantly and deterministically —
// the same fixture internal/repair's own tests use for a reliable
// StateAbsent without touching a real network.
const unreachableURI = "ipp://127.0.0.1:1/ipp/print"

// fakeDiscoverer is a discovery.Discoverer double that returns a fixed
// set of candidates (or a fixed error) without touching the network.
type fakeDiscoverer struct {
	candidates []discovery.Candidate
	err        error
}

func (f fakeDiscoverer) Browse(ctx context.Context, timeout time.Duration) ([]discovery.Candidate, error) {
	return f.candidates, f.err
}

// setDeviceURICall records one SetDeviceURI invocation, so a test can
// assert not just that it was called, but with exactly which arguments —
// the distinction that matters when the first anti-goal is repairing the
// wrong queue.
type setDeviceURICall struct {
	name string
	uri  string
}

// fakeAdmin is a queue.Admin double whose List is fixed by the test and
// whose Create/SetDeviceURI/Delete calls are all recorded rather than
// acted on, so a test can assert exactly what Heal (or Adopt) did
// without a real cupsd.
type fakeAdmin struct {
	queues []queue.Queue

	createCalls  []setDeviceURICall // name+uri reused for Create's (name, uri) pair
	setCalls     []setDeviceURICall
	deleteCalls  []string
	createErr    error
	setDeviceErr error
	listErr      error
}

func (f *fakeAdmin) List(ctx context.Context) ([]queue.Queue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.queues, nil
}

func (f *fakeAdmin) Create(ctx context.Context, name, uri, description string) error {
	f.createCalls = append(f.createCalls, setDeviceURICall{name: name, uri: uri})
	return f.createErr
}

func (f *fakeAdmin) SetDeviceURI(ctx context.Context, name, uri string) error {
	f.setCalls = append(f.setCalls, setDeviceURICall{name: name, uri: uri})
	return f.setDeviceErr
}

func (f *fakeAdmin) Delete(ctx context.Context, name string) error {
	f.deleteCalls = append(f.deleteCalls, name)
	return nil
}

// fakeStore is a store.Store double backed by an in-memory slice.
type fakeStore struct {
	adopted []store.Adopted
	saved   [][]store.Adopted
}

func (f *fakeStore) Load() ([]store.Adopted, error) {
	return f.adopted, nil
}

func (f *fakeStore) Save(a []store.Adopted) error {
	f.saved = append(f.saved, a)
	f.adopted = a
	return nil
}

// fakeKeyer is a store.NetworkKeyer double returning a fixed key.
type fakeKeyer struct{ key string }

func (f fakeKeyer) Key() string { return f.key }

// TestHealRepairsSilentlyAndWritesNothing is the brief's Step 4,
// verbatim: a fake Admin whose List returns an adopted queue, a fake
// Discoverer returning the same printer (by UUID) at a new address, and
// a probe reporting absent (the queue's current address is
// unreachableURI) must make Heal call SetDeviceURI exactly once with the
// new address, write nothing to Out, and never call Delete.
//
// The silence assertion alone would pass against a Heal that does
// nothing at all in every situation — this test also pins the
// SetDeviceURI call, which a no-op Heal cannot produce, so the two
// assertions together rule that out.
func TestHealRepairsSilentlyAndWritesNothing(t *testing.T) {
	const uuid = "cfe92100-67c4-11d4-a45f-e0bb9e00973d"
	const newURI = "ipp://192.168.1.99:631/ipp/print"

	adopted := store.Adopted{
		QueueName:   "Reception_Printer",
		Fingerprint: identity.FromCandidate(discovery.Candidate{UUID: uuid, Model: "Reception Printer"}),
		AdoptedAt:   time.Now().Add(-24 * time.Hour),
		LastSeen: map[string]store.Seen{
			"home": {URI: unreachableURI, At: time.Now().Add(-time.Hour)},
		},
	}

	admin := &fakeAdmin{
		queues: []queue.Queue{
			{Name: "Reception_Printer", DeviceURI: unreachableURI, Description: "Reception Printer"},
		},
	}
	st := &fakeStore{adopted: []store.Adopted{adopted}}
	disco := fakeDiscoverer{candidates: []discovery.Candidate{
		{UUID: uuid, Model: "Reception Printer", URI: newURI},
	}}

	var out bytes.Buffer
	app := &App{
		Disco:  disco,
		Queues: admin,
		Store:  st,
		Keyer:  fakeKeyer{key: "home"},
		Out:    &out,
	}

	if err := app.Heal(context.Background()); err != nil {
		t.Fatalf("Heal() error = %v", err)
	}

	if len(admin.setCalls) != 1 {
		t.Fatalf("SetDeviceURI called %d times, want exactly 1: %+v", len(admin.setCalls), admin.setCalls)
	}
	want := setDeviceURICall{name: "Reception_Printer", uri: newURI}
	if admin.setCalls[0] != want {
		t.Fatalf("SetDeviceURI call = %+v, want %+v", admin.setCalls[0], want)
	}
	if len(admin.deleteCalls) != 0 {
		t.Fatalf("Delete was called (%v); Heal must never delete a queue", admin.deleteCalls)
	}
	if out.Len() != 0 {
		t.Fatalf("Heal wrote %q to Out; a successful silent repair must write nothing", out.String())
	}
}

// TestHealSetDeviceURIAuthorizationErrorIsHumanized is the CRITICAL
// finding from code review applied to Heal's own queue.Admin call:
// a repair that fails because this account cannot administer queues
// must not surface "this account is not a CUPS admin" to whatever
// reads Heal's returned error (main.go prints it to stderr verbatim).
func TestHealSetDeviceURIAuthorizationErrorIsHumanized(t *testing.T) {
	const uuid = "cfe92100-67c4-11d4-a45f-e0bb9e00973d"
	const newURI = "ipp://192.168.1.99:631/ipp/print"

	adopted := store.Adopted{
		QueueName:   "Reception_Printer",
		Fingerprint: identity.FromCandidate(discovery.Candidate{UUID: uuid, Model: "Reception Printer"}),
		LastSeen: map[string]store.Seen{
			"home": {URI: unreachableURI, At: time.Now().Add(-time.Hour)},
		},
	}
	admin := &fakeAdmin{
		queues:       []queue.Queue{{Name: "Reception_Printer", DeviceURI: unreachableURI, Description: "Reception Printer"}},
		setDeviceErr: queue.ErrNeedsAuthorization,
	}
	st := &fakeStore{adopted: []store.Adopted{adopted}}
	disco := fakeDiscoverer{candidates: []discovery.Candidate{{UUID: uuid, Model: "Reception Printer", URI: newURI}}}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	err := app.Heal(context.Background())
	if err == nil {
		t.Fatalf("Heal() returned no error for a failed SetDeviceURI")
	}
	if strings.Contains(err.Error(), queue.ErrNeedsAuthorization.Error()) {
		t.Fatalf("error leaked the raw sentinel text verbatim: %q", err.Error())
	}
	assertNoDomainVocabulary(t, err.Error())
}

// TestHealListServiceUnavailableErrorIsHumanized covers Heal's other
// queue.Admin call site, List.
func TestHealListServiceUnavailableErrorIsHumanized(t *testing.T) {
	admin := &fakeAdmin{listErr: queue.ErrServiceUnavailable}
	st := &fakeStore{}
	disco := fakeDiscoverer{}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	err := app.Heal(context.Background())
	if err == nil {
		t.Fatalf("Heal() returned no error for a failed List")
	}
	if strings.Contains(err.Error(), queue.ErrServiceUnavailable.Error()) {
		t.Fatalf("error leaked the raw sentinel text verbatim: %q", err.Error())
	}
	assertNoDomainVocabulary(t, err.Error())
}

// TestHealReportsWhenNothingPlausibleIsFound is the complementary case
// to the silence test above: it proves Heal is not silent because it
// never writes anything, by exercising a path where it must. An absent
// printer with no plausible candidate anywhere must be reported, in
// plain language, and must still never touch SetDeviceURI or Delete.
func TestHealReportsWhenNothingPlausibleIsFound(t *testing.T) {
	adopted := store.Adopted{
		QueueName:   "Lobby_Printer",
		Fingerprint: identity.FromCandidate(discovery.Candidate{UUID: "11111111-2222-3333-4444-555555555555", ServiceName: "Lobby Printer"}),
		AdoptedAt:   time.Now().Add(-24 * time.Hour),
		LastSeen: map[string]store.Seen{
			"home": {URI: unreachableURI, At: time.Now().Add(-time.Hour)},
		},
	}

	admin := &fakeAdmin{
		queues: []queue.Queue{
			{Name: "Lobby_Printer", DeviceURI: unreachableURI, Description: "Lobby Printer"},
		},
	}
	st := &fakeStore{adopted: []store.Adopted{adopted}}
	disco := fakeDiscoverer{candidates: nil} // nothing on the network at all

	var out bytes.Buffer
	app := &App{
		Disco:  disco,
		Queues: admin,
		Store:  st,
		Keyer:  fakeKeyer{key: "home"},
		Out:    &out,
	}

	if err := app.Heal(context.Background()); err != nil {
		t.Fatalf("Heal() error = %v", err)
	}

	if len(admin.setCalls) != 0 {
		t.Fatalf("SetDeviceURI was called (%v); nothing plausible was found to repoint to", admin.setCalls)
	}
	if len(admin.deleteCalls) != 0 {
		t.Fatalf("Delete was called (%v); Heal must never delete a queue", admin.deleteCalls)
	}
	if out.Len() == 0 {
		t.Fatalf("Heal wrote nothing to Out; an unresolvable absence must be reported")
	}
	assertNoDomainVocabulary(t, out.String())
}

// TestHealNeverActsOnAQueueTheUserDidNotAdopt is the CLI-layer half of
// the global constraint "never act on a queue the user did not adopt":
// a queue present on the machine but absent from the store must produce
// no SetDeviceURI, Create, or Delete call at all.
func TestHealNeverActsOnAQueueTheUserDidNotAdopt(t *testing.T) {
	admin := &fakeAdmin{
		queues: []queue.Queue{
			{Name: "Hand_Made_Printer", DeviceURI: unreachableURI, Description: "Hand Made Printer"},
		},
	}
	st := &fakeStore{adopted: nil}
	disco := fakeDiscoverer{candidates: nil}

	var out bytes.Buffer
	app := &App{Disco: disco, Queues: admin, Store: st, Keyer: fakeKeyer{key: "home"}, Out: &out}

	if err := app.Heal(context.Background()); err != nil {
		t.Fatalf("Heal() error = %v", err)
	}
	if len(admin.setCalls) != 0 || len(admin.createCalls) != 0 || len(admin.deleteCalls) != 0 {
		t.Fatalf("Heal acted on an unadopted queue: set=%v create=%v delete=%v", admin.setCalls, admin.createCalls, admin.deleteCalls)
	}
	if out.Len() != 0 {
		t.Fatalf("Heal wrote %q to Out for a queue nothing was adopted about", out.String())
	}
}

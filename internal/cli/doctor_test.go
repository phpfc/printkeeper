package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/identity"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/store"
)

// TestDoctorReportsAbsentPrinterByFriendlyName confirms Doctor always
// reports — it is a deliberate "how is my printer doing" check, so
// unlike Heal it must never be silent, even when nothing is wrong to
// report beyond "did not answer".
func TestDoctorReportsAbsentPrinterByFriendlyName(t *testing.T) {
	adopted := store.Adopted{
		QueueName:   "Reception_Printer",
		Fingerprint: identity.FromCandidate(discovery.Candidate{ServiceName: "Reception Printer"}),
	}
	admin := &fakeAdmin{
		queues: []queue.Queue{{Name: "Reception_Printer", DeviceURI: unreachableURI, Description: "Reception Printer"}},
	}
	st := &fakeStore{adopted: []store.Adopted{adopted}}

	var out bytes.Buffer
	app := &App{Queues: admin, Store: st, Out: &out}

	if err := app.Doctor(context.Background()); err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}

	if !strings.Contains(out.String(), "Reception Printer") {
		t.Fatalf("Doctor output does not name the printer: %q", out.String())
	}
	assertNoDomainVocabulary(t, out.String())
}

// TestDoctorWithNothingAdoptedSaysSo covers the fresh-install case:
// Doctor must say something sensible, not silently print nothing or
// crash, when the store is empty.
func TestDoctorWithNothingAdoptedSaysSo(t *testing.T) {
	st := &fakeStore{}
	var out bytes.Buffer
	app := &App{Queues: &fakeAdmin{}, Store: st, Out: &out}

	if err := app.Doctor(context.Background()); err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if out.Len() == 0 {
		t.Fatalf("Doctor wrote nothing for an empty store; it must say something")
	}
	assertNoDomainVocabulary(t, out.String())
}

// TestDoctorListServiceUnavailableErrorIsHumanized is the CRITICAL
// finding from code review applied to Doctor's own queue.Admin call:
// cupsd being unreachable must not surface as a raw command-failure
// string on Doctor's returned error either.
func TestDoctorListServiceUnavailableErrorIsHumanized(t *testing.T) {
	adopted := store.Adopted{QueueName: "Reception_Printer"}
	admin := &fakeAdmin{listErr: queue.ErrServiceUnavailable}
	st := &fakeStore{adopted: []store.Adopted{adopted}}

	var out bytes.Buffer
	app := &App{Queues: admin, Store: st, Out: &out}

	err := app.Doctor(context.Background())
	if err == nil {
		t.Fatalf("Doctor() returned no error for a failed List")
	}
	if strings.Contains(err.Error(), queue.ErrServiceUnavailable.Error()) {
		t.Fatalf("error leaked the raw sentinel text verbatim: %q", err.Error())
	}
	assertNoDomainVocabulary(t, err.Error())
}

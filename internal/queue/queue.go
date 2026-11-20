// Package queue is the only package in printkeeper that writes to the
// user's machine: it creates, repairs, and deletes real CUPS print
// queues by way of lpadmin, and reads live queue state by way of IPP's
// CUPS-Get-Printers operation. It never shells out to lpstat — lpstat's
// output is a sentence in whatever locale the user's shell is set to
// ("impressora spike_test está ociosa" on this machine), and anything
// parsed from that sentence breaks the moment the locale changes.
//
// The repair ladder in a later task decides whether to act on a queue;
// Admin is how that decision gets carried out.
package queue

import (
	"context"
	"errors"
)

// Queue is one CUPS print queue as reported by Admin.List.
type Queue struct {
	Name        string
	DeviceURI   string
	Description string
	Accepting   bool

	// State is the raw IPP printer-state enum (RFC 8011 §5.4.11) that
	// cupsd reported, exactly as goipp decodes it: the bare decimal
	// ordinal of the enum value, as a string — "3", "4", or "5" — never
	// the word "idle", "processing", or "stopped". Compare it against
	// StateIdle, StateProcessing, or StateStopped below; a comparison
	// against a literal word like "stopped" will compile, and will
	// never match.
	State string
}

// StateIdle, StateProcessing, and StateStopped are the values Queue.State
// can carry — the three printer-state enum ordinals RFC 8011 §5.4.11
// defines, as the decimal strings goipp.Integer.String() produces for
// them. They exist so callers never need to invent the mapping (or the
// word) themselves.
const (
	StateIdle       = "3"
	StateProcessing = "4"
	StateStopped    = "5"
)

// Admin creates, repairs, and deletes CUPS print queues on the host.
type Admin interface {
	// List reports every print queue currently configured on the host.
	List(ctx context.Context) ([]Queue, error)

	// Create adds a new driverless ("everywhere") queue named name,
	// pointing at uri, with description as its human-readable info. The
	// name becomes permanent the moment this succeeds: it is what the
	// user sees in every print dialog for as long as the queue exists,
	// and the daemon can never rename it afterward.
	Create(ctx context.Context, name, uri, description string) error

	// SetDeviceURI repoints an existing queue's device address, leaving
	// its name untouched.
	SetDeviceURI(ctx context.Context, name, uri string) error

	// Delete removes a queue entirely. Callers must only ever delete a
	// queue the daemon itself created and the user adopted — Admin does
	// not enforce that; it trusts the caller.
	Delete(ctx context.Context, name string) error
}

// ErrNeedsAuthorization means lpadmin reported that administering queues
// requires a privilege the current account does not hold. Spec §5 scopes
// v1 to detecting and explaining this rather than shipping a privileged
// helper, so this error exists for the CLI to translate into plain
// language instead of relaying a raw command failure.
var ErrNeedsAuthorization = errors.New("queue administration requires authorization: this account is not a CUPS admin")

// ErrServiceUnavailable means lpadmin could not reach cupsd at all.
var ErrServiceUnavailable = errors.New("the CUPS service is not reachable")

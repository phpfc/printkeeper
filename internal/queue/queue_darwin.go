package queue

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/OpenPrinting/goipp"
	"github.com/phpfc/printkeeper/internal/ippclient"
)

// cupsBaseURI is the local cupsd this adapter administers. printkeeper
// only ever manages the queues on the machine it runs on. It is a var,
// not a const — mirroring lpadminPath below — so a unit test can point
// List at an httptest.Server serving a canned CUPS-Get-Printers
// response instead of this machine's real cupsd.
var cupsBaseURI = "ipp://localhost:631/"

// lpadminPath is absolute, never looked up on PATH, so this adapter's
// behaviour does not depend on the caller's shell environment. It is a
// var, not a const, so a test could point it at a stub.
var lpadminPath = "/usr/sbin/lpadmin"

// listAttributes are the CUPS-Get-Printers attribute keywords List asks
// for — exactly what Queue needs, nothing more.
var listAttributes = []string{
	"printer-name",
	"device-uri",
	"printer-info",
	"printer-is-accepting-jobs",
	"printer-state",
}

// successStatusCeiling is one past the end of IPP's "successful" status
// range (RFC 8011 §13.1): statuses below 0x0100 are success, 0x0200 and
// above are client or server error.
const successStatusCeiling = goipp.Status(0x0100)

func isSuccessStatus(status goipp.Status) bool {
	return status < successStatusCeiling
}

type darwinAdmin struct{}

// NewDarwinAdmin returns an Admin backed by this machine's cupsd: reads go
// over IPP, writes go through lpadmin.
func NewDarwinAdmin() Admin {
	return darwinAdmin{}
}

// List issues a CUPS-Get-Printers request to the local cupsd over IPP —
// never lpstat, whose output is localized prose, not data. A host with no
// queues configured answers with IPP status client-error-not-found rather
// than an empty successful response; that is reported here as an empty
// slice, not an error. Each returned Queue's State is the raw IPP
// printer-state enum ordinal as a string (see StateIdle, StateProcessing,
// StateStopped), not a word.
func (darwinAdmin) List(ctx context.Context) ([]Queue, error) {
	groups, status, err := ippclient.CupsGetPrinters(ctx, cupsBaseURI, listAttributes)
	if err != nil {
		return nil, fmt.Errorf("list queues: %w", err)
	}
	if status == goipp.StatusErrorNotFound {
		return []Queue{}, nil
	}
	if !isSuccessStatus(status) {
		return nil, fmt.Errorf("list queues: cupsd returned ipp status %s", status)
	}

	queues := make([]Queue, 0, len(groups))
	for _, attrs := range groups {
		queues = append(queues, Queue{
			Name:        firstValue(attrs, "printer-name"),
			DeviceURI:   firstValue(attrs, "device-uri"),
			Description: firstValue(attrs, "printer-info"),
			Accepting:   firstValue(attrs, "printer-is-accepting-jobs") == "true",
			State:       firstValue(attrs, "printer-state"),
		})
	}
	return queues, nil
}

// Create adds a new driverless ("everywhere") queue. name is assumed
// already sanitised by SanitiseName — Create does not sanitise it again,
// since doing so here, against a taken list Create has no access to,
// could silently produce a different name than the one the caller thinks
// it just created.
func (darwinAdmin) Create(ctx context.Context, name, uri, description string) error {
	return runLpadmin(ctx, "-p", name, "-E", "-v", uri, "-m", "everywhere", "-D", description)
}

// SetDeviceURI repoints an existing queue's device address without
// touching its name — the name is permanent once Create has run.
func (darwinAdmin) SetDeviceURI(ctx context.Context, name, uri string) error {
	return runLpadmin(ctx, "-p", name, "-v", uri)
}

// Delete removes a queue entirely.
func (darwinAdmin) Delete(ctx context.Context, name string) error {
	return runLpadmin(ctx, "-x", name)
}

// runLpadmin runs lpadmin with args, capturing stderr so a non-zero exit
// can be classified into a human-actionable error rather than surfaced as
// a bare command failure.
func runLpadmin(ctx context.Context, args ...string) error {
	cmd := buildLpadminCmd(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if classified := classifyLpadminError(stderr.String()); classified != nil {
			return classified
		}
		return fmt.Errorf("lpadmin %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// buildLpadminCmd constructs (but does not run) the lpadmin command for
// args, with its environment forced to the C locale. Every lpadmin
// invocation goes through this one seam — not only the ones whose
// stderr classifyLpadminError inspects today — so nothing calling
// lpadmin later can reintroduce a locale-dependent parse by accident.
//
// Forcing the locale, rather than assuming the host happens to be
// English, is what makes classifyLpadminError's keyword matching sound:
// CUPS's own CLI tools translate their messages through locale-specific
// catalogs keyed off LC_ALL/LANG — a spike confirmed this directly,
// `lpstat` on this very machine printing "impressora spike_test está
// ociosa" — and lpadmin's stderr goes through the same mechanism.
// Without this, a standard user's authorization failure (exactly the
// case classifyLpadminError exists to recognise) would silently fall
// through to a generic error on any non-English system.
func buildLpadminCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, lpadminPath, args...)
	cmd.Env = cLocaleEnv()
	return cmd
}

// cLocaleEnv returns the current process's environment with LC_ALL and
// LANG forced to "C", dropping any inherited values for those keys first
// rather than merely appending — os.Environ()-style env slices resolve a
// key to its first match, so a stale inherited entry left in place could
// still win over an override appended after it.
func cLocaleEnv() []string {
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+2)
	for _, kv := range inherited {
		if strings.HasPrefix(kv, "LC_ALL=") || strings.HasPrefix(kv, "LANG=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C", "LANG=C")
}

// classifyLpadminError maps lpadmin's captured stderr — forced to the C
// locale by buildLpadminCmd, so this text is deterministic untranslated
// English by construction, not by assuming the operator's machine
// happens to be set to English — to one of this package's sentinel
// errors, so the CLI can explain a failure in plain language instead of
// relaying a raw command failure. It returns nil when stderr does not
// match a known case, leaving the caller to report the raw failure.
//
// This is the one place in the package that inspects a CLI tool's
// output text, and it is deliberately narrow: it looks for the IPP
// status keyword cupsd's own error message always carries ("forbidden",
// "not authorized") rather than trying to match a full sentence.
func classifyLpadminError(stderr string) error {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "not authorized"), strings.Contains(lower, "not-authorized"), strings.Contains(lower, "forbidden"):
		return ErrNeedsAuthorization
	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "unable to connect"),
		strings.Contains(lower, "could not connect"):
		return ErrServiceUnavailable
	default:
		return nil
	}
}

// firstValue returns the first value reported for name, or "" if absent.
func firstValue(attrs map[string][]string, name string) string {
	if v := attrs[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

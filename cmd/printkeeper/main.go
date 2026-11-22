// Command printkeeper is the CLI client for the printkeeper daemon. It
// carries "inspect", the validation tool that captures everything
// knowable about a real printer so identity-matching weights can be
// designed against measured data rather than guessed field shapes, and
// the three commands a person actually runs day to day: "adopt" (choose
// a printer to protect, from a numbered list — never by typing an
// address), "doctor" (check on adopted printers), and "heal" (repair any
// that moved).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/phpfc/printkeeper/internal/cli"
	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/inspect"
	"github.com/phpfc/printkeeper/internal/ipc"
	"github.com/phpfc/printkeeper/internal/resolve"
	"github.com/phpfc/printkeeper/internal/store"
)

const (
	browseTimeout = 5 * time.Second
	probeTimeout  = 10 * time.Second
	fixturesDir   = "testdata/fixtures"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "inspect":
		err = runInspect(os.Args[2:])
	case "adopt":
		err = runAdopt(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "heal":
		err = runHeal(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "printkeeper:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: printkeeper <adopt|doctor|heal|inspect> [--json] [uri]")
}

// runAdopt implements `printkeeper adopt`: the only command that ever
// asks the user to type anything, and the only thing it asks for is a
// small number picked from a list of human-readable printer names.
// Unlike doctor and heal, this never prefers the daemon: it is the one
// command that reads from a terminal (os.Stdin), and the daemon's IPC
// protocol carries a single JSON request, not an interactive stream —
// see internal/ipc's own handling of the "adopt" method, which always
// declines for exactly this reason. It always runs in-process.
func runAdopt(args []string) error {
	app, err := newApp()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), adoptTimeout)
	defer cancel()
	return app.Adopt(ctx, os.Stdin)
}

// runDoctor implements `printkeeper doctor`: a direct, always-reported
// health check of every adopted printer. It prefers asking a running
// daemon over the IPC socket — the daemon's own view is at least as
// fresh, and this spares a printer two cold-start network round trips
// instead of one — and falls back to running the same check in-process
// whenever no daemon answers.
func runDoctor(args []string) error {
	if output, ok, err := runViaDaemon("status"); ok {
		if err != nil {
			return err
		}
		fmt.Print(output)
		return nil
	}

	app, err := newApp()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	return app.Doctor(ctx)
}

// runHeal implements `printkeeper heal`: run the repair confidence
// ladder against every adopted printer and act on what it decides. A
// successful repair writes nothing — spec §2 goal 3. Like runDoctor, it
// prefers a running daemon over the socket and falls back to running
// in-process when none answers.
func runHeal(args []string) error {
	if output, ok, err := runViaDaemon("heal"); ok {
		if err != nil {
			return err
		}
		fmt.Print(output)
		return nil
	}

	app, err := newApp()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), healTimeout)
	defer cancel()
	return app.Heal(ctx)
}

// runViaDaemon attempts method ("status" or "heal") against a running
// daemon's socket. ok is false whenever no daemon answered at all — a
// missing socket (nothing running), a refused connection (a stale
// socket file left by a daemon that crashed), a connection that
// accepted but then went silent or dropped mid-request, a read that
// hit the client's own deadline (internal/ipc.Client never blocks
// forever; see its doc comment) — anything internal/ipc.TransportError
// wraps — in which case the caller falls back to running the same
// command in-process; none of those are reported as errors, since
// "the daemon didn't answer" is indistinguishable, from the user's own
// point of view, from "there is no daemon right now", which is itself
// the ordinary, expected case for anyone who has not installed the
// launchd agent. A reviewer's finding was that this used to only be
// true for Dial itself: once a daemon had been successfully dialled,
// any later failure — including a transport failure that looks
// exactly like no daemon at all — was treated as a real error instead
// of falling back, which defeated the entire point of preferring the
// daemon when one answers and running in-process when one doesn't.
//
// Once the daemon has genuinely *answered* — even with an
// application-level failure carried in Response.Error, not a
// transport one — that answer is real and is returned rather than
// silently swallowed into another in-process attempt that would just
// repeat the same failure.
func runViaDaemon(method string) (output string, ok bool, err error) {
	path, pathErr := socketPath()
	if pathErr != nil {
		return "", false, nil
	}

	client, dialErr := ipc.Dial(path)
	if dialErr != nil {
		return "", false, nil
	}
	defer client.Close()

	switch method {
	case "status":
		output, err = client.Status()
	case "heal":
		output, err = client.Heal()
	default:
		return "", false, nil
	}

	var transportErr *ipc.TransportError
	if errors.As(err, &transportErr) {
		return "", false, nil
	}
	return output, true, err
}

// adoptTimeout, doctorTimeout, and healTimeout bound each command's
// whole run (browsing plus however many printers it probes), not any
// single network call within it — those are bounded individually inside
// internal/cli.
const (
	adoptTimeout  = 30 * time.Second
	doctorTimeout = 30 * time.Second
	healTimeout   = 60 * time.Second
)

// newApp wires every port into a *cli.App using this platform's real
// adapters: Bonjour discovery, the local cupsd, and a JSON store at
// storePath. It is the one place production ties these together — every
// internal/cli test uses fakes instead.
func newApp() (*cli.App, error) {
	path, err := storePath()
	if err != nil {
		return nil, fmt.Errorf("find where to keep adopted printers: %w", err)
	}
	disco := newDiscoverer()
	return &cli.App{
		Disco:      disco,
		Queues:     newAdmin(),
		Store:      store.NewFileStore(path),
		Keyer:      newNetworkKeyer(),
		Out:        os.Stdout,
		Strategies: resolve.DefaultStrategies(disco),
	}, nil
}

// storePath returns where this machine's adopted-printer store lives:
// a single JSON file under the current user's per-user application
// data directory, created on first write with permissions that reflect
// what it records (store.NewFileStore handles that part).
func storePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the current user's home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "printkeeper", "adopted.json"), nil
}

// socketPath returns where the daemon's IPC socket lives if one is
// running: the same per-user Application Support directory storePath
// uses, so both binaries agree on one location without either
// importing the other's unexported path logic (cmd/printkeeperd
// computes this identically in its own appSupportDir).
func socketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the current user's home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "printkeeper", "sock"), nil
}

// runInspect implements `printkeeper inspect [--json] [uri]`. With no uri
// it browses the network and inspects every candidate found; with one, it
// inspects only that address. --json writes an anonymised fixture per
// printer instead of printing a human-readable summary.
func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "write an anonymised fixture to testdata/fixtures/<model>.json instead of printing a summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	candidates, err := candidatesToInspect(fs.Arg(0))
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("no printers found")
		return nil
	}

	for _, c := range candidates {
		probeCtx, probeCancel := probeContext(probeTimeout)
		report, err := inspect.Inspect(probeCtx, c.URI)
		probeCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "printkeeper: inspect %s: %v\n", c.URI, err)
			continue
		}
		report.Candidate = c

		if *jsonOut {
			if err := writeFixture(report); err != nil {
				return err
			}
		} else {
			printSummary(report)
		}
	}

	return nil
}

// probeContext returns a context for probing exactly one candidate, with
// its own full, independent timeout window. It takes no parent context at
// all — every call derives straight from context.Background() — because
// deriving every candidate's probe deadline from one context shared across
// candidates meant a slow or unresponsive printer could starve the probe
// window of every printer discovered after it (found by code review, not
// by a test). timeout is an explicit parameter, rather than probeContext
// reading the probeTimeout constant itself, so a test can call this exact
// function at a small scale and make that starvation visible in
// milliseconds instead of needing to wait out the real multi-second
// production timeout — see probeContext's test for the regression this
// guards.
func probeContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// candidatesToInspect resolves what to inspect: the single uri if given, or
// every candidate a network browse finds. Browsing gets its own context,
// scoped to exactly the browse phase — independent of probeContext, which
// is scoped per candidate.
func candidatesToInspect(uri string) ([]discovery.Candidate, error) {
	if uri != "" {
		return []discovery.Candidate{{URI: uri}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), browseTimeout)
	defer cancel()

	d := newDiscoverer()
	found, err := d.Browse(ctx, browseTimeout)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	return found, nil
}

func writeFixture(report inspect.Report) error {
	anon := inspect.Anonymise(report)

	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", fixturesDir, err)
	}

	model := anon.Candidate.Model
	if model == "" && len(anon.Attributes["printer-make-and-model"]) > 0 {
		model = anon.Attributes["printer-make-and-model"][0]
	}
	path := filepath.Join(fixturesDir, fixtureName(model)+".json")

	data, err := json.MarshalIndent(anon, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal fixture: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	fmt.Println("wrote", path)
	return nil
}

var nonFixtureChars = regexp.MustCompile(`[^a-z0-9]+`)

// fixtureName derives a filesystem-safe fixture name from a printer model
// string, falling back to a generic name when the model is unknown.
func fixtureName(model string) string {
	name := nonFixtureChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(model)), "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "printer"
	}
	return name
}

// summaryAttributes are the attributes most informative to a human glancing
// at inspect output; the fixture file still carries every attribute found.
var summaryAttributes = []string{
	"printer-uuid",
	"printer-device-id",
	"printer-make-and-model",
	"printer-info",
	"printer-state",
	"printer-state-reasons",
}

func printSummary(report inspect.Report) {
	fmt.Printf("%s  %s\n", report.Candidate.ServiceName, report.Candidate.URI)
	fmt.Printf("  round-trip: %s\n", report.RoundTrip)
	for _, name := range summaryAttributes {
		if v, ok := report.Attributes[name]; ok {
			fmt.Printf("  %s: %s\n", name, strings.Join(v, ", "))
		}
	}
}

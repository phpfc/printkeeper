// Command printkeeper is the CLI client for the printkeeper daemon. Today
// it carries only "inspect", the validation tool that captures everything
// knowable about a real printer so identity-matching weights can be
// designed against measured data rather than guessed field shapes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/inspect"
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
	fmt.Fprintln(os.Stderr, "usage: printkeeper inspect [--json] [uri]")
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

package discovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ippfindPath is the absolute path to the system's ippfind binary. Absolute,
// never looked up on PATH, so this adapter's behaviour does not depend on
// the caller's shell environment. A var, not a const, so tests can point it
// at a stub or a nonexistent path to exercise Browse's error handling.
var ippfindPath = "/usr/bin/ippfind"

// ippfindFormat asks ippfind for one tab-separated line per discovered
// service. A tab (rather than a space, as a naive reading of the spike
// would suggest) is required so a service name containing spaces still
// parses as a single field.
const ippfindFormat = "{service_name}\t{}\t{txt_uuid}\t{txt_ty}\t{txt_rp}\t{txt_pdl}"

type darwinDiscoverer struct{}

// NewDarwinDiscoverer returns a Discoverer that browses Bonjour via the
// system ippfind binary.
func NewDarwinDiscoverer() Discoverer {
	return &darwinDiscoverer{}
}

func (d *darwinDiscoverer) Browse(ctx context.Context, timeout time.Duration) ([]Candidate, error) {
	secs := int(timeout.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}

	out, err := runIppfind(ctx, secs)
	if err != nil {
		// A cancelled context always wins: whatever ippfind returned in
		// that case is not meaningful.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// ippfind(1) documents exit code 1 as "no expression matched" —
		// the normal, expected shape of "found nothing on the network".
		// Everything else (exit code 2: browsing/query/resolution failed,
		// 3: bad option or expression, 4: out of memory, or an error that
		// is not even an *exec.ExitError because the process never
		// started at all — a missing or unreadable binary, a permission
		// error) is a real failure and must be reported, not silently
		// treated as "nothing found".
		if !isIppfindNothingFound(err) {
			return nil, fmt.Errorf("ippfind: %w", err)
		}
	}

	var candidates []Candidate
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if c, ok := parseIppfindLine(scanner.Text()); ok {
			candidates = append(candidates, c)
		}
	}
	return candidates, nil
}

// runIppfind shells out to the system ippfind binary and returns its
// stdout. exec.CommandContext arranges for the child process to be killed
// as soon as ctx is cancelled, satisfying the requirement that Browse's
// context controls the child's lifetime.
func runIppfind(ctx context.Context, timeoutSeconds int) (string, error) {
	cmd := exec.CommandContext(ctx, ippfindPath,
		"_ipp._tcp", "_ipps._tcp",
		"-T", strconv.Itoa(timeoutSeconds),
		"--exec", "echo", ippfindFormat, ";",
	)
	out, err := cmd.Output()
	return string(out), err
}

// isIppfindNothingFound reports whether err is exactly the documented
// ippfind(1) exit status for "the result of any processed expression is
// false" (exit code 1) — the normal outcome when a browse finds no
// services. Any other *exec.ExitError, or an error that is not an
// *exec.ExitError at all (the process failed to even start, so it never
// produced an exit code), is not this case.
func isIppfindNothingFound(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// parseIppfindLine parses one line produced by ippfindFormat into a
// Candidate. It returns ok=false for any line that does not have exactly
// the expected number of tab-separated fields, or whose URI does not parse
// to a usable host — such a line is skipped rather than turned into a
// zero-value Candidate that would silently masquerade as a real result.
func parseIppfindLine(line string) (Candidate, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) != 6 {
		return Candidate{}, false
	}

	serviceName := fields[0]
	rawURI := fields[1]
	uuid := fields[2]
	model := fields[3]
	resourcePath := fields[4]
	pdl := fields[5]

	if serviceName == "" || rawURI == "" {
		return Candidate{}, false
	}

	u, err := url.Parse(rawURI)
	if err != nil || u.Hostname() == "" {
		return Candidate{}, false
	}

	port, _ := strconv.Atoi(u.Port())

	var pdls []string
	if pdl != "" {
		pdls = strings.Split(pdl, ",")
	}

	return Candidate{
		ServiceName:  serviceName,
		URI:          rawURI,
		UUID:         uuid,
		Model:        model,
		ResourcePath: resourcePath,
		PDLs:         pdls,
		Host:         u.Hostname(),
		Port:         port,
	}, true
}

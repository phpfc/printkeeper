# printkeeper Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go daemon plus CLI that keeps a user-adopted printer working — probing it for real, re-identifying it by fingerprint when its address moves, and repairing the CUPS queue in place without the user ever typing an address.

**Architecture:** All logic lives in a portable core (`internal/`) behind three ports — `Discoverer`, `QueueAdmin`, `NetworkKeyer` — with per-OS adapters. Reads come from structured IPP attributes, never from localized CLI text; writes go through `lpadmin`. Front ends (CLI now, native UI later) are thin clients; the daemon owns all state.

**Tech Stack:** Go 1.22+, `github.com/OpenPrinting/goipp` for IPP, macOS `ippfind`/`dns-sd` for Bonjour discovery, `lpadmin` for queue writes, `ippeveprinter` as test hardware, launchd for scheduling.

**Spec:** `docs/superpowers/specs/2026-11-20-printkeeper-design.md`

## Prerequisites

Go is not installed on the development machine. Before Task 1: `brew install go`.

## Global Constraints

- Module path `github.com/phpfc/printkeeper`. License MIT.
- **Never parse localized CLI output.** `lpstat` prints in the user's language (spec §5). Queue reads use the IPP `CUPS-Get-Printers` operation against `ipp://localhost:631/`. Parsing is permitted only for output whose format we supply ourselves, such as `ippfind --exec` with an explicit format string.
- **No network calls other than to printers and DNS.** No telemetry, no crash reporting (spec §11.5).
- **Never delete a queue the daemon did not create, and never delete any queue unasked** (spec §11.7).
- **Never act on a queue the user did not adopt** (spec §6.5).
- A queue name, once created, is immutable from the daemon's side (spec §11.3).
- Committed fixtures are anonymised: UUIDs, MACs, serials and internal hostnames replaced with stable fakes (spec §9).
- Integration tests that touch real cupsd or spawn `ippeveprinter` carry `//go:build integration` and run on macOS only. `go test ./...` with no tags must pass on Linux CI.
- Commits use Conventional Commits in English. One PR per task, each under ~500 diff lines, against `phpfc`.

## Review Focus

Input classes the spec implies that no task's happy path exercises. Each has a test assigned to the task that owns the code.

1. **Two printers of the same model with no usable UUID** must resolve to ambiguous, never to a coin flip — printing to the wrong printer is spec §4's first anti-goal. *Task 7.*
2. **A printer that is reachable but in an error state** (out of paper, cover open) must count as present. Treating a paper jam as a missing printer would rewrite a healthy queue's address. *Task 3.*
3. **A queue on the machine that the user never adopted** must be left untouched by every code path, including repair sweeps. *Task 7.*
4. **A printer whose advertised name carries non-ASCII characters, spaces, or slashes** must still produce a valid, collision-free CUPS queue name. *Task 4.*
5. **A machine with no network at all** must report "cannot tell yet", not "printer gone", and must not churn probes. *Task 3.*

---

### Task 1: Project skeleton, discovery port, and `printkeeper inspect`

Scaffolding folds in here because this is the task whose deliverable needs it. The deliverable is the §8 validation tool: point it at the real problematic printer and capture everything it reveals.

**Files:**
- Create: `go.mod`, `README.md`, `LICENSE`, `.github/workflows/ci.yml`, `.gitignore`
- Create: `internal/discovery/discovery.go`, `internal/discovery/darwin.go`
- Create: `internal/inspect/inspect.go`, `internal/inspect/anonymise.go`
- Create: `cmd/printkeeper/main.go`
- Test: `internal/discovery/darwin_test.go`, `internal/inspect/anonymise_test.go`, `internal/inspect/inspect_integration_test.go`

**Interfaces:**
- Produces:
  - `type Candidate struct { ServiceName, URI, UUID, Model, ResourcePath string; PDLs []string; Host string; Port int }`
  - `type Discoverer interface { Browse(ctx context.Context, timeout time.Duration) ([]Candidate, error) }`
  - `func NewDarwinDiscoverer() Discoverer`
  - `func Inspect(ctx context.Context, uri string) (Report, error)` where `Report` holds the candidate, the full IPP attribute set as `map[string][]string`, and the probe round-trip time
  - `func Anonymise(r Report) Report`

- [ ] **Step 1: Write the failing anonymisation test**

```go
func TestAnonymiseReplacesIdentifyingValuesStably(t *testing.T) {
	r := Report{Candidate: discovery.Candidate{
		UUID: "15f4c408-3a35-3057-5ab6-12023a25f895",
		Host: "printer.internal.corp",
	}, Attributes: map[string][]string{
		"printer-uuid": {"urn:uuid:15f4c408-3a35-3057-5ab6-12023a25f895"},
		"printer-info": {"HP at printer.internal.corp"},
	}}
	a, b := Anonymise(r), Anonymise(r)
	if a.Candidate.UUID == r.Candidate.UUID {
		t.Fatal("uuid survived anonymisation")
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("anonymisation is not stable across calls")
	}
	if strings.Contains(fmt.Sprint(a), "internal.corp") {
		t.Fatal("internal hostname leaked")
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/inspect/ -run TestAnonymise -v`
Expected: FAIL — package does not build, `Anonymise` undefined.

- [ ] **Step 3: Implement `Anonymise(Report) Report` in `internal/inspect/anonymise.go`**

Derive replacements by hashing each original value with a fixed salt so the same input always yields the same fake, and the same real value maps to one fake everywhere in the report. Scrub UUIDs, MACs, serials, and any substring matching a hostname found on the candidate.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test ./internal/inspect/ -run TestAnonymise -v`
Expected: PASS

- [ ] **Step 5: Write the failing discovery-parsing test**

Table test over recorded `ippfind --exec` lines in the format this adapter itself requests, including one line with an empty `uuid=` field and one with a space in the service name. Assert each maps to the expected `Candidate`, and that a malformed line is skipped rather than returned as a zero-value candidate.

- [ ] **Step 6: Run it and confirm it fails**

Run: `go test ./internal/discovery/ -v`
Expected: FAIL — `parseIppfindLine` undefined.

- [ ] **Step 7: Implement the darwin discoverer in `internal/discovery/darwin.go`**

`Browse` shells out to `/usr/bin/ippfind _ipp._tcp,_ipps._tcp -T <timeout> --exec <fmt> ";"` with a tab-separated format string containing `{service_name}`, `{}`, `{txt_uuid}`, `{txt_ty}`, `{txt_rp}`, `{txt_pdl}`. Context cancellation must kill the child process.

- [ ] **Step 8: Run it and confirm it passes**

Run: `go test ./internal/discovery/ -v`
Expected: PASS

- [ ] **Step 9: Write the integration test against a fake printer**

Tagged `//go:build integration`. A helper starts `ippeveprinter -p <free port> SpikePrinter`, waits for the port to accept connections, and returns a cleanup func that kills it. Assert `Browse` finds the service by name and that `Inspect` on its URI returns a report whose attributes include `printer-uuid` and `printer-device-id`.

- [ ] **Step 10: Run it and confirm it passes**

Run: `go test -tags=integration ./internal/inspect/ -v`
Expected: PASS

- [ ] **Step 11: Wire `printkeeper inspect [--json] [uri]` in `cmd/printkeeper/main.go`**

With no URI, browse and inspect every candidate found. `--json` writes the anonymised report to `testdata/fixtures/<model>.json`; without it, print a human-readable summary. Add `README.md` (problem statement, spike evidence table from spec §7, usage), `LICENSE` (MIT), and `.github/workflows/ci.yml` running `go vet ./...` and `go test ./...` on `ubuntu-latest` and `macos-latest`.

- [ ] **Step 12: Run against the real printer and commit the anonymised fixture**

Run: `go run ./cmd/printkeeper inspect --json`
Expected: a fixture file per discovered printer. **Read the fixture before committing it** and confirm no real hostname, UUID, MAC or serial survived.

- [ ] **Step 13: Commit**

```bash
git add go.mod go.sum README.md LICENSE .gitignore .github internal/discovery internal/inspect cmd/printkeeper testdata
git commit -m "feat(discovery): add bonjour discovery and printer inspection"
```

---

### Task 2: Identity fingerprint and scored matching

**Files:**
- Create: `internal/identity/fingerprint.go`, `internal/identity/match.go`
- Test: `internal/identity/match_test.go`

**Interfaces:**
- Consumes: `discovery.Candidate` (Task 1); fixtures in `testdata/fixtures/` as test input.
- Produces:
  - `type Fingerprint struct { UUID, MAC, DeviceID, ServiceName, ResourcePath string; PDLs []string }`
  - `func FromCandidate(c discovery.Candidate) Fingerprint`
  - `type Scored struct { Candidate discovery.Candidate; Score int }`
  - `func Match(stored Fingerprint, candidates []discovery.Candidate) []Scored` — sorted descending by score
  - `const ( WeightUUID = 200; WeightMAC = 60; WeightDeviceID = 25; WeightServiceName = 20; WeightResourcePath = 5 )` — WeightUUID must strictly exceed the sum of the other four (110), so no combination of non-UUID signals can ever reach a UUID match's score

- [ ] **Step 1: Write the failing weight tests**

Table test asserting: a UUID match alone outscores every combination without one; identical device-id plus service name scores `WeightDeviceID + WeightServiceName`; an empty stored field contributes zero rather than matching another empty field; results come back sorted descending.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/identity/ -v`
Expected: FAIL — `Match` undefined.

- [ ] **Step 3: Implement `FromCandidate` and `Match` with the weights above**

Scoring is additive over fields where both sides are non-empty and equal. Normalise UUIDs by stripping any `urn:uuid:` prefix and lowercasing before comparison.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/identity/ -v`
Expected: PASS

- [ ] **Step 5: Write the real-fixture test**

Load the Task 1 fixture, build a `Fingerprint` from it, and assert `Match` against a candidate list containing it plus two unrelated candidates ranks it first with a UUID-weighted score. This is the check that the weights were designed against measured data, not guessed field shapes.

- [ ] **Step 6: Run and confirm pass**

Run: `go test ./internal/identity/ -run Fixture -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/identity
git commit -m "feat(identity): add printer fingerprint and scored matching"
```

---

### Task 3: Active probe

This task encodes the spike's central finding: the only way to know whether a printer is there is to ask it.

**Files:**
- Create: `internal/probe/probe.go`
- Test: `internal/probe/probe_test.go`, `internal/probe/probe_integration_test.go`

**Interfaces:**
- Produces:
  - `type State int` with `StatePresent`, `StateErrored`, `StateAbsent`, `StateUnknown`
  - `type Result struct { State State; PrinterState string; Attributes map[string][]string; RTT time.Duration; Err error }`
  - `func Probe(ctx context.Context, uri string, timeout time.Duration) Result`
  - `func HasNetwork() bool`

- [ ] **Step 1: Write the failing offline test (Review Focus 5)**

```go
func TestProbeWithoutNetworkReturnsUnknownNotAbsent(t *testing.T) {
	r := probeWith(noNetwork{}, context.Background(), "ipp://10.0.0.9/ipp/print", time.Second)
	if r.State != StateUnknown {
		t.Fatalf("offline machine reported %v; a missing network is not a missing printer", r.State)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/probe/ -v`
Expected: FAIL — `probeWith` undefined.

- [ ] **Step 3: Implement `Probe` and the injectable `probeWith` in `internal/probe/probe.go`**

Send IPP `Get-Printer-Attributes` via goipp with the given timeout, requesting `printer-state`, `printer-state-reasons`, `printer-uuid`, `printer-device-id`. Map the outcome: a successful response is `StatePresent`; a response whose `printer-state-reasons` indicate a stopped device is `StateErrored`; a connection failure with network up is `StateAbsent`; a connection failure with no network is `StateUnknown`. `HasNetwork` checks for a default route.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/probe/ -v`
Expected: PASS

- [ ] **Step 5: Write the integration tests against the fake printer**

Tagged `//go:build integration`, reusing Task 1's `ippeveprinter` helper. Three cases: printer alive → `StatePresent`; printer killed → `StateAbsent`; **printer alive but reporting a stopped state → `StateErrored`, not `StateAbsent`** (Review Focus 2) — drive this by starting `ippeveprinter` with a state-reasons argument, or by injecting the attribute map into the mapping function if the binary cannot be made to report it.

- [ ] **Step 6: Run and confirm pass**

Run: `go test -tags=integration ./internal/probe/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/probe
git commit -m "feat(probe): add active printer reachability probe"
```

---

### Task 4: Queue port and darwin adapter

**Files:**
- Create: `internal/queue/queue.go`, `internal/queue/darwin.go`, `internal/queue/name.go`
- Test: `internal/queue/name_test.go`, `internal/queue/darwin_integration_test.go`

**Interfaces:**
- Produces:
  - `type Queue struct { Name, DeviceURI, Description string; Accepting bool; State string }`
  - `type Admin interface { List(ctx context.Context) ([]Queue, error); Create(ctx context.Context, name, uri, description string) error; SetDeviceURI(ctx context.Context, name, uri string) error; Delete(ctx context.Context, name string) error }`
  - `func NewDarwinAdmin() Admin`
  - `func SanitiseName(advertised string, taken []string) string`

- [ ] **Step 1: Write the failing name-sanitisation test (Review Focus 4)**

Table test: `"HP LaserJet 4/Floor 3"` yields a name with no spaces or slashes; `"Impressora Térrea"` yields a valid CUPS name with the accent folded; a name already taken yields a numeric suffix; two different advertised names must never collide after sanitisation.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/queue/ -v`
Expected: FAIL — `SanitiseName` undefined.

- [ ] **Step 3: Implement `SanitiseName` in `internal/queue/name.go`**

CUPS queue names allow printable ASCII except space, `/`, `#`, and must be at most 127 characters. Fold accents, replace disallowed runs with `_`, lowercase, then de-duplicate against `taken` with `_2`, `_3`, and so on.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/queue/ -v`
Expected: PASS

- [ ] **Step 5: Implement the darwin adapter in `internal/queue/darwin.go`**

`List` issues IPP `CUPS-Get-Printers` to `ipp://localhost:631/` via goipp and reads `printer-name`, `device-uri`, `printer-info`, `printer-is-accepting-jobs`, `printer-state` — **never `lpstat`**, per the global constraint. `Create` runs `lpadmin -p <name> -E -v <uri> -m everywhere -D <description>`; `SetDeviceURI` runs `lpadmin -p <name> -v <uri>`; `Delete` runs `lpadmin -x <name>`. Surface a non-zero exit as an error carrying the captured stderr.

- [ ] **Step 6: Write the integration test for the full repair cycle**

Tagged `//go:build integration`. Start `ippeveprinter` on port A, `Create` a queue named `printkeeper_itest` pointing at it, assert `List` reports it with the right `DeviceURI`, restart the fake printer on port B, `SetDeviceURI` to the new URI, assert `List` reflects it, print a file with `lp` and assert the fake printer's spool directory received it. `t.Cleanup` must `Delete` the queue and kill the printer even on failure — this test creates real state on the developer's machine.

- [ ] **Step 7: Run and confirm pass**

Run: `go test -tags=integration ./internal/queue/ -v`
Expected: PASS, and `lpstat -t` afterwards shows no `printkeeper_itest` queue left behind.

- [ ] **Step 8: Write the failing standard-user test**

Spec §5 requires v1 to detect that queue administration needs authentication and
explain it, rather than shipping a privileged helper. `lpadmin` signals this with
a non-zero exit and a `forbidden`/`not authorized` IPP status on stderr. Assert
that `classifyLpadminError` maps such output to `ErrNeedsAuthorization`, and an
unreachable cupsd to `ErrServiceUnavailable`, from recorded stderr samples.

- [ ] **Step 9: Run, implement `classifyLpadminError`, run again**

Run: `go test ./internal/queue/ -run Error -v`
Expected: PASS. `Create` and `SetDeviceURI` return the classified error so the CLI
can explain it in human language instead of showing a command failure.

- [ ] **Step 10: Commit**

```bash
git add internal/queue
git commit -m "feat(queue): add cups queue adapter with ipp reads and lpadmin writes"
```

---

### Task 5: Store with per-network address history

**Files:**
- Create: `internal/store/store.go`, `internal/store/network.go`, `internal/store/network_darwin.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `identity.Fingerprint` (Task 2).
- Produces:
  - `type Adopted struct { QueueName string; Fingerprint identity.Fingerprint; AdoptedAt time.Time; LastSeen map[string]Seen }` keyed by network key
  - `type Seen struct { URI string; At time.Time }`
  - `type Store interface { Load() ([]Adopted, error); Save([]Adopted) error }`
  - `func NewFileStore(path string) Store`
  - `type NetworkKeyer interface { Key() string }` and `func NewDarwinNetworkKeyer() NetworkKeyer`

- [ ] **Step 1: Write the failing per-network history test**

Assert that recording a last-known-good URI under key `net-home` and another under `net-office` for the same adopted printer keeps both, and that reading back under `net-home` returns the home URI — the laptop case from spec §11.4.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/store/ -v`
Expected: FAIL — package does not build.

- [ ] **Step 3: Implement the file store**

JSON at `~/Library/Application Support/printkeeper/adopted.json`, created with `0700` directory and `0600` file permissions. Write via a temp file and `os.Rename` so a crash cannot truncate the store.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/store/ -v`
Expected: PASS

- [ ] **Step 5: Implement `NewDarwinNetworkKeyer` in `internal/store/network_darwin.go`**

Prefer the Wi-Fi SSID; fall back to the default gateway's MAC from `route -n get default` plus `arp -n <gw>`; fall back to the literal `"unknown"`. Pure-function parsing of each command's output must be separately testable from recorded output.

- [ ] **Step 6: Write and run the keyer parsing tests**

Run: `go test ./internal/store/ -run Keyer -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/store
git commit -m "feat(store): add adopted printer store keyed by network"
```

---

### Task 6: Address resolution strategies

Spec §6.4 lists five ordered strategies. Task 1 built only the third, Bonjour
browse. The one that recovers Pedro's actual painful case is the second:
re-resolving a stored hostname through unicast DNS when mDNS is silent. Without
this task the project does not solve the problem it was started for.

**Files:**
- Create: `internal/resolve/resolve.go`, `internal/resolve/lastknown.go`, `internal/resolve/unicast.go`, `internal/resolve/arp_darwin.go`
- Test: `internal/resolve/resolve_test.go`, `internal/resolve/resolve_integration_test.go`

**Interfaces:**
- Consumes: `discovery.Candidate` and `discovery.Discoverer` (Task 1), `probe.Probe` (Task 3), `store.Adopted` and `store.Seen` (Task 5).
- Produces:
  - `type Strategy interface { Name() string; Find(ctx context.Context, a store.Adopted, netKey string) ([]discovery.Candidate, error) }`
  - `func DefaultStrategies(d discovery.Discoverer) []Strategy` — in spec order: `lastKnownGood`, `unicastHostname`, `bonjourBrowse`, `unicastDNSSD`, `snmpIdentity`
  - `func Resolve(ctx context.Context, strategies []Strategy, a store.Adopted, netKey string) ([]discovery.Candidate, []Attempt)` — stops at the first strategy returning candidates; `Attempt` records each strategy's name, duration and outcome so the CLI can say what was tried
  - `func MACFor(ctx context.Context, host string) string` — populates the `MAC` field of `identity.Fingerprint`, which Task 2 defines but nothing yet fills

- [ ] **Step 1: Write the failing ordering test**

Assert `Resolve` tries strategies in order and stops at the first that yields a
candidate: with a fake whose second strategy succeeds, the third and later must
never be called, and `Attempt` must record exactly two entries.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/resolve/ -v`
Expected: FAIL — `Resolve` undefined.

- [ ] **Step 3: Implement `Resolve` and the `lastKnownGood` strategy**

`lastKnownGood` reads `a.LastSeen[netKey]`, probes that URI directly, and returns
it as a single candidate when the probe reports present. Returns no candidates
rather than an error when the key is absent.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/resolve/ -v`
Expected: PASS

- [ ] **Step 5: Write the failing unicast-hostname test**

This is the strategy that answers the internal-hostname case. Given an adopted
printer whose last-known URI is `ipp://printer.internal.corp:631/ipp/print` and
an injected resolver that maps that name to a new address, assert
`unicastHostname` returns a candidate carrying the new address and the original
hostname preserved, and that a `NXDOMAIN` yields no candidates and no error.

- [ ] **Step 6: Run, implement `unicastHostname` in `internal/resolve/unicast.go`, run again**

Resolve the host from the stored URI with an injectable `*net.Resolver` so the
test needs no DNS, honouring the system search domains. Rebuild the URI with the
resolved address, keeping scheme, port and resource path.
Run: `go test ./internal/resolve/ -run Unicast -v`
Expected: PASS

- [ ] **Step 7: Implement the remaining strategies**

`bonjourBrowse` wraps the Task 1 discoverer. `unicastDNSSD` runs
`ippfind --domain <search-domain>` when the system has one configured, and
returns nothing when it does not. `snmpIdentity` is a stub returning no
candidates with a `Name()` of `snmp`, so the ladder and the CLI already handle a
strategy that reports "not attempted" before SNMP is implemented — this keeps the
`Attempt` list honest instead of silently omitting a strategy.

- [ ] **Step 8: Implement `MACFor` in `internal/resolve/arp_darwin.go`**

Parse `arp -n <host>` output for the hardware address; return an empty string on
any failure. Test the parsing against recorded output, including the no-entry
case.

- [ ] **Step 9: Write the integration test for the full resolution chain**

Tagged `//go:build integration`. Start `ippeveprinter` on port A, record it as
last-known-good, kill it, restart on port B, and assert `Resolve` falls past
`lastKnownGood` and recovers it via `bonjourBrowse`, with `Attempt` showing the
first strategy as tried-and-failed.

- [ ] **Step 10: Run and confirm pass**

Run: `go test -tags=integration ./internal/resolve/ -v`
Expected: PASS

- [ ] **Step 11: Commit**

```bash
git add internal/resolve
git commit -m "feat(resolve): add ordered address resolution strategies"
```
### Task 7: Repair confidence ladder

**Files:**
- Create: `internal/repair/ladder.go`
- Test: `internal/repair/ladder_test.go`

**Interfaces:**
- Consumes: `identity.Scored` (Task 2), `probe.Result` (Task 3), `store.Adopted` (Task 5), `resolve.Attempt` (Task 6) for the failure report.
- Produces:
  - `type Action int` with `ActionNothing`, `ActionRepairSilently`, `ActionRepairAndRecord`, `ActionAsk`, `ActionReport`, `ActionLetGo`
  - `type Decision struct { QueueName string; Action Action; Target discovery.Candidate; Choices []discovery.Candidate; Reason string }` — QueueName makes a Decision self-describing (populated from the store.Adopted it was decided about) so a caller never has to re-associate it with a queue by position in Sweep's returned slice
  - `func Decide(a store.Adopted, p probe.Result, matches []identity.Scored, now time.Time) Decision`
  - `func Sweep(ctx context.Context, queues []queue.Queue, adopted []store.Adopted, candidates []discovery.Candidate, now time.Time) []Decision`
  - `const ( ThresholdExact = WeightUUID; ThresholdPartial = 40; AbandonAfter = 21 * 24 * time.Hour )` — ThresholdExact resolves to `200` (Task 2's `WeightUUID`), a score only a UUID match can ever reach, since every other weight summed is 110

- [ ] **Step 1: Write the failing ladder table test**

One case per row of spec §6.5 plus §11.7: present → `ActionNothing`; absent with a UUID match → `ActionRepairSilently`; absent with one partial match → `ActionRepairAndRecord`; absent with two matches above `ThresholdPartial` → `ActionAsk` with both in `Choices`; absent with none → `ActionReport`; `StateUnknown` → `ActionNothing`; absent continuously for longer than `AbandonAfter` → `ActionLetGo`.

- [ ] **Step 2: Write the failing same-model-ambiguity test (Review Focus 1)**

```go
func TestTwoIdenticalModelsWithoutUUIDNeverAutoRepair(t *testing.T) {
	d := Decide(adoptedWithoutUUID, absent, twoSameModelMatches, now)
	if d.Action != ActionAsk {
		t.Fatalf("got %v; printing to the wrong printer is anti-goal 1", d.Action)
	}
	if len(d.Choices) != 2 {
		t.Fatalf("user must be offered both, got %d", len(d.Choices))
	}
}
```

- [ ] **Step 3: Write the failing unadopted-queue test (Review Focus 3)**

Assert `Sweep` returns decisions only for queues present in `adopted`, given a machine whose queue list contains two queues the user never adopted.

- [ ] **Step 4: Run and confirm all three fail**

Run: `go test ./internal/repair/ -v`
Expected: FAIL — `Decide` and `Sweep` undefined.

- [ ] **Step 5: Implement `Decide` and `Sweep` in `internal/repair/ladder.go`**

`Sweep(ctx, queues []queue.Queue, adopted []store.Adopted, candidates []discovery.Candidate, now time.Time) []Decision` joins on queue name and skips anything unadopted. Ambiguity means two or more matches at or above `ThresholdPartial` whose scores differ by less than `WeightDeviceID`.

- [ ] **Step 6: Run and confirm pass**

Run: `go test ./internal/repair/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/repair
git commit -m "feat(repair): add repair confidence ladder"
```

---

### Task 8: CLI — `adopt`, `doctor`, `heal`

**Files:**
- Modify: `cmd/printkeeper/main.go`
- Create: `internal/cli/adopt.go`, `internal/cli/doctor.go`, `internal/cli/heal.go`, `internal/cli/human.go`
- Test: `internal/cli/human_test.go`, `internal/cli/heal_test.go`

**Interfaces:**
- Consumes: every port and core package from Tasks 1–7, injected as a struct so the CLI tests use fakes.
- Produces: `type App struct { Disco discovery.Discoverer; Queues queue.Admin; Store store.Store; Keyer store.NetworkKeyer; Out io.Writer }` with `Adopt`, `Doctor`, `Heal` methods.

- [ ] **Step 1: Write the failing human-language test**

Assert the rendered output for each `repair.Action` contains no occurrence of `ipp://`, `IPP`, `mDNS`, `CUPS`, `URI`, or `queue` — spec §4's third anti-goal, checked mechanically rather than by eye.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/cli/ -v`
Expected: FAIL — `internal/cli` does not exist.

- [ ] **Step 3: Implement `internal/cli/human.go`**

One function per action turning a `repair.Decision` into a sentence a non-technical reader understands. Printers are referred to by their friendly description, never by address.

- [ ] **Step 4: Write the failing heal test with fakes**

Given a fake `Admin` whose `List` returns an adopted queue, a fake `Discoverer` returning the same printer at a new address, and a probe reporting absent, assert `Heal` calls `SetDeviceURI` exactly once with the new address, writes nothing to `Out`, and never calls `Delete`. The silence is the assertion: spec §2 goal 3.

- [ ] **Step 5: Run, implement `Adopt`/`Doctor`/`Heal`, run again**

`Adopt` browses, prints a numbered list of friendly names, reads a selection, sanitises a queue name and calls `Create`. On first run it also lists existing queues and offers to protect them (spec §11.2). `Doctor` probes every adopted printer and reports. `Heal` runs `repair.Sweep` and executes the decisions.
Run: `go test ./internal/cli/ -v`
Expected: PASS

- [ ] **Step 6: Verify by hand against the real printer**

Run: `go run ./cmd/printkeeper adopt`, then move the printer's address, then `go run ./cmd/printkeeper heal`, then print a test page. Record the outcome in the PR body — this is the spec §8 validation result, including whether `printer-uuid` survived the address change.

- [ ] **Step 7: Commit**

```bash
git add cmd/printkeeper internal/cli
git commit -m "feat(cli): add adopt, doctor and heal commands"
```

---

### Task 9: Daemon, IPC, and launchd agent

**Files:**
- Create: `cmd/printkeeperd/main.go`, `internal/ipc/server.go`, `internal/ipc/client.go`, `internal/daemon/schedule.go`
- Create: `packaging/darwin/io.github.phpfc.printkeeper.plist`
- Modify: `cmd/printkeeper/main.go`
- Test: `internal/ipc/ipc_test.go`, `internal/daemon/schedule_test.go`

**Interfaces:**
- Produces:
  - `func Serve(ctx context.Context, socketPath string, app *cli.App) error` — newline-delimited JSON requests `{"method":"status"|"heal"|"adopt","params":{...},"id":N}`
  - `func Dial(socketPath string) (*Client, error)`
  - `func NextProbeAt(last time.Time, lastResult probe.State, onBattery bool) time.Duration`

- [ ] **Step 1: Write the failing scheduling test**

Assert: a present printer schedules the 15-minute heartbeat; an absent one schedules a shorter retry with backoff capped at the heartbeat; `StateUnknown` schedules no probe at all; on battery and idle, only the heartbeat applies. Spec §6.6.

- [ ] **Step 2: Run, implement `NextProbeAt`, run again**

Run: `go test ./internal/daemon/ -v`
Expected: PASS

- [ ] **Step 3: Write the failing IPC round-trip test**

Over a socket in `t.TempDir()`, assert a `status` request returns the adopted printers from a fake app, that a malformed line gets an error response without killing the server, and that the socket is created with `0600`.

- [ ] **Step 4: Run, implement `Serve` and `Dial`, run again**

Run: `go test ./internal/ipc/ -v`
Expected: PASS

- [ ] **Step 5: Implement the daemon and the launchd plist**

`printkeeperd` loads the store, serves the socket at `~/Library/Application Support/printkeeper/sock`, and runs the probe loop on the `NextProbeAt` schedule, re-probing immediately on `SCNetworkReachability`-equivalent change. The plist is a `RunAtLoad` user agent with `KeepAlive` on successful exit disabled. `printkeeper` commands prefer the daemon when the socket exists and fall back to running in-process.

- [ ] **Step 6: Install, verify, and uninstall by hand**

Run: `launchctl bootstrap gui/$(id -u) packaging/darwin/io.github.phpfc.printkeeper.plist`, confirm `printkeeper status` answers over the socket, break the printer's address, wait for one cycle, confirm the queue repaired itself with no notification, then `launchctl bootout` and confirm the process is gone.

- [ ] **Step 7: Commit**

```bash
git add cmd/printkeeperd internal/ipc internal/daemon packaging cmd/printkeeper
git commit -m "feat(daemon): add launchd agent with ipc and probe scheduling"
```

---

### Task 5b: TLS upgrade for printer connections

Inserted after Task 5, ahead of Task 6, by controller ruling on 2026-11-21. Not part of
the original plan: pointing `printkeeper inspect` at the first real printer available on
the network failed outright, and every later task's real-hardware verification depends
on this working.

**The measurement.** `go run ./cmd/printkeeper inspect ipp://EPSON00973D.local:631/ipp/print`
returned `decode ipp response: Message truncated at 0x0`. `curl` against the same
endpoint explains it: the printer answers `HTTP/1.1 426 Upgrade Required` with
`Upgrade: TLS/1.0, HTTP/1.1` and `Content-Length: 0`. It refuses plaintext IPP on port
631. `ipptool` reads the same printer without trouble because CUPS's HTTP layer performs
the upgrade transparently. `internal/ippclient` never inspects the HTTP status code, so
it hands an empty body to the decoder.

**Why it matters beyond `inspect`.** `probe` classifies this healthy printer as
`StateErrored` — bytes arrived, undecodable. That is the correct side of the
absent/errored boundary, so the repair ladder will not rewrite a healthy queue's
address over it, but the printer is reported as broken when it is fine.
`queue.List` is unaffected: localhost cupsd demands no upgrade.

**Files:**
- Modify: `internal/ippclient/ippclient.go`
- Modify: `docs/superpowers/specs/2026-11-20-printkeeper-design.md` (§5 constraints — the design never mentions TLS)
- Test: `internal/ippclient/ippclient_test.go`, `internal/ippclient/ippclient_integration_test.go`

**Interfaces:**
- Produces: no new exported surface. `GetPrinterAttributes` and `CupsGetPrinters` keep their signatures; the upgrade happens beneath them.

- [ ] **Step 1: Write the failing test for the 426 path**

An `httptest.Server` that answers the first request with `426 Upgrade Required`, an
`Upgrade: TLS/1.0, HTTP/1.1` header and an empty body, and serves a valid IPP response
over TLS on retry. Assert the client returns the decoded attributes rather than a
decode error. Use `httptest.NewTLSServer` for the TLS leg and point the client at the
plaintext endpoint first.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/ippclient/ -run Upgrade -v`
Expected: FAIL with a truncated-message decode error — the same symptom the real printer produced.

- [ ] **Step 3: Implement the upgrade**

Inspect the HTTP status before decoding. On `426`, retry the identical request against
`https://host:port/path`. Certificate verification is disabled **for printer
connections only** — printers ship self-signed certificates, CUPS itself trusts them on
first use, and printkeeper sends no credentials to a printer, only attribute queries.
Document that trade-off at the call site, not only in the design doc. Any non-success
status that is not `426` becomes an error naming the status, so a `401` or `403` stops
looking like a malformed response.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test ./internal/ippclient/ -run Upgrade -v`
Expected: PASS

- [ ] **Step 5: Write the failing test for non-426 failure statuses**

Assert a `401` and a `503` each produce an error that names the status and is
distinguishable from `MalformedResponseError`, since `probe` maps the latter to
`StateErrored` on the grounds that bytes arrived.

- [ ] **Step 6: Run, implement, run**

Run: `go test ./internal/ippclient/ -v`
Expected: PASS

- [ ] **Step 7: Verify against the real printer**

Run: `go run ./cmd/printkeeper inspect --json ipp://EPSON00973D.local:631/ipp/print`
Expected: a report, and an anonymised fixture. **Read the fixture before committing it**
and confirm the real serial (`SN:` inside `printer-device-id`), the real UUID and the
real hostname are all absent — that check has never been run against real-hardware data,
only against simulator output.

- [ ] **Step 8: Update the design document**

Add to §5 Constraints that real printers may refuse plaintext IPP and require an in-place
TLS upgrade, with the certificate-verification decision and its reasoning.

- [ ] **Step 9: Commit**

```bash
git add internal/ippclient docs testdata
git commit -m "fix(ipp): upgrade to TLS when the printer requires it"
```

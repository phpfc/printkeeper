# printkeeper — Design Document

**Date:** 2026-11-20
**Author:** Pedro Henrique França (with Claude)
**Status:** Pending approval

---

## 1. Intent and why

Printers configured on macOS silently stop working. The queue stays listed and
looks healthy, but the device behind it is unreachable — usually because the
address stored in the queue (an IP, an mDNS name) is no longer where the printer
is. The only proactive repair macOS offers is *Add Printer → IP → type the
hostname by hand*, which requires knowing the host, the protocol (`ipp` vs
`ipps` vs `socket`), and gives a generic failure when any of those is wrong.

A spike on 2026-11-20 established the mechanism (see §7). The decisive finding:
**CUPS reports a dead printer as `idle` and `accepting requests`.** There is no
event, no flag, no state change. Nothing in the OS can act proactively because
the OS does not know. Only an active probe discovers the truth.

printkeeper is a background service plus a thin native UI that keeps a printer
the user once chose working: it probes for real, re-identifies the device on the
network by fingerprint, and repairs the queue in place — silently when it is
confident. The user never types an address and never loses their printer.

**Audience:** non-technical macOS users. Nobody is expected to know what CUPS,
IPP, or mDNS are, at any point, including error states.

## 2. Goals

1. A printer the user chose once never needs to be manually re-added.
2. Zero address typing, ever. Adoption happens from a human-readable list.
3. **Success is silent.** No notification when a repair works.
4. When repair is impossible, explain what was tried in human language.
5. Portable core: macOS first, Linux nearly free (same CUPS surface), Windows as
   a later adapter against the print spooler.

### 2.1 Secondary intent: this is a portfolio piece

The repository is as much the deliverable as the binary. That raises the bar on
things a side project would let slide: a README that states the problem and
carries the spike evidence, a commit history that reads as a narrative, tests
that run with no hardware attached, CI that proves it, and this document living
in the repo. It lowers the bar elsewhere: no licensing, no update channel, no
paid signing in v1. License: MIT.

## 3. Non-goals (v1)

- Job-level management: stuck jobs, paused queues, cancelling, reprinting.
- Driver and PPD management beyond driverless IPP Everywhere (`-m everywhere`).
- Print server administration, accounting, quotas, access control.
- Scanning on multifunction devices.
- Standard (non-admin) user support. Detected and explained, not solved — see §5.
- Windows. The architecture leaves room for it; v1 does not ship it.

## 4. Anti-goals — these make the project a failure even if it technically works

- **Printing to the wrong printer.** For this audience, "nothing came out" is a
  comprehensible problem; "it came out on another floor" is a lost document and
  a situation they cannot diagnose. A wrong guess costs more than an omission.
- **Becoming another notification-spam menubar app.** If it announces its
  successes, it becomes something to ignore, then something to uninstall.
- **Leaking domain vocabulary into the UI.** No "IPP", no "mDNS", no "CUPS
  queue", no URIs shown to the user — not even in error messages.
- **A visible footprint.** Measurable battery drain, constant wakeups, or a
  daemon with conspicuous memory use. Invisibility is a feature, not a polish
  item.

## 5. Constraints

**Distribution.** The Mac App Store is impossible: administering a print queue
requires operations outside the app sandbox with admin authentication. Since
this ships free and as a portfolio piece (§2.1), v1 does **not** buy Developer
ID signing or notarization: the repository is the primary artifact, and users
build from source or clear Gatekeeper once on an unsigned build. Signing
(99 USD/year) becomes a later decision rather than a precondition — which
removes the only hard cash dependency from the critical path.

**Privilege, measured not assumed.** `/etc/cups/cupsd.conf` requires
`Require user @SYSTEM` for `CUPS-Add-Modify-Printer`. In the spike, `lpadmin`
succeeded with **no sudo and no password prompt**, because the operator is an
admin user and the local cupsd accepts peer credentials over its domain socket.
So the privileged helper (`SMAppService`) is **conditional, not mandatory**: the
common case (personal Mac, admin user) needs none. For standard users the
operation requires authentication; v1 detects this and explains it rather than
shipping a helper.

**Localized CLI output is unparseable.** The spike's `lpstat` printed
*"impressora spike_test está ociosa"*. Any parser over that breaks under a
different locale. **Reads go through structured IPP attributes, never scraped
CLI text.** Writes stay on `lpadmin`, which is stable and generates the
IPP Everywhere PPD for free.

**mDNS does not cross subnets or most VPNs.** The case that hurts most — a
printer reachable only by an internal hostname — is exactly where Bonjour is
silent. Discovery needs fallbacks (§6.4).

**Battery.** Probing must be driven by network-change events with a slow
heartbeat, never a tight poll loop.

**Build environment.** The development machine has Swift 6.4 via Command Line
Tools only, no full Xcode. The Go core and CLI build today; the SwiftUI client
will require installing Xcode.

## 6. Architecture

### 6.1 Approaches considered

**A — Headless Swift core (SwiftPM) + CLI + launchd agent, UI later.** Builds
today with no new toolchain; shortest path to validating the open hardware risk.
Cost: the core is Swift, so a Windows adapter later is effectively a rewrite;
nobody non-technical can test until a UI exists. *Recommended, not chosen.*

**B — SwiftUI menubar app from day one.** Real product shape early, testable
with real users sooner. Cost: Xcode plus a paid Apple account before the first
useful line, and UI built on top of a hypothesis not yet validated against real
hardware.

**C — Portable core in Go or Rust + per-OS adapters, thin native UI.**
**Chosen by Pedro.** Pays the multi-OS cost up front; Linux comes nearly free
because it is the same CUPS surface the spike exercised. The objection raised
against C during the discussion — an FFI bridge to the native UI — dissolved on
closer design: the product *requires* a long-lived background prober, so the
core is a daemon by nature and the UI is an IPC client. There is no FFI.

**Language: Go.** Decided as the author's call, open to veto. `goipp` is
maintained by OpenPrinting, the CUPS maintainers — the strongest available
ecosystem signal for an IPP-domain project, with adjacent reference code
(`ipp-usb`, `cups-browsed`) to read. Daemon + IPC is Go's home turf, and Go's
real weakness here (behaving badly as an in-process library inside a macOS app)
is moot once FFI is gone. The trade-off accepted: roughly 10 MB binary and
20–30 MB RSS, against roughly 4 MB for Rust.

### 6.2 Shape

A daemon owns all logic and state. Every front end — CLI, menubar app, future
Windows UI — is a thin client over IPC.

```
printkeeperd (Go, launchd user agent)
   ├── core/identity   fingerprint construction and scored matching
   ├── core/probe      active truth-telling: is this device really there?
   ├── core/repair     confidence ladder -> action
   ├── core/store      adopted printers, fingerprint history, last-known-good
   └── ports/          Discovery, QueueAdmin, Notifier  (interfaces)
         └── adapters/darwin   ippfind / lpadmin / IPP to local cupsd
             adapters/linux    same surface, different paths
             adapters/windows  later: print spooler API

clients: printkeeper (Go CLI)  ·  printkeeper.app (SwiftUI, later)
         both speak JSON-RPC over a UNIX domain socket
```

Portability lives at the `ports/` boundary, not in the language. A Windows
adapter is a new implementation of three interfaces, not a rewrite.

### 6.3 Identity

The hypothesis under test is that a printer has a stable identity independent of
its address. The spike confirmed `printer-uuid` is exposed both as an IPP
attribute and in the Bonjour TXT record (`uuid=`), readable **without connecting
to the device**. It could not confirm *stability*, because `ippeveprinter`
derives its UUID from host and port. This remains the project's main open risk,
to be settled against real hardware before any UI work.

The design therefore never bets on a single field. A fingerprint is a set of
signals, matched with weights:

| Signal | Source | Weight |
|---|---|---|
| `printer-uuid` | TXT `uuid=`, IPP `printer-uuid` | highest |
| MAC address | SNMP, ARP on last-known address | high |
| `printer-device-id` (MFG/MDL) | IPP, TXT `ty=` | medium |
| DNS-SD service name | mDNS browse | medium |
| Resource path (`rp=`) + supported PDLs | TXT | low |
| Last-known-good address history | local store | tiebreaker only |

Exact weights and thresholds are left to the builder to tune against real
devices; the ladder in §6.5 defines what the thresholds must distinguish.

### 6.4 Discovery, including the painful case

Ordered strategies, cheapest and safest first:

1. Retry the last-known-good address directly (TCP 631/9100, then IPP probe).
2. Re-resolve the stored hostname through unicast DNS, honouring the system
   search domains — this is what recovers the internal-hostname case when mDNS
   is silent.
3. Bonjour browse (`_ipp`, `_ipps`, `_pdl-datastream`) for same-segment devices.
4. Unicast DNS-SD against a configured search domain, when one exists.
5. SNMP identity query (`sysName`, serial) against known candidates.

**Explicitly excluded from v1:** subnet-wide port scanning. On a corporate
network it looks like hostile reconnaissance, and it is a poor trade for the
coverage it buys. Builder's call, recorded here: if it is ever added, it must be
opt-in and user-initiated, never a background behaviour.

### 6.5 Repair, and who decides which printer is "mine"

**Explicit choice once, defended forever. The daemon never adopts a printer the
user did not choose.** Rationale in §4: the protocol carries no information that
would let it guess correctly — `printer-location` came back empty even in the
spike — so a guess would be luck, and the cost of being wrong is high.

What keeps this from feeling bureaucratic is treating the single choice as a
product moment: a list with human-readable names built from the TXT record's
`ty` and model, and **zero typing**. That is where the original pain dies.

Repair then runs on a confidence ladder:

| Situation | Action |
|---|---|
| Identity matches exactly (UUID, or full fingerprint) | **Repair silently.** No notification. The user never learns it broke. |
| Partial match, single candidate | Repair and record. Surfaced only if the user opens the app. |
| Two or more plausible candidates | **Do not choose.** Ask once, in human language: "Two printers look like yours. Which one do you use?" |
| No candidate | Report what was tried and what failed, in human language. |

Repair itself is surgical and proven: `lpadmin -p <queue> -v <new-uri>` changes
only the address and **preserves the queue name**, so the printer the user sees
in Word or Preview never changed — it just started working again. The spike
verified this end to end, including a real job arriving at the device after the
address moved.

### 6.6 Probe scheduling

Event-driven first: on network change, on wake from sleep, on daemon start.
A slow heartbeat (order of 15 minutes) covers silent failures while the network
looks stable. No probing while on battery and idle beyond the heartbeat. Exact
intervals are the builder's to tune against measured wakeup cost.

## 7. Spike evidence (2026-11-20)

Run against `ippeveprinter`, a real IPP Everywhere printer advertised over
Bonjour, on the development machine. macOS 27.3, CUPS 2.3.4.

| Question | Result |
|---|---|
| Is identity readable without connecting? | **Yes.** TXT carried `uuid`, `ty`, `rp`, `pdl`. |
| Does CUPS report an unreachable printer? | **No.** Reported `idle` / `accepting requests` with the device dead. Only `ipptool` knew. |
| Can a queue be created driverless, unprivileged? | **Yes.** `lpadmin -m everywhere`, exit 0, no sudo, no prompt (admin user). |
| Can a queue be repaired in place after the address moves? | **Yes.** `lpadmin -v <new-uri>`; queue name preserved; a real job then reached the device (19 KB of raster). |
| Is `printer-uuid` stable across an address change? | **Unproven.** Simulator derives it from host+port. **Open risk.** |

All spike changes were reverted; the machine has no printers configured, as
before.

## 8. Validation milestone (gates UI work)

**A real problematic printer is available on Pedro's network.** The gate opens
immediately instead of blocking the project, and it reorders delivery (§9): the
first slice harvests real identity data before any matching logic is designed.

The questions it must answer: does `printer-uuid` survive an address change, and
does strategy 2 of §6.4 recover a printer reachable only by an internal
hostname? If UUID proves unstable, the weights in §6.3 absorb it — but the
answer must be measured, not assumed.

## 9. Delivery

Vertical slices, each under ~500 diff lines, each with tests, each its own PR in
English with Conventional Commits, against `phpfc` on GitHub:

1. `feat(adapters): darwin discovery and printer inspection` — includes a
   `printkeeper inspect` command that dumps everything knowable about a real
   device (TXT records, full IPP attribute set) and records it as a test fixture.
2. `feat(core): printer identity and fingerprint matching` — designed against
   those real fixtures, not against guessed field shapes.
3. `feat(probe): active printer reachability probe`
4. `feat(queue): cups queue adapter, IPP reads and lpadmin writes`
5. `feat(store): adopted printer store keyed by network`
6. `feat(resolve): ordered address resolution strategies` — the five strategies
   of §6.4, including the unicast hostname re-resolution that recovers the
   internal-hostname case
7. `feat(repair): repair confidence ladder`
8. `feat(cli): adopt, doctor and heal commands`
9. `feat(daemon): launchd agent with IPC and probe scheduling`

Slice 1 comes first precisely because the hardware is reachable: identity
weights get designed against measured data. **Fixtures are anonymised before
they are committed** — UUIDs, MAC addresses, serials and internal hostnames
replaced with stable fakes. The repository is public and those values describe
somebody's actual network.

`ippeveprinter` becomes test infrastructure: CI can start a fake printer, move
its port, kill it, and assert the repair ladder without hardware.

## 10. Left to the builder

- Fingerprint weights and ladder thresholds (tune against real devices).
- IPC schema and versioning.
- Probe interval tuning against measured wakeup cost.
- Store format and migration policy.

## 11. Calls settled during the builder check

These were gaps a builder would have had to guess at. They are the author's
decisions, not Pedro's — listed separately so they can be checked and reversed.

**11.1 printkeeper owns adoption, not just recovery.** The daemon creates the
queue itself (`lpadmin -p <name> -E -v <uri> -m everywhere`), from a picked
candidate. It does not merely watch queues that macOS created. This follows from
the original pain: the error-prone step being replaced *is* the add flow, so
"zero typing" has to cover first setup, not only re-finding.

**11.2 Existing queues are imported, not ignored.** On first run, printkeeper
lists queues already configured on the machine and offers to protect them. This
is the likeliest onboarding path — the user installs it *because* a printer they
already had broke.

**11.3 Queue naming.** The queue name is derived from the model (TXT `ty`),
sanitised to the CUPS name charset, de-duplicated with a numeric suffix. The
user may rename it; that is harmless typing, unlike typing an address. The name
is then immutable from the daemon's side, because it is what the user sees in
every print dialog forever.

**11.4 Last-known-good is keyed per network.** A laptop has a home printer and
an office printer, and the same printer can live at different addresses on
different networks. The store keys address history by a network fingerprint
(SSID where available, else default-gateway MAC), so returning to a known
network is a cache hit rather than a rediscovery.

**11.5 Nothing leaves the machine.** No telemetry, no crash reporting to a
server, no network calls other than to printers and DNS. For an audience on
corporate networks, running a background process that inventories devices, this
is the only defensible default. Revisit only as an explicit opt-in.

**11.6 Linux is CI-only in v1.** The core and its tests build and run on Linux
to prevent darwin-only drift leaking into `core/`, but no Linux adapter,
packaging, or support ships. Windows stays out entirely per §3.

**11.7 Letting go.** A printer that fails every probe for an extended period
(order of weeks) is likely decommissioned, not broken. The daemon stops probing
it and, on next user contact, offers to forget it. It never deletes a queue on
its own — deleting something the user chose is outside what it may do unasked.

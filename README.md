# printkeeper

Printers configured on macOS silently stop working. The queue stays listed
and looks healthy, but the device behind it is unreachable — usually because
the address stored in the queue (an IP, an mDNS name) is no longer where the
printer is. The only proactive repair macOS offers is *Add Printer → IP →
type the hostname by hand*, which requires knowing the host, the protocol
(`ipp` vs `ipps` vs `socket`), and gives a generic failure when any of those
is wrong.

A spike on 2026-11-20 established the mechanism. The decisive finding:
**CUPS reports a dead printer as `idle` and `accepting requests`.** There is
no event, no flag, no state change. Nothing in the OS can act proactively
because the OS does not know. Only an active probe discovers the truth.

printkeeper is a background service plus a thin native UI that keeps a
printer the user once chose working: it probes for real, re-identifies the
device on the network by fingerprint, and repairs the queue in place —
silently when it is confident. The user never types an address and never
loses their printer.

The full design is in
[`docs/superpowers/specs/2026-11-20-printkeeper-design.md`](docs/superpowers/specs/2026-11-20-printkeeper-design.md).

## Spike evidence (2026-11-20)

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

## This slice: discovery and inspection

This first slice lays the project skeleton and ships `printkeeper inspect`,
the validation tool the rest of the project depends on: it dumps everything
knowable about a real printer — the Bonjour TXT record and the full IPP
`Get-Printer-Attributes` response — so later work designs identity-matching
weights against measured data instead of guessed field shapes.

- `internal/discovery` — finds printers by browsing Bonjour (`ippfind`),
  without connecting to them.
- `internal/inspect` — connects to one printer and asks it, over real IPP,
  everything it is willing to report. `Anonymise` strips anything
  identifying (UUIDs, MAC addresses, serials, hostnames) before a report is
  written to disk, with a stable, deterministic replacement so the same
  input always redacts the same way.
- `cmd/printkeeper` — the CLI entry point.

### Usage

```sh
# Browse the network and print a human-readable summary of every printer found.
go run ./cmd/printkeeper inspect

# Inspect one printer directly, by URI, skipping discovery.
go run ./cmd/printkeeper inspect ipp://printer.local:631/ipp/print

# Browse (or inspect one URI) and write an anonymised fixture per printer to
# testdata/fixtures/<model>.json instead of printing a summary.
go run ./cmd/printkeeper inspect --json
```

### Testing

```sh
go test ./...                    # unit tests, no hardware required
go test -tags=integration ./...  # also starts a real ippeveprinter and probes it (macOS only)
```

## License

MIT. See [LICENSE](LICENSE).

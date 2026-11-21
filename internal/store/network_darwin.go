package store

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// This file's command output parsers were checked against the real
// tools on macOS 27.3 before being written, not assumed. Each of
// `networksetup -listallhardwareports`/`-getairportnetwork`, `route -n
// get default`, `scutil --dns`, and `arp -n <gw>` was re-run with
// LANG/LC_ALL forced to a non-English locale (pt_BR.UTF-8, fr_FR.UTF-8,
// de_DE.UTF-8, and ja_JP.UTF-8 respectively — scutil was checked under
// de_DE.UTF-8 and ja_JP.UTF-8 as well) and produced byte-identical
// English labels to the untouched-locale run. `strings` on
// /usr/sbin/networksetup, /sbin/route, /usr/sbin/scutil, and
// /usr/sbin/arp confirms why: the labels this file matches on
// ("Hardware Port:", "Device:", "Current Wi-Fi Network: ", "You are not
// associated with an AirPort network.", "gateway:", "interface:",
// "search domain[0] :", "(incomplete)", "-- no entry") are format
// strings embedded directly in the binaries, not pulled from a locale
// catalog the way CUPS's CLI tools are (see
// internal/queue/queue_darwin.go's buildLpadminCmd). So, unlike
// lpadmin, nothing here forces LC_ALL=C — there is no catalog to pin
// against.
//
// A fix round on this file also found, by actually running each
// command from a compiled Go binary rather than trusting a shell
// session, that two of the signals this keyer depends on fail silently
// rather than erroring:
//
//   - `arp -a` (and `arp -n <gw>`) returns nothing to a process tree
//     rooted at a plain Go binary that has not been granted whatever
//     privacy/location entitlement macOS now gates ARP-table
//     visibility behind, even though the exact same command typed into
//     an interactive shell on the same machine sees the real table.
//     This is not a sandbox artifact of any particular dev environment
//     — it reproduces on real hardware outside a sandbox too — and it
//     means the gateway's MAC address is usually unavailable to a
//     background daemon in particular, which is exactly how
//     printkeeper runs.
//   - `networksetup -getairportnetwork` reports "You are not
//     associated with an AirPort network." when SSID disclosure is
//     denied by Location Services permission, which is indistinguishable
//     from genuinely not being on Wi-Fi. A caller cannot tell "no
//     Wi-Fi" from "yes Wi-Fi, but not allowed to say so" from this
//     string alone.
//
// Both are real signals when permission has been granted (so they are
// still gathered and layered into the key when they return something),
// but neither is reachable on a freshly installed daemon. Treating
// either as foundational would make Key() collapse to "unknown" in the
// daemon's actual running context — exactly the silent, undetected
// failure mode per-network keying exists to prevent (spec §11.4). The
// composite key below is built instead from `route -n get default`
// (gateway IP, interface) and `scutil --dns` (search domain), both of
// which were independently confirmed to work unprivileged from a
// compiled Go binary; SSID and gateway MAC are layered on top only when
// they resolve to something real.

// networksetupPath, routePath, scutilPath, and arpPath are absolute,
// never looked up on PATH, so this adapter's behaviour does not depend
// on the caller's shell environment. They are vars, not consts, so a
// test could point them at a stub.
var (
	networksetupPath = "/usr/sbin/networksetup"
	routePath        = "/sbin/route"
	scutilPath       = "/usr/sbin/scutil"
	arpPath          = "/usr/sbin/arp"
)

// commandTimeout bounds every shell-out below. These are local
// administrative tools with no network round trip of their own
// (arp -n reads the kernel's cache; it does not send an ARP request),
// so a hang here would mean something is badly wrong with the tool
// itself, not that it is waiting on the network.
const commandTimeout = 2 * time.Second

// unknownKey is NetworkKeyer's fallback when there is genuinely no
// network to key on — no default route at all. It must never be
// returned while a default route exists; see composeKey.
const unknownKey = "unknown"

type darwinNetworkKeyer struct{}

// NewDarwinNetworkKeyer returns a NetworkKeyer that identifies the
// current network (spec §11.4) from a composite of what is actually
// reachable to a daemon process: the default gateway's IP and
// interface, plus the DNS search domain when the network advertises
// one, enriched with the Wi-Fi SSID and the gateway's MAC address when
// those happen to be available (see this file's top comment for why
// they can't be relied on alone). Key returns the literal "unknown"
// only when there is no default route at all.
func NewDarwinNetworkKeyer() NetworkKeyer {
	return darwinNetworkKeyer{}
}

// Key implements NetworkKeyer.
func (darwinNetworkKeyer) Key() string {
	routeOutput, err := runCommand(routePath, "-n", "get", "default")
	if err != nil {
		return unknownKey
	}
	if _, ok := parseDefaultGatewayIP(routeOutput); !ok {
		return unknownKey
	}

	// Every signal from here on is best-effort enrichment: a failed
	// command just means its output parses to "not available" below,
	// never an error that would make Key itself fail.
	dnsOutput, _ := runCommand(scutilPath, "--dns")
	airportOutput := currentAirportOutput()
	gateway, _ := parseDefaultGatewayIP(routeOutput)
	arpOutput, _ := runCommand(arpPath, "-n", gateway)

	return composeKey(routeOutput, dnsOutput, airportOutput, arpOutput)
}

// currentAirportOutput runs `networksetup -getairportnetwork` for this
// machine's Wi-Fi device, if it has one, and returns its raw output
// (or "" if there is no Wi-Fi hardware or the command failed) for
// composeKey to parse. Finding the device by hardware port, rather
// than assuming "en0", matters because en0 is conventionally Wi-Fi on
// most Macs but is not guaranteed to be.
func currentAirportOutput() string {
	hardwarePorts, err := runCommand(networksetupPath, "-listallhardwareports")
	if err != nil {
		return ""
	}
	device, ok := wifiDevice(hardwarePorts)
	if !ok {
		return ""
	}
	out, err := runCommand(networksetupPath, "-getairportnetwork", device)
	if err != nil {
		return ""
	}
	return out
}

// composeKey builds the network key from the raw output of all four
// commands Key gathers. It is a pure function — no I/O — specifically
// so the property that matters (gateway present → never "unknown";
// a denied/absent Wi-Fi reading → contributes nothing; two networks
// differing only in gateway → different keys) can be pinned by a test
// without executing anything.
//
// The key is "gw:<ip>" at minimum, with "|if:<iface>", "|domain:<d>",
// "|ssid:<s>", and "|mac:<m>" appended in that order for whichever of
// interface, DNS search domain, Wi-Fi SSID, and gateway MAC actually
// parsed from their respective output. Gateway IP is the only
// foundation: everything else is enrichment that strengthens the key
// when available and is simply omitted, never faked, when it is not.
func composeKey(routeOutput, dnsOutput, airportOutput, arpOutput string) string {
	gateway, ok := parseDefaultGatewayIP(routeOutput)
	if !ok {
		return unknownKey
	}

	key := "gw:" + gateway
	if iface, ok := parseDefaultInterface(routeOutput); ok {
		key += "|if:" + iface
	}
	if domain, ok := parseDNSSearchDomain(dnsOutput); ok {
		key += "|domain:" + domain
	}
	if ssid, ok := parseCurrentWiFiNetwork(airportOutput); ok {
		key += "|ssid:" + ssid
	}
	if mac, ok := parseARPMac(arpOutput); ok {
		key += "|mac:" + mac
	}
	return key
}

// wifiDevice parses `networksetup -listallhardwareports` output and
// returns the device name of the "Wi-Fi" hardware port, if the machine
// has one. The format is a blank-line-separated list of
// "Hardware Port: <name>" / "Device: <dev>" / "Ethernet Address: <mac>"
// stanzas; this looks for the stanza whose Hardware Port line is
// exactly "Wi-Fi" and returns its Device line's value.
func wifiDevice(output string) (string, bool) {
	lines := strings.Split(output, "\n")
	inWiFiStanza := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			name := strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
			inWiFiStanza = name == "Wi-Fi"
		case inWiFiStanza && strings.HasPrefix(line, "Device:"):
			device := strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
			if device == "" {
				return "", false
			}
			return device, true
		}
	}
	return "", false
}

// parseCurrentWiFiNetwork parses `networksetup -getairportnetwork`
// output. On success it prints exactly "Current Wi-Fi Network: <ssid>"
// (confirmed against the format string embedded in the networksetup
// binary); any other output — "You are not associated with an AirPort
// network." (also printed when SSID disclosure is denied by Location
// Services permission — see this file's top comment), "All Wi-Fi
// network services are disabled.", an interface error, or simply "" —
// means no SSID is available, and contributes nothing to the caller's
// key rather than a distinct value of its own.
func parseCurrentWiFiNetwork(output string) (string, bool) {
	const prefix = "Current Wi-Fi Network: "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			ssid := strings.TrimSuffix(strings.TrimPrefix(line, prefix), "\r")
			if ssid == "" {
				return "", false
			}
			return ssid, true
		}
	}
	return "", false
}

// gatewayLineRE matches `route -n get default`'s "    gateway: <addr>"
// line. Anchored to line start (with leading whitespace) so it cannot
// match the unrelated "GATEWAY" token that also appears in that
// output's flags line.
var gatewayLineRE = regexp.MustCompile(`(?m)^\s*gateway:\s*(\S+)\s*$`)

// parseDefaultGatewayIP parses `route -n get default` output and
// returns the gateway address from its "gateway: <addr>" line. Absent
// a default route, the command prints no such line (and typically
// exits non-zero, e.g. "route: writing to routing socket: not in
// table"), so a missing line is treated as no gateway rather than an
// error to propagate. This is the one signal composeKey treats as
// foundational: its presence is what decides real key vs. unknownKey.
func parseDefaultGatewayIP(output string) (string, bool) {
	m := gatewayLineRE.FindStringSubmatch(output)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// interfaceLineRE matches `route -n get default`'s "  interface: <name>"
// line, from the same output parseDefaultGatewayIP reads.
var interfaceLineRE = regexp.MustCompile(`(?m)^\s*interface:\s*(\S+)\s*$`)

// parseDefaultInterface parses `route -n get default` output and
// returns the interface the default route goes out on.
func parseDefaultInterface(output string) (string, bool) {
	m := interfaceLineRE.FindStringSubmatch(output)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// dnsSearchDomainRE matches `scutil --dns`'s first
// "  search domain[0] : <domain>" line. scutil lists one resolver block
// per configured network path, in priority order, with the primary
// network's resolver first; taking the first match is what picks that
// one rather than a VPN's or a scoped/secondary resolver's domain
// further down the output.
var dnsSearchDomainRE = regexp.MustCompile(`(?m)^\s*search domain\[0\]\s*:\s*(\S+)\s*$`)

// parseDNSSearchDomain parses `scutil --dns` output and returns the
// primary resolver's first search domain, if the network advertises
// one. A network with no search domain configured (the common case for
// a home network) prints no "search domain[N]" line at all for that
// resolver, which this reports as not available rather than an empty
// string.
func parseDNSSearchDomain(output string) (string, bool) {
	m := dnsSearchDomainRE.FindStringSubmatch(output)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// arpMacRE matches `arp -n <ip>` output's "at <mac>" field, excluding
// the literal "(incomplete)" placeholder arp prints when it has not
// yet resolved the address.
var arpMacRE = regexp.MustCompile(`\bat\s+([0-9A-Fa-f]{1,2}(?::[0-9A-Fa-f]{1,2}){5})\b`)

// parseARPMac parses `arp -n <ip>` output and returns the resolved MAC
// address. It is false for an incomplete entry ("at (incomplete)"), a
// miss ("<ip> (<ip>) -- no entry"), the empty table a sandboxed or
// unprivileged process sees (see this file's top comment), or any
// output that does not match the expected shape.
func parseARPMac(output string) (string, bool) {
	m := arpMacRE.FindStringSubmatch(output)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// runCommand runs name with args and returns its standard output
// verbatim — callers trim what they need. It never sets LC_ALL/LANG —
// see this file's top-of-file comment for why that is safe here,
// unlike in internal/queue.
func runCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

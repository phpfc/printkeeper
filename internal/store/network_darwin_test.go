package store

import "testing"

// These tests drive the darwin network keyer's parsing from command
// output recorded on real hardware (see network_darwin.go's doc comment
// for how each sample was captured), never from a live network call —
// the parse functions are pure and take a string in, so CI can run them
// without any network at all.

func TestKeyerParsesWiFiDeviceFromHardwarePorts(t *testing.T) {
	const output = `
Hardware Port: Ethernet Adapter (en3)
Device: en3
Ethernet Address: 9a:71:44:81:58:56

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: 36:9f:6d:13:21:00

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: 00:81:2a:f0:99:28

Hardware Port: Thunderbolt 1
Device: en1
Ethernet Address: 36:9f:6d:13:21:00

VLAN Configurations
===================
`
	device, ok := wifiDevice(output)
	if !ok {
		t.Fatalf("wifiDevice() ok = false, want true")
	}
	if device != "en0" {
		t.Errorf("wifiDevice() = %q, want %q", device, "en0")
	}
}

func TestKeyerWiFiDeviceAbsentWhenNoWiFiHardware(t *testing.T) {
	const output = `
Hardware Port: Ethernet Adapter (en3)
Device: en3
Ethernet Address: 9a:71:44:81:58:56

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: 36:9f:6d:13:21:00
`
	if _, ok := wifiDevice(output); ok {
		t.Errorf("wifiDevice() ok = true, want false on a machine with no Wi-Fi hardware port")
	}
}

func TestKeyerParsesSSIDWhenAssociated(t *testing.T) {
	const output = "Current Wi-Fi Network: HomeNetwork5G\n"
	ssid, ok := parseCurrentWiFiNetwork(output)
	if !ok {
		t.Fatalf("parseCurrentWiFiNetwork() ok = false, want true")
	}
	if ssid != "HomeNetwork5G" {
		t.Errorf("parseCurrentWiFiNetwork() = %q, want %q", ssid, "HomeNetwork5G")
	}
}

func TestKeyerParsesSSIDWithSpacesAndColon(t *testing.T) {
	// SSIDs can contain spaces and even colons; the parse must only
	// strip the fixed "Current Wi-Fi Network: " prefix, not split on
	// every colon in the line.
	const output = "Current Wi-Fi Network: Pedro's Office: 5G\n"
	ssid, ok := parseCurrentWiFiNetwork(output)
	if !ok {
		t.Fatalf("parseCurrentWiFiNetwork() ok = false, want true")
	}
	if ssid != "Pedro's Office: 5G" {
		t.Errorf("parseCurrentWiFiNetwork() = %q, want %q", ssid, "Pedro's Office: 5G")
	}
}

func TestKeyerNoSSIDWhenNotAssociated(t *testing.T) {
	const output = "You are not associated with an AirPort network.\n"
	if _, ok := parseCurrentWiFiNetwork(output); ok {
		t.Errorf("parseCurrentWiFiNetwork() ok = true, want false when not associated")
	}
}

func TestKeyerNoSSIDWhenWiFiPowerIsOff(t *testing.T) {
	const output = "All Wi-Fi network services are disabled.\n"
	if _, ok := parseCurrentWiFiNetwork(output); ok {
		t.Errorf("parseCurrentWiFiNetwork() ok = true, want false when Wi-Fi power is off")
	}
}

func TestKeyerParsesDefaultGateway(t *testing.T) {
	const output = `   route to: default
destination: default
       mask: default
    gateway: 192.168.0.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1500         0
`
	gw, ok := parseDefaultGatewayIP(output)
	if !ok {
		t.Fatalf("parseDefaultGatewayIP() ok = false, want true")
	}
	if gw != "192.168.0.1" {
		t.Errorf("parseDefaultGatewayIP() = %q, want %q", gw, "192.168.0.1")
	}
}

func TestKeyerNoDefaultGatewayWhenRouteFails(t *testing.T) {
	// "route -n get default" with no default route configured does not
	// print a "gateway:" line at all (and typically exits non-zero);
	// the parse must fail closed, not panic or return a garbage value.
	const output = "route: writing to routing socket: not in table\n"
	if _, ok := parseDefaultGatewayIP(output); ok {
		t.Errorf("parseDefaultGatewayIP() ok = true, want false with no default route")
	}
}

func TestKeyerParsesDefaultInterface(t *testing.T) {
	const output = `   route to: default
destination: default
       mask: default
    gateway: 192.168.0.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
`
	iface, ok := parseDefaultInterface(output)
	if !ok {
		t.Fatalf("parseDefaultInterface() ok = false, want true")
	}
	if iface != "en0" {
		t.Errorf("parseDefaultInterface() = %q, want %q", iface, "en0")
	}
}

func TestKeyerNoDefaultInterfaceWhenRouteFails(t *testing.T) {
	const output = "route: writing to routing socket: not in table\n"
	if _, ok := parseDefaultInterface(output); ok {
		t.Errorf("parseDefaultInterface() ok = true, want false with no default route")
	}
}

func TestKeyerParsesARPMac(t *testing.T) {
	const output = "? (192.168.0.1) at 74:3a:ef:74:6b:23 on en0 ifscope [ethernet]\n"
	mac, ok := parseARPMac(output)
	if !ok {
		t.Fatalf("parseARPMac() ok = false, want true")
	}
	if mac != "74:3a:ef:74:6b:23" {
		t.Errorf("parseARPMac() = %q, want %q", mac, "74:3a:ef:74:6b:23")
	}
}

func TestKeyerParsesARPMacWithPermanentFlag(t *testing.T) {
	const output = "? (192.168.0.1) at 74:3a:ef:74:6b:23 on en0 ifscope permanent [ethernet]\n"
	mac, ok := parseARPMac(output)
	if !ok {
		t.Fatalf("parseARPMac() ok = false, want true")
	}
	if mac != "74:3a:ef:74:6b:23" {
		t.Errorf("parseARPMac() = %q, want %q", mac, "74:3a:ef:74:6b:23")
	}
}

func TestKeyerNoARPMacWhenIncomplete(t *testing.T) {
	const output = "? (192.168.0.2) at (incomplete) on en0 ifscope [ethernet]\n"
	if _, ok := parseARPMac(output); ok {
		t.Errorf("parseARPMac() ok = true, want false for an incomplete ARP entry")
	}
}

func TestKeyerNoARPMacWhenNoEntry(t *testing.T) {
	const output = "192.168.0.254 (192.168.0.254) -- no entry\n"
	if _, ok := parseARPMac(output); ok {
		t.Errorf("parseARPMac() ok = true, want false when arp reports no entry")
	}
}

func TestKeyerParsesDNSSearchDomain(t *testing.T) {
	const output = `DNS configuration

resolver #1
  search domain[0] : lan
  nameserver[0] : 192.168.0.1
  if_index : 11 (en0)
  flags    : Request A records, Request AAAA records
  reach    : 0x00000002 (Reachable)
`
	domain, ok := parseDNSSearchDomain(output)
	if !ok {
		t.Fatalf("parseDNSSearchDomain() ok = false, want true")
	}
	if domain != "lan" {
		t.Errorf("parseDNSSearchDomain() = %q, want %q", domain, "lan")
	}
}

func TestKeyerNoDNSSearchDomainWhenAbsent(t *testing.T) {
	// A home network with no configured search domain: scutil prints a
	// resolver block with no "search domain[N]" line at all, not an
	// empty one.
	const output = `DNS configuration

resolver #1
  nameserver[0] : 192.168.0.1
  if_index : 11 (en0)
  flags    : Request A records, Request AAAA records
  reach    : 0x00000002 (Reachable)
`
	if _, ok := parseDNSSearchDomain(output); ok {
		t.Errorf("parseDNSSearchDomain() ok = true, want false when no search domain is configured")
	}
}

// --- composeKey: the orchestration-level property tests the fix round
// requires. composeKey is still a pure function — it takes the raw
// output of all four commands as plain strings and returns the
// composite key — so these drive it from recorded output exactly like
// every other test in this file, with no live command execution.

func TestKeyerComposeKeyIncludesEveryAvailableSignal(t *testing.T) {
	const routeOutput = `   route to: default
destination: default
       mask: default
    gateway: 192.168.0.1
  interface: en0
`
	const dnsOutput = "DNS configuration\n\nresolver #1\n  search domain[0] : lan\n"
	const airportOutput = "Current Wi-Fi Network: HomeNetwork5G\n"
	const arpOutput = "? (192.168.0.1) at 74:3a:ef:74:6b:23 on en0 ifscope [ethernet]\n"

	got := composeKey(routeOutput, dnsOutput, airportOutput, arpOutput)
	want := "gw:192.168.0.1|if:en0|domain:lan|ssid:HomeNetwork5G|mac:74:3a:ef:74:6b:23"
	if got != want {
		t.Errorf("composeKey() = %q, want %q", got, want)
	}
}

// TestKeyerComposeKeyIsNotUnknownWithGatewayOnly pins requirement 1 from
// the fix round: "unknown" must be reserved for genuinely no network.
// A gateway with every enrichment signal unavailable (no DNS search
// domain, no Wi-Fi association, no ARP entry) must still produce a real
// key derived from the gateway, never fall through to "unknown".
func TestKeyerComposeKeyIsNotUnknownWithGatewayOnly(t *testing.T) {
	const routeOutput = `   route to: default
destination: default
       mask: default
    gateway: 192.168.0.1
  interface: en0
`
	const dnsOutput = "" // no scutil output / no search domain configured
	const airportOutput = "You are not associated with an AirPort network.\n"
	const arpOutput = "192.168.0.1 (192.168.0.1) -- no entry\n"

	got := composeKey(routeOutput, dnsOutput, airportOutput, arpOutput)
	if got == unknownKey {
		t.Fatalf("composeKey() = %q, want a real key derived from the reachable gateway, not the unknown sentinel", got)
	}
	want := "gw:192.168.0.1|if:en0"
	if got != want {
		t.Errorf("composeKey() = %q, want %q", got, want)
	}
}

// TestKeyerComposeKeyReturnsUnknownWithNoDefaultRoute is the other half
// of requirement 1: "unknown" only when there genuinely is no network.
func TestKeyerComposeKeyReturnsUnknownWithNoDefaultRoute(t *testing.T) {
	const routeOutput = "route: writing to routing socket: not in table\n"
	got := composeKey(routeOutput, "", "", "")
	if got != unknownKey {
		t.Errorf("composeKey() = %q, want %q with no default route", got, unknownKey)
	}
}

// TestKeyerComposeKeyIgnoresNotAssociatedAirportOutput pins requirement
// 2: "You are not associated with an AirPort network" is indistinguishable
// from a real Location-permission denial, so it must contribute nothing
// to the key — not a stable constant that would be identical on every
// network and look like a working signal.
func TestKeyerComposeKeyIgnoresNotAssociatedAirportOutput(t *testing.T) {
	const routeOutput = `   route to: default
destination: default
       mask: default
    gateway: 192.168.0.1
  interface: en0
`
	notAssociated := composeKey(routeOutput, "", "You are not associated with an AirPort network.\n", "")
	noAirportOutputAtAll := composeKey(routeOutput, "", "", "")
	if notAssociated != noAirportOutputAtAll {
		t.Errorf("composeKey() with \"not associated\" output = %q, want the same as with no airport output at all (%q) — the denial message must not contribute a distinct signal", notAssociated, noAirportOutputAtAll)
	}
}

// TestKeyerComposeKeyDiffersByGatewayAlone is the laptop-at-home-and-
// office property the fix round asks for directly: two recorded
// environments differing only in gateway IP (everything else held
// identical — same interface, no DNS search domain, no Wi-Fi
// association, no ARP entry) must still produce different keys, because
// the gateway IP is part of the foundation tier, not an enrichment.
func TestKeyerComposeKeyDiffersByGatewayAlone(t *testing.T) {
	const notAssociated = "You are not associated with an AirPort network.\n"
	const noARPEntry = "192.168.0.1 (192.168.0.1) -- no entry\n"

	const homeRoute = `   route to: default
destination: default
       mask: default
    gateway: 192.168.0.1
  interface: en0
`
	const officeRoute = `   route to: default
destination: default
       mask: default
    gateway: 10.0.5.1
  interface: en0
`

	home := composeKey(homeRoute, "", notAssociated, noARPEntry)
	office := composeKey(officeRoute, "", notAssociated, noARPEntry)
	if home == office {
		t.Fatalf("composeKey() for home (%q) and office (%q) gateways produced the same key — per-network keying cannot distinguish them", home, office)
	}
}

func TestNewDarwinNetworkKeyerReturnsNetworkKeyer(t *testing.T) {
	var _ NetworkKeyer = NewDarwinNetworkKeyer()
}

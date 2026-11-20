package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
)

// noNetwork is a networkChecker fake that always reports no network at
// all. It lets the "closed laptop" case be proven through injection rather
// than by actually disabling this machine's network interface.
type noNetwork struct{}

func (noNetwork) HasNetwork() bool { return false }

// alwaysNetwork is a networkChecker fake that always reports a network is
// present, isolating the StateAbsent path from this machine's real
// network state.
type alwaysNetwork struct{}

func (alwaysNetwork) HasNetwork() bool { return true }

// TestProbeWithoutNetworkReturnsUnknownNotAbsent pins Review Focus 5: a
// machine with no network at all must report StateUnknown, never
// StateAbsent. StateAbsent means "I asked and nothing answered";
// StateUnknown means "I could not ask." A closed laptop on a train has not
// lost its printer, and the ladder must do nothing in that case.
func TestProbeWithoutNetworkReturnsUnknownNotAbsent(t *testing.T) {
	r := probeWith(noNetwork{}, context.Background(), "ipp://10.0.0.9/ipp/print", time.Second)
	if r.State != StateUnknown {
		t.Fatalf("offline machine reported %v; a missing network is not a missing printer", r.State)
	}
}

// TestProbeConnectionFailureWithNetworkReturnsAbsent is the counterpart to
// the offline test above: when the network is up but nothing answers, the
// probe must report StateAbsent, not StateUnknown. Port 1 on loopback is
// never listening, so the connection is refused immediately without
// depending on this machine's real LAN.
func TestProbeConnectionFailureWithNetworkReturnsAbsent(t *testing.T) {
	r := probeWith(alwaysNetwork{}, context.Background(), "ipp://127.0.0.1:1/ipp/print", time.Second)
	if r.State != StateAbsent {
		t.Fatalf("unreachable printer with network up reported %v; want StateAbsent", r.State)
	}
}

// TestClassifyStoppedPrinterIsErroredNotAbsent pins Review Focus 2: a
// printer that answers but reports itself stopped — out of paper, cover
// open, jammed — is PRESENT, just unhealthy. It must classify as
// StateErrored, never StateAbsent: the repair ladder must not conclude the
// printer moved and rewrite a healthy queue's address just because someone
// left the paper tray empty.
func TestClassifyStoppedPrinterIsErroredNotAbsent(t *testing.T) {
	attrs := map[string][]string{
		"printer-state":         {"5"}, // IPP printer-state enum: 5 = stopped
		"printer-state-reasons": {"media-jam-error"},
	}
	if got := classify(attrs); got != StateErrored {
		t.Fatalf("stopped printer with reasons %v classified %v; want StateErrored", attrs["printer-state-reasons"], got)
	}
}

// TestClassifyIdlePrinterIsPresent is the happy-path counterpart: a
// healthy printer with no error reasons classifies as StatePresent.
func TestClassifyIdlePrinterIsPresent(t *testing.T) {
	attrs := map[string][]string{
		"printer-state":         {"3"}, // IPP printer-state enum: 3 = idle
		"printer-state-reasons": {"none"},
	}
	if got := classify(attrs); got != StatePresent {
		t.Fatalf("idle printer classified %v; want StatePresent", got)
	}
}

// linkLocalAddr builds a net.Addr carrying ip, for feeding synthetic
// interface addresses into hasRoutableAddress without touching this
// machine's real interfaces.
func addrOf(t *testing.T, ip string) net.Addr {
	t.Helper()
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Fatalf("bad test IP %q", ip)
	}
	bits := 32
	if parsed.To4() == nil {
		bits = 128
	}
	return &net.IPNet{IP: parsed, Mask: net.CIDRMask(bits, bits)}
}

// TestHasRoutableAddressRejectsIPv4LinkLocal pins the DHCP-failure case: a
// macOS interface that failed to get a DHCP lease self-assigns a
// 169.254.0.0/16 address. The interface looks "up" with an address, but
// there is no route anywhere — that is "I could not ask", not "I asked
// and nothing answered", so it must not count as having a network.
func TestHasRoutableAddressRejectsIPv4LinkLocal(t *testing.T) {
	addrs := []net.Addr{addrOf(t, "169.254.12.34")}
	if hasRoutableAddress(addrs) {
		t.Fatal("169.254.0.0/16 self-assigned address counted as routable; a failed DHCP lease is not connectivity")
	}
}

// TestHasRoutableAddressRejectsIPv6LinkLocal is the IPv6 counterpart:
// fe80::/10 is link-local and never routed off the segment.
func TestHasRoutableAddressRejectsIPv6LinkLocal(t *testing.T) {
	addrs := []net.Addr{addrOf(t, "fe80::1")}
	if hasRoutableAddress(addrs) {
		t.Fatal("fe80::/10 link-local address counted as routable; it cannot reach anything off-segment")
	}
}

// TestHasRoutableAddressAcceptsRoutableAddress is the happy-path
// counterpart: an ordinary private-network address (the kind a working
// DHCP lease actually hands out) does count as having a network.
func TestHasRoutableAddressAcceptsRoutableAddress(t *testing.T) {
	addrs := []net.Addr{addrOf(t, "192.168.1.42")}
	if !hasRoutableAddress(addrs) {
		t.Fatal("ordinary routable address not counted as having a network")
	}
}

// ippStub starts an HTTP server that answers every request with the given
// raw bytes as the IPP response body, standing in for a real device
// without needing ippeveprinter (which, being a healthy, spec-compliant
// implementation, cannot be made to send either of these responses). It
// returns an ipp:// URI pointing at the stub.
func ippStub(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", goipp.ContentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return "ipp://" + srv.Listener.Addr().String() + "/ipp/print"
}

// TestProbeNonSuccessIPPStatusIsErroredNotAbsent pins the heart of this
// task: a device that answers over IPP but reports a non-success status
// (here, server-error-device-error — exactly what a genuinely
// malfunctioning device could plausibly send) has still answered. Bytes
// came back, so it is present — StateErrored, never StateAbsent. Getting
// this backwards is how the repair ladder would rewrite a healthy queue's
// address because a printer had a bad day.
func TestProbeNonSuccessIPPStatusIsErroredNotAbsent(t *testing.T) {
	msg := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusErrorDevice, 1)
	payload, err := msg.EncodeBytes()
	if err != nil {
		t.Fatalf("encode fixture response: %v", err)
	}

	uri := ippStub(t, payload)
	r := probeWith(alwaysNetwork{}, context.Background(), uri, time.Second)
	if r.State != StateErrored {
		t.Fatalf("device answering with ipp status %s reported %v (err=%v); want StateErrored", goipp.StatusErrorDevice, r.State, r.Err)
	}
}

// TestProbeUndecodableResponseIsErroredNotAbsent is the other half of the
// same distinction: a response body that cannot even be parsed as IPP
// still proves the device is reachable — it sent bytes. That must not be
// folded into the same path as a dead socket.
func TestProbeUndecodableResponseIsErroredNotAbsent(t *testing.T) {
	uri := ippStub(t, []byte("not an ipp message"))
	r := probeWith(alwaysNetwork{}, context.Background(), uri, time.Second)
	if r.State != StateErrored {
		t.Fatalf("device sending an undecodable body reported %v (err=%v); want StateErrored — it is reachable, not absent", r.State, r.Err)
	}
}

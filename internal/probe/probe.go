// Package probe answers the one question CUPS cannot: is the printer
// actually there right now? A spike confirmed that CUPS reports a dead
// printer as "idle" and "accepting requests" seconds after it was
// unplugged — there is no event, no flag, no state change. The only way
// to know is to ask the device directly over IPP.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/phpfc/printkeeper/internal/ippclient"
)

// State is the outcome of asking a printer whether it is there.
type State int

const (
	// StatePresent means the device answered and is healthy.
	StatePresent State = iota
	// StateErrored means the device answered but reports it cannot
	// currently print (out of paper, cover open, jammed). It is still
	// present — only unhealthy.
	StateErrored
	// StateAbsent means nothing answered, and the machine had a usable
	// network route to try with. The device is most likely gone.
	StateAbsent
	// StateUnknown means the probe could not be attempted meaningfully:
	// the machine itself has no network. This is deliberately distinct
	// from StateAbsent — "I could not ask" is not "I asked and nothing
	// answered" — so callers must take no action on it.
	StateUnknown
)

func (s State) String() string {
	switch s {
	case StatePresent:
		return "present"
	case StateErrored:
		return "errored"
	case StateAbsent:
		return "absent"
	case StateUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// Result is the outcome of one Probe call.
type Result struct {
	State State
	// PrinterState is the raw printer-state attribute value as reported
	// by the device (IPP enum, e.g. "3", "4", "5"), empty when the
	// device did not answer.
	PrinterState string
	// Attributes is the full set of attributes requested from the
	// device, empty when it did not answer.
	Attributes map[string][]string
	RTT        time.Duration
	Err        error
}

// requestedAttributes are the keywords Probe asks the device for. It asks
// for exactly what it needs to decide reachability and health — not "all",
// which is Inspect's job.
var requestedAttributes = []string{
	"printer-state",
	"printer-state-reasons",
	"printer-uuid",
	"printer-device-id",
}

// printerStateStopped is the IPP printer-state enum value for "stopped"
// (RFC 8011 §5.4.11: 3=idle, 4=processing, 5=stopped). goipp decodes enum
// values to their raw integer string, not a keyword, so the comparison is
// against "5" rather than the word "stopped".
const printerStateStopped = "5"

// successStatusCeiling is one past the end of IPP's "successful" status
// range (RFC 8011 §13.1): ok (0x0000) through ok-events-complete
// (0x0007) all sit below 0x0100; redirection, client-error, and
// server-error statuses are 0x0200 and above.
const successStatusCeiling = goipp.Status(0x0100)

// isSuccessStatus reports whether status is in IPP's "successful" family.
func isSuccessStatus(status goipp.Status) bool {
	return status < successStatusCeiling
}

// networkChecker reports whether the host currently has a usable network
// route. It exists so Probe's behavior when a connection fails can be
// driven by a test through injection — proving the "no network at all"
// case without actually disabling the test machine's network.
type networkChecker interface {
	HasNetwork() bool
}

// systemNetwork is the production networkChecker, backed by HasNetwork.
type systemNetwork struct{}

func (systemNetwork) HasNetwork() bool { return HasNetwork() }

// Probe asks uri, over IPP, whether the printer is there and healthy. It
// never trusts CUPS's queue state — it talks to the device directly, with
// the given timeout bounding the round trip.
func Probe(ctx context.Context, uri string, timeout time.Duration) Result {
	return probeWith(systemNetwork{}, ctx, uri, timeout)
}

// probeWith is Probe with its networkChecker injected, so tests can pin
// the StateAbsent/StateUnknown distinction without touching this
// machine's real network.
//
// The question that drives every branch below is "did any bytes come
// back?" — not "did the device say everything is fine?". A response
// that arrives at all, however malformed or however bad its IPP status,
// proves the device is reachable: StateErrored, never StateAbsent.
// Nothing coming back at all is the only path to StateAbsent/StateUnknown.
func probeWith(nc networkChecker, ctx context.Context, uri string, timeout time.Duration) Result {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	attrs, status, rtt, err := ippclient.GetPrinterAttributes(reqCtx, uri, requestedAttributes)
	if err != nil {
		var malformed *ippclient.MalformedResponseError
		if errors.As(err, &malformed) {
			// The HTTP transport succeeded — bytes came back — they just
			// didn't decode as IPP. The device is there; it answered
			// strangely, which is "broken", not "gone".
			return Result{State: StateErrored, RTT: rtt, Err: err}
		}

		var httpStatus *ippclient.HTTPStatusError
		if errors.As(err, &httpStatus) {
			// An HTTP status is still a response: something answered the
			// connection and said so explicitly — busy, forbidden, wants
			// credentials. That is "there, but it said no", not "gone".
			// Classifying it as absent would have the repair ladder
			// conclude a busy or credential-gated printer moved and
			// re-resolve it, which is the failure this boundary exists
			// to prevent.
			return Result{State: StateErrored, RTT: rtt, Err: err}
		}

		state := StateAbsent
		if !nc.HasNetwork() {
			state = StateUnknown
		}
		return Result{State: state, RTT: rtt, Err: err}
	}

	if !isSuccessStatus(status) {
		// The device decoded and answered, just not with success: it
		// explicitly told us it could not service the request. That is
		// still a response — present, just broken — so it must not fall
		// through to classify's StatePresent default just because no
		// printer-state-reasons happened to carry an "-error" suffix.
		return Result{
			State:        StateErrored,
			PrinterState: firstValue(attrs, "printer-state"),
			Attributes:   attrs,
			RTT:          rtt,
			Err:          fmt.Errorf("device returned ipp status %s", status),
		}
	}

	return Result{
		State:        classify(attrs),
		PrinterState: firstValue(attrs, "printer-state"),
		Attributes:   attrs,
		RTT:          rtt,
	}
}

// classify maps a device's reported printer-state and
// printer-state-reasons to a State. A response that arrived at all means
// the device is present — the only remaining question is whether it is
// healthy. A printer-state of "stopped", or any state-reason carrying the
// IPP "-error" severity suffix, means the device answered but cannot
// currently print (out of paper, cover open, jammed): that is
// StateErrored, never StateAbsent. Treating an error state as absent would
// make the repair ladder conclude the printer moved and rewrite a healthy
// queue's address — someone would refill the paper tray and find their
// printer reconfigured.
func classify(attrs map[string][]string) State {
	if firstValue(attrs, "printer-state") == printerStateStopped {
		return StateErrored
	}
	for _, reason := range attrs["printer-state-reasons"] {
		if strings.HasSuffix(reason, "-error") {
			return StateErrored
		}
	}
	return StatePresent
}

// firstValue returns the first value reported for name, or "" if absent.
func firstValue(attrs map[string][]string, name string) string {
	if v := attrs[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// HasNetwork reports whether the host has any usable network route: a
// non-loopback interface that is up and carries a routable (non
// link-local) address. It is what lets Probe tell "the printer is gone"
// apart from "I have no way to ask" — a closed laptop on a train has lost
// its network, not its printer.
//
// This deliberately inspects interfaces/addresses rather than reading the
// OS route table: a syscall-level route-socket read would be the more
// literal "default route" check, but it is platform-specific machinery
// disproportionate to what this needs, and the interface/address view
// already distinguishes every case that matters here without resorting to
// parsing any (possibly localized) CLI tool's output.
func HasNetwork() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		if hasRoutableAddress(addrs) {
			return true
		}
	}
	return false
}

// hasRoutableAddress reports whether any address in addrs is a usable,
// routable unicast address. It excludes link-local/self-assigned
// addresses — 169.254.0.0/16 (IPv4) and fe80::/10 (IPv6) — because macOS
// assigns one of these to an interface that is up but whose DHCP lease
// failed: the interface looks connected, but there is no route anywhere.
// That is "I could not ask", not "I asked and nothing answered", so it
// must not be mistaken for a usable network. Pulled out as its own
// function so tests can drive it with synthetic addresses instead of this
// machine's real interfaces.
func hasRoutableAddress(addrs []net.Addr) bool {
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.IsLinkLocalMulticast() {
			continue
		}
		return true
	}
	return false
}

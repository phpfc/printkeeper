package inspect

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/phpfc/printkeeper/internal/discovery"
)

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

// TestAnonymiseScrubsHostnameInEveryCasingIPPEchoesItBack pins a real leak
// found by reading a captured fixture by hand (not by this test suite): a
// real printer's own IPP attributes echo its hostname back in lower-cased,
// trailing-dot FQDN form (e.g. printer-more-info, printer-supply-info-uri)
// which does not literally, case-sensitively match the mixed-case hostname
// discovery sees on the Candidate. Shapes below are taken from the
// captured ippeveprinter fixture (testdata/fixtures/example-printer.json).
func TestAnonymiseScrubsHostnameInEveryCasingIPPEchoesItBack(t *testing.T) {
	const host = "MacBook-Air-de-Pedro.local"

	r := Report{
		Candidate: discovery.Candidate{
			Host: host,
			URI:  "ipp://" + host + ":34468/ipp/print",
		},
		Attributes: map[string][]string{
			// Same casing as Candidate.Host.
			"printer-uri-supported": {"ipp://" + host + ":34468/ipp/print"},
			// Lower-cased, trailing-dot FQDN form, as a real printer's own
			// attributes echoed it back.
			"printer-more-info":       {"https://macbook-air-de-pedro.local.:34468/"},
			"printer-supply-info-uri": {"https://macbook-air-de-pedro.local.:34468/supplies"},
		},
	}

	a := Anonymise(r)

	rendered := strings.ToLower(fmt.Sprint(a))
	for _, form := range []string{
		host,                        // Candidate's own casing
		strings.ToLower(host),       // lower-cased
		strings.ToLower(host) + ".", // lower-cased, trailing-dot FQDN
	} {
		if strings.Contains(rendered, strings.ToLower(form)) {
			t.Fatalf("hostname leaked in form %q; rendered report: %s", form, fmt.Sprint(a))
		}
	}
}

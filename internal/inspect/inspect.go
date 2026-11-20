// Package inspect captures everything knowable about a real IPP printer: the
// discovery candidate that pointed at it, and the full attribute set returned
// by an actual Get-Printer-Attributes probe.
package inspect

import (
	"context"
	"time"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/ippclient"
)

// Report holds everything captured about one printer: the candidate that
// identified it, the full IPP attribute set as returned by the device, and
// how long the probe round-trip took.
type Report struct {
	Candidate  discovery.Candidate
	Attributes map[string][]string
	RoundTrip  time.Duration
}

// Inspect sends a real Get-Printer-Attributes request to uri and returns
// everything the device reports. The returned Report's Candidate carries
// only the URI; callers that already have a richer discovery.Candidate
// should overwrite that field before anonymising or persisting the report.
//
// Inspect records what came back; it does not judge it. It ignores the
// IPP status GetPrinterAttributes reports — even an error-status response
// still carries attributes worth capturing — and only fails when nothing
// usable came back at all.
func Inspect(ctx context.Context, uri string) (Report, error) {
	attrs, _, roundTrip, err := ippclient.GetPrinterAttributes(ctx, uri, []string{"all"})
	if err != nil {
		return Report{}, err
	}

	return Report{
		Candidate:  discovery.Candidate{URI: uri},
		Attributes: attrs,
		RoundTrip:  roundTrip,
	}, nil
}

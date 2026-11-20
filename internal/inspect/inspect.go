// Package inspect captures everything knowable about a real IPP printer: the
// discovery candidate that pointed at it, and the full attribute set returned
// by an actual Get-Printer-Attributes probe.
package inspect

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/phpfc/printkeeper/internal/discovery"
)

// ippsClient is used for all probes. Printers serving ipps:// almost never
// present a certificate signed by a trusted CA — it is self-signed by the
// device itself — so, like every other IPP client (ipptool included),
// verification is skipped. This is a diagnostic probe of a LAN device, not
// a browser visiting an untrusted site.
var ippsClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	},
}

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
func Inspect(ctx context.Context, uri string) (Report, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(uri)))
	req.Operation.Add(goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("all")))

	payload, err := req.EncodeBytes()
	if err != nil {
		return Report{}, fmt.Errorf("encode ipp request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, toHTTPURL(uri), bytes.NewReader(payload))
	if err != nil {
		return Report{}, fmt.Errorf("build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)

	start := time.Now()
	resp, err := ippsClient.Do(httpReq)
	roundTrip := time.Since(start)
	if err != nil {
		return Report{}, fmt.Errorf("ipp request to %s: %w", uri, err)
	}
	defer resp.Body.Close()

	var msg goipp.Message
	if err := msg.Decode(resp.Body); err != nil {
		return Report{}, fmt.Errorf("decode ipp response from %s: %w", uri, err)
	}

	attrs := map[string][]string{}
	for _, group := range msg.AttrGroups() {
		for _, attr := range group.Attrs {
			for _, v := range attr.Values {
				attrs[attr.Name] = append(attrs[attr.Name], v.V.String())
			}
		}
	}

	return Report{
		Candidate:  discovery.Candidate{URI: uri},
		Attributes: attrs,
		RoundTrip:  roundTrip,
	}, nil
}

// toHTTPURL rewrites an ipp(s):// printer URI to the http(s):// URL IPP is
// actually transported over.
func toHTTPURL(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	switch u.Scheme {
	case "ipp":
		u.Scheme = "http"
	case "ipps":
		u.Scheme = "https"
	}
	return u.String()
}

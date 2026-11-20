// Package ippclient sends IPP Get-Printer-Attributes requests and decodes
// the response into a flat attribute map. It is the one place that knows
// how to build and parse that request over HTTP(S); internal/inspect and
// internal/probe both build on it instead of each encoding IPP themselves.
package ippclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/OpenPrinting/goipp"
)

// client is used for all requests. Printers serving ipps:// almost never
// present a certificate signed by a trusted CA — it is self-signed by the
// device itself — so, like every other IPP client (ipptool included),
// verification is skipped. This is a diagnostic probe of a LAN device, not
// a browser visiting an untrusted site.
var client = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	},
}

// MalformedResponseError means the device's HTTP transport succeeded —
// bytes came back — but the response body could not be decoded as an IPP
// message. Unlike every other error GetPrinterAttributes can return, this
// one proves the device is reachable: callers that distinguish "gone"
// from "broken" must treat it as the latter, never as a dead connection.
type MalformedResponseError struct {
	err error
}

// Error returns the same text GetPrinterAttributes has always returned
// for a decode failure; wrapping it in this type adds a way for callers
// to detect "bytes arrived" without changing what the error says.
func (e *MalformedResponseError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying decode error to errors.Is / errors.As.
func (e *MalformedResponseError) Unwrap() error { return e.err }

// GetPrinterAttributes sends a Get-Printer-Attributes request to uri asking
// for the given attribute keywords (use []string{"all"} for everything the
// device will report) and returns the flattened attribute map, the IPP
// status code the device reported, and the round-trip time. The context
// governs cancellation and deadline; callers that need a bounded wait
// should derive a context with a timeout before calling.
//
// The status is only meaningful when err is nil — it reports whatever the
// device said, success or failure alike; GetPrinterAttributes itself does
// not judge it. A caller that wants to tell "answered successfully" apart
// from "answered with an error status" must inspect the returned status.
//
// The round-trip time is returned even on error, since a timed-out or
// refused connection still took measurable time and callers may want it
// for diagnostics.
func GetPrinterAttributes(ctx context.Context, uri string, requested []string) (map[string][]string, goipp.Status, time.Duration, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(uri)))

	reqAttrs := goipp.Attribute{Name: "requested-attributes"}
	for _, name := range requested {
		reqAttrs.Values.Add(goipp.TagKeyword, goipp.String(name))
	}
	req.Operation.Add(reqAttrs)

	payload, err := req.EncodeBytes()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("encode ipp request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, toHTTPURL(uri), bytes.NewReader(payload))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)

	start := time.Now()
	resp, err := client.Do(httpReq)
	rtt := time.Since(start)
	if err != nil {
		return nil, 0, rtt, fmt.Errorf("ipp request to %s: %w", uri, err)
	}
	defer resp.Body.Close()

	var msg goipp.Message
	if err := msg.Decode(resp.Body); err != nil {
		return nil, 0, rtt, &MalformedResponseError{err: fmt.Errorf("decode ipp response from %s: %w", uri, err)}
	}

	attrs := map[string][]string{}
	for _, group := range msg.AttrGroups() {
		for _, attr := range group.Attrs {
			for _, v := range attr.Values {
				attrs[attr.Name] = append(attrs[attr.Name], v.V.String())
			}
		}
	}

	return attrs, goipp.Status(msg.Code), rtt, nil
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

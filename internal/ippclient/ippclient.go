// Package ippclient sends IPP requests over HTTP(S) and decodes the
// response into attribute maps. It is the one place that knows how to
// build and parse those requests; internal/inspect, internal/probe, and
// internal/queue all build on it instead of each encoding IPP
// themselves.
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
	req.Operation.Add(requestedAttributesValue(requested))

	msg, rtt, err := sendRequest(ctx, uri, req)
	if err != nil {
		return nil, 0, rtt, err
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

// CupsGetPrinters sends a CUPS-Get-Printers request to the server at uri
// (e.g. "ipp://localhost:631/") asking for the given attribute keywords,
// and returns one attribute map per printer the server reports.
//
// Unlike GetPrinterAttributes, which asks about a single device and so
// may safely flatten every group in the response into one map,
// CUPS-Get-Printers answers for the whole server: its response carries
// one printer-attributes group per queue configured on the host.
// Flattening those together would merge unrelated queues' attributes
// into a single record, so each group is kept separate here.
//
// A host with no queues configured is not an error: cupsd answers that
// case with status client-error-not-found rather than an empty successful
// response, so callers must treat that specific status as "zero
// printers", not as a failure.
//
// The natural-language value here is "en", not "en-US" as
// GetPrinterAttributes uses for a device: a spike found that this
// server's own cupsd — unlike the devices GetPrinterAttributes talks to
// — validates this vendor-extension operation's language tag strictly
// against RFC 3066 and rejects "en-US" with client-error-bad-request.
// "en" is accepted.
func CupsGetPrinters(ctx context.Context, uri string, requested []string) ([]map[string][]string, goipp.Status, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetPrinters, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	req.Operation.Add(requestedAttributesValue(requested))

	msg, _, err := sendRequest(ctx, uri, req)
	if err != nil {
		return nil, 0, err
	}

	var printers []map[string][]string
	for _, group := range msg.AttrGroups() {
		if group.Tag != goipp.TagPrinterGroup {
			continue
		}
		attrs := map[string][]string{}
		for _, attr := range group.Attrs {
			for _, v := range attr.Values {
				attrs[attr.Name] = append(attrs[attr.Name], v.V.String())
			}
		}
		printers = append(printers, attrs)
	}

	return printers, goipp.Status(msg.Code), nil
}

// requestedAttributesValue builds the "requested-attributes" operation
// attribute shared by every IPP request in this package that asks the
// server to limit what it reports.
func requestedAttributesValue(requested []string) goipp.Attribute {
	attr := goipp.Attribute{Name: "requested-attributes"}
	for _, name := range requested {
		attr.Values.Add(goipp.TagKeyword, goipp.String(name))
	}
	return attr
}

// sendRequest encodes req, POSTs it to uri, and decodes the response
// into an IPP message. It is the one place that knows how to perform
// that HTTP round trip; every operation in this package builds on it
// instead of repeating the encode/post/decode sequence itself.
func sendRequest(ctx context.Context, uri string, req *goipp.Message) (*goipp.Message, time.Duration, error) {
	payload, err := req.EncodeBytes()
	if err != nil {
		return nil, 0, fmt.Errorf("encode ipp request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, toHTTPURL(uri), bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)

	start := time.Now()
	resp, err := client.Do(httpReq)
	rtt := time.Since(start)
	if err != nil {
		return nil, rtt, fmt.Errorf("ipp request to %s: %w", uri, err)
	}
	defer resp.Body.Close()

	var msg goipp.Message
	if err := msg.Decode(resp.Body); err != nil {
		return nil, rtt, &MalformedResponseError{err: fmt.Errorf("decode ipp response from %s: %w", uri, err)}
	}

	return &msg, rtt, nil
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

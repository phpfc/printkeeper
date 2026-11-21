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

// client is used for every request this package sends, to a printer or to
// this machine's own cupsd alike.
//
// The certificate verification decision: this client never checks a
// server certificate. That is a deliberate trade-off, not an oversight,
// and it is scoped as narrowly as this package can make it:
//
//   - It only ever matters for printer connections. CupsGetPrinters talks
//     exclusively to cupsBaseURI ("ipp://localhost:631/", see
//     internal/queue), which is plain HTTP and never negotiates TLS at
//     all — this setting has no effect on that traffic.
//   - Real printers — including the one this task was filed against —
//     serve ipps:// with a certificate self-signed by the device itself;
//     there is no CA to validate against. CUPS itself trusts a printer's
//     certificate on first use for the same reason. ipptool, which reads
//     these same devices without complaint, does not verify either.
//   - printkeeper sends no credentials over this connection: only
//     attribute queries, and later, print jobs the user themselves
//     initiated. A spoofed printer could at most lie about its own
//     attributes or receive a document the user already chose to print
//     to it — not exfiltrate a secret.
//   - A narrower scope — skip verification only on the retry performed
//     after a 426 Upgrade Required, in sendRequest below — was
//     considered and rejected: discovered printers can advertise an
//     ipps:// URI directly (see internal/discovery), with no 426 dance
//     and no retry to scope the exemption to. Limiting this to the retry
//     path alone would leave every such device unreadable, which is the
//     same failure this task exists to fix.
//
// The alternative is printkeeper simply not working against a large
// class of real printers, which is a worse outcome than this.
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

// HTTPStatusError means the device's HTTP transport rejected the request
// outright — a status other than success and other than 426 Upgrade
// Required (which sendRequest handles itself, see below) — before any
// attempt was made to decode a body as IPP. A 401 or 403 is a rejection,
// not a garbled response: unlike MalformedResponseError, this type must
// never be read by a caller as "bytes arrived, so the device is merely
// unwell" — probe, in particular, relies on that boundary staying clean.
type HTTPStatusError struct {
	URI        string
	StatusCode int
}

// Error names the URI and the rejecting status, e.g. "ipp request to
// ipp://printer.local/ipp/print: unexpected http status 401 Unauthorized".
func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("ipp request to %s: unexpected http status %d %s", e.URI, e.StatusCode, http.StatusText(e.StatusCode))
}

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

// sendRequest encodes req, POSTs it to uri, and decodes the response into
// an IPP message. It is the one place that knows how to perform that HTTP
// round trip; every operation in this package builds on it instead of
// repeating the encode/post/decode sequence itself.
//
// Before decoding, it inspects the HTTP status. Some real printers —
// the one this task was filed against among them — refuse plaintext IPP
// on their well-known port outright, answering 426 Upgrade Required with
// an empty body instead of anything resembling IPP; ipptool reads the
// same devices without trouble only because CUPS's HTTP layer performs
// the upgrade underneath it. So a 426 here triggers exactly one retry of
// the identical request against https://host:port/path — the same
// host:port the original URI named, not a different endpoint — before
// giving up. Any other non-success status is reported as HTTPStatusError
// rather than handed to the decoder: an empty or unexpected body for a
// 401 or 403 would otherwise decode-fail and look exactly like
// MalformedResponseError's "bytes arrived but didn't parse" case, which
// would mislead callers like probe that treat that error as proof the
// device is present.
func sendRequest(ctx context.Context, uri string, req *goipp.Message) (*goipp.Message, time.Duration, error) {
	payload, err := req.EncodeBytes()
	if err != nil {
		return nil, 0, fmt.Errorf("encode ipp request: %w", err)
	}

	start := time.Now()
	resp, err := postIPP(ctx, toHTTPURL(uri), payload)
	if err != nil {
		return nil, time.Since(start), fmt.Errorf("ipp request to %s: %w", uri, err)
	}

	if resp.StatusCode == http.StatusUpgradeRequired {
		resp.Body.Close()

		httpsURL, convErr := toHTTPSURL(uri)
		if convErr != nil {
			return nil, time.Since(start), fmt.Errorf("ipp request to %s: %w", uri, convErr)
		}
		resp, err = postIPP(ctx, httpsURL, payload)
		if err != nil {
			return nil, time.Since(start), fmt.Errorf("ipp request to %s: retry over tls: %w", uri, err)
		}
	}
	rtt := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, rtt, &HTTPStatusError{URI: uri, StatusCode: resp.StatusCode}
	}

	var msg goipp.Message
	if err := msg.Decode(resp.Body); err != nil {
		return nil, rtt, &MalformedResponseError{err: fmt.Errorf("decode ipp response from %s: %w", uri, err)}
	}

	return &msg, rtt, nil
}

// postIPP sends payload as an IPP-typed POST to url and returns the raw
// HTTP response, unexamined — callers decide what each status means.
func postIPP(ctx context.Context, url string, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)
	return client.Do(httpReq)
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

// toHTTPSURL rewrites uri to an https:// URL on the identical host:port
// and path, regardless of uri's original scheme. It is used only for the
// retry sendRequest performs after a 426 Upgrade Required: the device
// that refused plaintext is asked again, over TLS, at the same address —
// never a different host or port the device did not name.
func toHTTPSURL(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("parse uri %s: %w", uri, err)
	}
	u.Scheme = "https"
	return u.String(), nil
}

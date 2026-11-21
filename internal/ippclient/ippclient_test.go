package ippclient

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
)

// goldenAttributesResponse encodes a valid, successful Get-Printer-Attributes
// response carrying one attribute, standing in for what a real printer
// sends once the connection is actually speaking IPP.
func goldenAttributesResponse(t *testing.T) []byte {
	t.Helper()
	msg := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 1)
	msg.Printer.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("TLS Test Printer")))
	payload, err := msg.EncodeBytes()
	if err != nil {
		t.Fatalf("encode golden ipp response: %v", err)
	}
	return payload
}

// selfSignedCert builds a throwaway, self-signed certificate for
// 127.0.0.1, standing in for the kind of certificate a real printer
// presents: not signed by any CA, trusted only because the caller already
// knows who it is dialing.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create self-signed certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// drainHTTPRequest reads exactly one HTTP request off conn and discards its
// body, so the connection is left clean for the response to follow.
func drainHTTPRequest(conn net.Conn) {
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	io.Copy(io.Discard, req.Body) //nolint:errcheck
	req.Body.Close()
}

// startUpgradeServer reproduces, on a single TCP port, exactly what
// curl saw from the real Epson printer that prompted this task: the first
// connection gets a plain-HTTP 426 Upgrade Required with an Upgrade
// header, and only a subsequent connection that actually performs a TLS
// handshake receives the real IPP response. The listener is never closed
// or rebound between the two phases — both are ready before the client
// ever dials — so there is no race between "serve the 426" and "serve the
// TLS leg" the way there would be if the two legs used two independently
// started servers sharing a port.
func startUpgradeServer(t *testing.T, golden []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	cert := selfSignedCert(t)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		drainHTTPRequest(conn)
		fmt.Fprint(conn, "HTTP/1.1 426 Upgrade Required\r\nUpgrade: TLS/1.0, HTTP/1.1\r\nConnection: Upgrade\r\nContent-Length: 0\r\n\r\n")
		conn.Close()

		conn2, err := ln.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(conn2, tlsConfig)
		drainHTTPRequest(tlsConn)
		fmt.Fprintf(tlsConn, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", goipp.ContentType, len(golden))
		tlsConn.Write(golden) //nolint:errcheck
		tlsConn.Close()
	}()

	return ln.Addr().String()
}

// TestGetPrinterAttributesUpgradesToTLSOn426 pins the fix this task exists
// for: a printer that refuses plaintext IPP on its well-known port with a
// 426 Upgrade Required must still be readable. The client is pointed at
// the plaintext endpoint first; the production code must notice the 426
// and retry over TLS against the identical host:port before this test's
// attributes come back decoded rather than as a truncated-message error.
func TestGetPrinterAttributesUpgradesToTLSOn426(t *testing.T) {
	addr := startUpgradeServer(t, goldenAttributesResponse(t))

	uri := "ipp://" + addr + "/ipp/print"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	attrs, status, _, err := GetPrinterAttributes(ctx, uri, []string{"all"})
	if err != nil {
		t.Fatalf("GetPrinterAttributes(%s) = _, _, _, %v; want decoded attributes after retrying over TLS", uri, err)
	}
	if status != goipp.StatusOk {
		t.Fatalf("status = %s, want %s", status, goipp.StatusOk)
	}
	if got := attrs["printer-name"]; len(got) != 1 || got[0] != "TLS Test Printer" {
		t.Fatalf("attrs[printer-name] = %v, want [TLS Test Printer]", got)
	}
}

// TestGetPrinterAttributesNonUpgradeFailureStatusIsNotMalformed pins the
// other half of this task: a 401 or 503 is the device explicitly
// rejecting the request, not a garbled response. probe maps
// MalformedResponseError to StateErrored on the grounds that bytes
// arrived and so the device is present but broken; folding every non-2xx
// HTTP status into that same error type would make an auth rejection
// report as "present but broken" too. These must come back as a
// distinct, status-naming error instead.
func TestGetPrinterAttributesNonUpgradeFailureStatusIsNotMalformed(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(srv.Close)

			uri := "ipp://" + srv.Listener.Addr().String() + "/ipp/print"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, _, _, err := GetPrinterAttributes(ctx, uri, []string{"all"})
			if err == nil {
				t.Fatalf("GetPrinterAttributes against an http %d response returned a nil error", status)
			}

			var malformed *MalformedResponseError
			if errors.As(err, &malformed) {
				t.Fatalf("http %d classified as MalformedResponseError (%v); probe would report this as a reachable-but-broken device, which is wrong for an explicit rejection", status, err)
			}

			var httpErr *HTTPStatusError
			if !errors.As(err, &httpErr) {
				t.Fatalf("err = %v (%T); want a *HTTPStatusError naming the status", err, err)
			}
			if httpErr.StatusCode != status {
				t.Fatalf("HTTPStatusError.StatusCode = %d, want %d", httpErr.StatusCode, status)
			}
		})
	}
}

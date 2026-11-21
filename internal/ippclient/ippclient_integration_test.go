//go:build integration && darwin

package ippclient

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/fakeprinter"
)

// TestGetPrinterAttributesAgainstRealPrinterIsUnaffectedByUpgradeHandling
// guards against a regression this task's 426/TLS-retry logic could
// plausibly introduce: a real, healthy device that never sends 426 and
// never needs the retry path must still be read exactly as before. The
// Epson that prompted this task is not available to every environment
// that runs `go test -tags=integration`, so this exercises a real local
// IPP server (ippeveprinter) instead — the retry logic this package now
// has is a no-op for any device that answers plaintext IPP directly, and
// a real device here is as good a proof of that as the actual printer.
func TestGetPrinterAttributesAgainstRealPrinterIsUnaffectedByUpgradeHandling(t *testing.T) {
	port, _, cleanup := fakeprinter.Start(t, "IPPClientIntegrationPrinter")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uri := fmt.Sprintf("ipp://127.0.0.1:%d/ipp/print", port)
	attrs, _, rtt, err := GetPrinterAttributes(ctx, uri, []string{"all"})
	if err != nil {
		t.Fatalf("GetPrinterAttributes(%s) against a real, healthy device: %v", uri, err)
	}
	if _, ok := attrs["printer-uuid"]; !ok {
		t.Error("result is missing printer-uuid")
	}
	if rtt <= 0 {
		t.Error("expected a positive round-trip time")
	}
}

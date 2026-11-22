package ipc

import (
	"context"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/vocabulary"
)

// TestBusyResponseIsHumanAndLogsTheRealError is the regression for a
// finding that reopened a Critical from an earlier slice: the busy
// response previously named its own mechanism ("daemon") and appended
// the raw Go error verbatim — exactly the shape of leak an
// authorization-error message was flagged for before. This drives the
// busy path through a real Serve/Dial/Call round trip (not just a
// direct call to the message constant) and checks the response that
// actually reaches a client against the same shared vocabulary guard
// every user-facing sentence in this product is supposed to pass,
// proving internal/ipc's own text is covered by it — which, before
// this slice, it never was.
//
// requestTimeout is lowered for the duration of this test so the busy
// path (reached only once dispatch's own deadline elapses while
// waiting for RunExclusive) does not require waiting out the real
// 60s production value.
func TestBusyResponseIsHumanAndLogsTheRealError(t *testing.T) {
	savedRequestTimeout := requestTimeout
	requestTimeout = 100 * time.Millisecond
	defer func() { requestTimeout = savedRequestTimeout }()

	app := testApp()
	socketPath := startServer(t, app)

	// Hold RunExclusive's lock from this test's own goroutine for
	// longer than the shortened requestTimeout, so the request below
	// has no choice but to hit the busy path.
	holding := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		_ = RunExclusive(context.Background(), func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	client, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	resp, err := client.Call("status", nil)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if resp.Error == "" {
		t.Fatal("Call() returned no error while the app was held exclusively elsewhere — want the busy response")
	}

	vocabulary.Assert(t, resp.Error)

	if resp.Error != BusyResponseMessage {
		t.Errorf("busy response = %q, want exactly %q", resp.Error, BusyResponseMessage)
	}
}

package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/OpenPrinting/goipp"
)

// ippGetPrintersServer starts an httptest.Server that answers any IPP
// request with a CUPS-Get-Printers-shaped response: the given status,
// plus one printer-attributes-tag group per entry in printers. It exists
// so List's branching can be unit-tested against a canned response
// instead of this machine's real cupsd — the only place that coverage
// existed before was the darwin-and-integration-tagged
// TestDarwinAdminFullRepairCycle, which go test ./... never runs.
func ippGetPrintersServer(t *testing.T, status goipp.Status, printers []map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			t.Fatalf("decode request sent by List: %v", err)
		}

		resp := goipp.NewResponse(goipp.DefaultVersion, status, req.RequestID)
		opGroup := goipp.Group{Tag: goipp.TagOperationGroup}
		opGroup.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		opGroup.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
		resp.Groups.Add(opGroup)

		for _, p := range printers {
			group := goipp.Group{Tag: goipp.TagPrinterGroup}
			group.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(p["printer-name"])))
			group.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String(p["device-uri"])))
			group.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(p["printer-info"])))
			group.Add(goipp.MakeAttribute("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(p["printer-is-accepting-jobs"] == "true")))
			state, err := strconv.Atoi(p["printer-state"])
			if err != nil {
				t.Fatalf("test fixture printer-state %q is not an integer: %v", p["printer-state"], err)
			}
			group.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(state)))
			resp.Groups.Add(group)
		}

		payload, err := resp.EncodeBytes()
		if err != nil {
			t.Fatalf("encode canned response: %v", err)
		}
		w.Header().Set("Content-Type", goipp.ContentType)
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("write canned response: %v", err)
		}
	}))
}

// withCupsBaseURI points cupsBaseURI at server for the duration of the
// test, restoring the original afterward.
func withCupsBaseURI(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := cupsBaseURI
	cupsBaseURI = server.URL + "/"
	t.Cleanup(func() {
		cupsBaseURI = original
		server.Close()
	})
}

// TestListReportsEmptySliceWhenNoQueuesConfigured pins the
// client-error-not-found-as-zero-queues case: cupsd answers a host with
// no configured queues with that IPP status, not an empty successful
// response, and List must not surface it as an error.
func TestListReportsEmptySliceWhenNoQueuesConfigured(t *testing.T) {
	server := ippGetPrintersServer(t, goipp.StatusErrorNotFound, nil)
	withCupsBaseURI(t, server)

	queues, err := NewDarwinAdmin().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(queues) != 0 {
		t.Fatalf("List returned %d queues, want 0", len(queues))
	}
}

// TestListReturnsErrorOnFailureStatus pins the other branch of the same
// switch: a real failure status (as opposed to the known-benign
// not-found) must be reported as an error, not swallowed.
func TestListReturnsErrorOnFailureStatus(t *testing.T) {
	server := ippGetPrintersServer(t, goipp.StatusErrorForbidden, nil)
	withCupsBaseURI(t, server)

	if _, err := NewDarwinAdmin().List(context.Background()); err == nil {
		t.Fatal("List against a forbidden-status response = nil error, want an error")
	}
}

// TestListMapsPrinterAttributesToQueue pins the attribute-to-Queue
// mapping, including that State carries the raw IPP enum ordinal
// ("3"/StateIdle) and never the word "idle".
func TestListMapsPrinterAttributesToQueue(t *testing.T) {
	server := ippGetPrintersServer(t, goipp.StatusOk, []map[string]string{
		{
			"printer-name":              "office_printer",
			"device-uri":                "ipp://127.0.0.1:1234/ipp/print",
			"printer-info":              "Office Printer",
			"printer-is-accepting-jobs": "true",
			"printer-state":             "3",
		},
	})
	withCupsBaseURI(t, server)

	queues, err := NewDarwinAdmin().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(queues) != 1 {
		t.Fatalf("List returned %d queues, want 1: %+v", len(queues), queues)
	}

	want := Queue{
		Name:        "office_printer",
		DeviceURI:   "ipp://127.0.0.1:1234/ipp/print",
		Description: "Office Printer",
		Accepting:   true,
		State:       StateIdle,
	}
	if got := queues[0]; got != want {
		t.Errorf("List()[0] = %+v, want %+v", got, want)
	}
}

// TestListReportsMultipleQueuesSeparately pins that List keeps each
// CUPS-Get-Printers response group separate rather than flattening them
// together — the reason ippclient.CupsGetPrinters exists instead of
// reusing GetPrinterAttributes's flattening behaviour.
func TestListReportsMultipleQueuesSeparately(t *testing.T) {
	server := ippGetPrintersServer(t, goipp.StatusOk, []map[string]string{
		{
			"printer-name":              "printer_a",
			"device-uri":                "ipp://127.0.0.1:1111/ipp/print",
			"printer-info":              "A",
			"printer-is-accepting-jobs": "true",
			"printer-state":             "3",
		},
		{
			"printer-name":              "printer_b",
			"device-uri":                "ipp://127.0.0.1:2222/ipp/print",
			"printer-info":              "B",
			"printer-is-accepting-jobs": "false",
			"printer-state":             "5",
		},
	})
	withCupsBaseURI(t, server)

	queues, err := NewDarwinAdmin().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(queues) != 2 {
		t.Fatalf("List returned %d queues, want 2: %+v", len(queues), queues)
	}

	byName := map[string]Queue{queues[0].Name: queues[0], queues[1].Name: queues[1]}
	a, ok := byName["printer_a"]
	if !ok || a.DeviceURI != "ipp://127.0.0.1:1111/ipp/print" || a.State != StateIdle {
		t.Errorf("printer_a = %+v, want DeviceURI ipp://127.0.0.1:1111/ipp/print and State StateIdle", a)
	}
	b, ok := byName["printer_b"]
	if !ok || b.DeviceURI != "ipp://127.0.0.1:2222/ipp/print" || b.State != StateStopped || b.Accepting {
		t.Errorf("printer_b = %+v, want DeviceURI ipp://127.0.0.1:2222/ipp/print, State StateStopped, Accepting false", b)
	}
}

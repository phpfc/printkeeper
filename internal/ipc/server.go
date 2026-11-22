// Package ipc is the one door into a running printkeeper daemon: a Unix
// domain socket carrying newline-delimited JSON requests. It exists so
// a `printkeeper` command, run by a person, can ask the long-running
// daemon for what it already knows (or have it act) instead of
// repeating the daemon's own network work from a cold start every time.
//
// The socket is an authority boundary: anything that can write to it
// can ask the daemon to modify printer configuration (a "heal" request
// repoints a real CUPS queue). It lives under the user's own
// Application Support directory, and Serve creates it with permissions
// restricted to its owner (0600) — that permission is the whole of this
// package's access control, asserted explicitly in Serve, not merely
// documented.
package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/phpfc/printkeeper/internal/cli"
)

// Request is one newline-delimited JSON request read from the socket.
// Method is one of "status", "heal", or "adopt". ID is echoed back on
// the matching Response so a client that pipelines more than one
// request at a time can match replies to requests.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	ID     int             `json:"id"`
}

// Response is the daemon's reply to one Request, written back
// newline-delimited on the same connection. Error is non-empty exactly
// when the request could not be fulfilled — a malformed line, an
// unknown method, or the underlying App method itself failing — and
// Result is only ever populated alongside an empty Error.
type Response struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// outputResult is what "status" and "heal" carry back in Response.Result:
// exactly the text App.Doctor or App.Heal would otherwise have written
// to a terminal, captured instead of printed, since a daemon has no
// terminal of its own to print to.
type outputResult struct {
	Output string `json:"output"`
}

// requestTimeout bounds how long any single request is allowed to run
// against app — generous enough for a Heal sweep across a handful of
// printers, short enough that one hung request cannot wedge the
// connection serving it forever. A var, not a const, so a test can
// lower it rather than waiting out the real production value to
// exercise the busy path below.
var requestTimeout = 60 * time.Second

// BusyResponseMessage is what a request gets back when it could not
// acquire exclusive access to app before its own deadline — most
// commonly because a heal cycle is already running, which is not a
// rare path: it happens whenever the daemon's own heartbeat sweep
// overlaps a person checking on their printer, which is to say,
// whenever they are worried enough to check. It must read as a plain
// sentence, not as a technical sentinel: no internal mechanism named
// ("daemon" is domain vocabulary — see internal/vocabulary — in
// exactly the way "CUPS" or "IPP" already were), and no error value
// appended, which was the same shape of leak that made an earlier
// slice's authorization message a Critical finding. The actual error
// RunExclusive returned is logged (see dispatch below), not shown.
//
// Exported so cmd/printkeeper's own tests can assert against the real
// constant instead of restating it as a second string literal that
// could silently drift out of sync with this one.
const BusyResponseMessage = "The background service is checking your printers right now. Try again in a moment."

// Serve listens on socketPath for newline-delimited JSON requests and
// answers them against app, until ctx is cancelled or the listener
// fails. socketPath's parent directory is assumed to already exist and
// be owned by the current user (cmd/printkeeperd creates it alongside
// the adopted-printer store, under the same Application Support
// directory) — Serve's own job is only the socket file itself.
//
// A malformed line never kills the server: a daemon that exits on one
// bad request is worse than no daemon, because the user's printers
// would then silently stop being watched, with nothing to say so. Every
// connection is handled in its own goroutine, every line within a
// connection gets its own recovered dispatch, and a bad line only ever
// produces an error Response — the listener keeps accepting, and the
// connection that sent it keeps being read.
func Serve(ctx context.Context, socketPath string, app *cli.App) error {
	// A daemon that crashed, or was killed, last time can leave its
	// socket file behind; a fresh Listen on the same path would
	// otherwise fail with "address already in use" even though nothing
	// is actually listening.
	if err := os.RemoveAll(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket %s: %w", socketPath, err)
	}

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}

	// net.Listen("unix", ...) applies the process umask to the socket
	// file's permissions, which is not the same thing as a promise of
	// 0600 under every umask this might ever run with — so this pins
	// the permission explicitly, as a real check against what actually
	// landed on disk, rather than trusting the umask to have done it.
	if err := os.Chmod(socketPath, 0600); err != nil {
		l.Close()
		return fmt.Errorf("set permissions on %s: %w", socketPath, err)
	}

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	srv := &server{app: app}
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("accept on %s: %w", socketPath, err)
			}
		}
		go srv.handle(conn)
	}
}

// appSem is a capacity-1 channel used as a mutex whose acquisition can
// be bounded by a context: sending into it succeeds only when it is
// not already full (acquire), and receiving from it frees that
// capacity for the next sender (release). This serialises every call
// into a *cli.App shared between this package's own request dispatch
// and anything else touching the same App concurrently — App's
// methods write to a shared Out field, which this package temporarily
// redirects per request to capture output, not safe for two calls to
// do at once — and cmd/printkeeperd's own background probe loop calls
// App.Heal directly from a second goroutine that Serve's dispatch
// knows nothing about. A single package-level semaphore, rather than
// one private to each server value, is what makes RunExclusive below
// actually exclusive with Serve's own dispatch rather than merely with
// other requests on the same connection: a daemon process creates
// exactly one *cli.App and calls Serve on it exactly once, so one
// semaphore for the whole package is the same thing as one per App in
// every configuration this project actually runs.
//
// A plain sync.Mutex cannot be bounded by a context — Lock blocks
// until it succeeds, full stop — which was a reviewer's finding:
// dispatch derives a context with its own requestTimeout deadline, but
// that deadline meant nothing once execution reached the mutex, so a
// slow heal cycle holding it could leave a request hanging regardless
// of any deadline the client itself set. A channel's send can select
// against ctx.Done(), which is what makes RunExclusive below able to
// give up cleanly instead of blocking forever.
var appSem = make(chan struct{}, 1)

// RunExclusive runs fn with the same exclusion Serve's own request
// dispatch uses, bounded by ctx: if exclusive access cannot be
// acquired before ctx is done, RunExclusive returns ctx.Err() without
// ever calling fn, rather than blocking indefinitely. A caller sharing
// one *cli.App with a running Serve from another goroutine —
// cmd/printkeeperd's background probe loop, which calls App.Heal and
// reads App.Store/App.Queues on its own schedule, independent of
// anything arriving over the socket — never races a socket request
// for access to that same App's shared, not-concurrency-safe state
// (notably Out).
func RunExclusive(ctx context.Context, fn func() error) error {
	select {
	case appSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-appSem }()
	return fn()
}

// server dispatches requests against one *cli.App.
type server struct {
	app *cli.App
}

// handle reads newline-delimited requests from conn until it is closed
// or a read fails, dispatching each line independently so one malformed
// or slow line never affects any other connection.
func (s *server) handle(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		resp := s.dispatch(line)

		data, err := json.Marshal(resp)
		if err != nil {
			// Marshalling our own Response failed — nothing a client
			// sent caused this, and there is nothing useful left to
			// say back on this connection, but it must not take the
			// listener down with it.
			continue
		}
		data = append(data, '\n')
		if _, err := conn.Write(data); err != nil {
			return
		}
	}
}

// dispatch decodes and answers one request line. A panic anywhere
// inside a handler — this package's own bug, not something a client
// can trigger on purpose — is recovered here rather than allowed to
// crash the goroutine (and, since nothing above catches it on a
// detached goroutine, the whole daemon process): the same "one bad
// request must not kill the server" promise Serve's doc comment makes,
// extended to failures Serve itself cannot anticipate.
func (s *server) dispatch(line []byte) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			resp = Response{Error: fmt.Sprintf("internal error: %v", r)}
		}
	}()

	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return Response{Error: fmt.Sprintf("malformed request: %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var out Response
	err := RunExclusive(ctx, func() error {
		switch req.Method {
		case "status":
			out = s.runCapturingOutput(ctx, req.ID, s.app.Doctor)
		case "heal":
			out = s.runCapturingOutput(ctx, req.ID, s.app.Heal)
		case "adopt":
			// Adopt is the one command that reads from a terminal —
			// spec's own description of it. A one-shot JSON request
			// carries no stream for that back-and-forth, so it is
			// never delegated to the daemon; the caller always runs it
			// in-process instead.
			out = Response{ID: req.ID, Error: `adopt needs an interactive terminal; run "printkeeper adopt" directly`}
		default:
			out = Response{ID: req.ID, Error: fmt.Sprintf("unknown method %q", req.Method)}
		}
		return nil
	})
	if err != nil {
		// Could not even acquire exclusive access to app before this
		// request's own deadline — most likely a heal cycle holding
		// it unusually long. This is "busy", not "broken": the
		// client's own longer per-method deadline (client.go) is what
		// turns this into a clean error instead of a hang on the
		// client side. The real error is diagnostic, for whoever reads
		// the daemon's own log — see BusyResponseMessage's own doc
		// comment for why it never reaches the Response itself.
		log.Printf("ipc: could not acquire exclusive access to app before this request's deadline: %v", err)
		return Response{ID: req.ID, Error: BusyResponseMessage}
	}
	return out
}

// runCapturingOutput runs fn (App.Doctor or App.Heal) with app.Out
// swapped to a buffer for the duration of the call, so the sentences
// those methods write for a human at a terminal are captured into the
// Response instead — a daemon has no terminal of its own.
func (s *server) runCapturingOutput(ctx context.Context, id int, fn func(context.Context) error) Response {
	var buf bytes.Buffer
	original := s.app.Out
	s.app.Out = &buf
	defer func() { s.app.Out = original }()

	if err := fn(ctx); err != nil {
		return Response{ID: id, Error: err.Error()}
	}

	result, err := json.Marshal(outputResult{Output: buf.String()})
	if err != nil {
		return Response{ID: id, Error: fmt.Sprintf("encode result: %v", err)}
	}
	return Response{ID: id, Result: result}
}

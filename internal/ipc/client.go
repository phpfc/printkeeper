package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// statusCallTimeout bounds a "status" request. "status" and "heal"
// have nothing in common in how long they may legitimately take, so
// one shared budget for both was either too long for "status" (a
// human watching a terminal for the production requestTimeout's worth
// of silence has already concluded the tool is broken, long before
// any fallback saves them) or too short for "heal" (see
// healCallTimeout below) — there is no single number that is right
// for both.
//
// The budget here is sized against what "status" actually costs
// today, not an idealised one: the "status" IPC method answers with
// App.Doctor, which performs a fresh IPP probe per adopted printer
// (internal/cli/doctor.go's doctorProbeTimeout, 10s each, run
// sequentially) rather than returning state the daemon already holds
// in memory. 20s comfortably covers one adopted printer's full
// absent-printer probe (the single most common real case this
// project exists for — checking on a printer that is not answering)
// with slack to spare, while staying well under both the server's own
// 60s requestTimeout and healCallTimeout below. It does not comfortably
// cover several adopted printers all absent at once; see this task's
// report for why the better long-term fix is serving "status" from
// the daemon's own already-probed state instead of re-probing on
// every request, which would make a genuinely short, "in-memory"
// budget both correct and safe — tracked for the whole-branch review,
// not fixed here.
var statusCallTimeout = 20 * time.Second

// healCallTimeout bounds a "heal" request, which legitimately takes
// real time: App.Heal browses the network (up to 5s) and then probes
// every adopted printer in turn. It is set deliberately *above* the
// server's own requestTimeout (server.go, 60s) — not because this
// client should ever wait that long in the ordinary case, but so the
// server's own internal deadline fires first and produces a real
// answer (success, or a clean error) before this client-side deadline
// is ever reached. Reaching it at all means the daemon is wedged badly
// enough to not even honour its own internal timeout — this is a
// backstop for that failure, not the expected path.
var healCallTimeout = requestTimeout + 10*time.Second

// callTimeoutFor returns the deadline Call should use for method. A
// method this client does not itself call directly (there are only
// two today) gets the longer, safer budget rather than risking an
// unrecognised slow operation being cut off early.
func callTimeoutFor(method string) time.Duration {
	if method == "status" {
		return statusCallTimeout
	}
	return healCallTimeout
}

// Client is a connection to a running daemon's socket, opened by Dial.
type Client struct {
	conn   net.Conn
	reader *bufio.Reader

	mu     sync.Mutex
	nextID int
}

// TransportError wraps a failure to complete a request/response round
// trip at all — a write that failed, a read that hit callTimeout, a
// response that did not even decode — as distinct from an error *from*
// the daemon itself (a non-empty Response.Error, meaning the daemon
// did answer, just not successfully). cmd/printkeeper uses this
// distinction to decide whether "the daemon didn't answer in time or
// at all" should fall back to running the command in-process (yes —
// indistinguishable, from the user's point of view, from no daemon
// running at all) versus "the daemon answered with a real failure",
// which is reported rather than silently retried in-process against
// what is likely the same underlying problem.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Dial connects to the daemon listening on socketPath. A failure to
// dial — most commonly because no daemon is running, so the socket
// does not exist — is an ordinary error, not a special sentinel: the
// documented way to fall back to running a command in-process is to
// attempt Dial and act on any error at all, not to distinguish why it
// failed.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, reader: bufio.NewReader(conn)}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Call sends one request for method with params (marshalled as JSON;
// nil for none) and waits for its matching response. Call is safe for
// concurrent use; requests from the same Client are serialised onto one
// connection and matched to their response by ID.
func (c *Client) Call(method string, params any) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID

	var rawParams json.RawMessage
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return Response{}, fmt.Errorf("encode params: %w", err)
		}
		rawParams = p
	}

	data, err := json.Marshal(Request{Method: method, Params: rawParams, ID: id})
	if err != nil {
		return Response{}, fmt.Errorf("encode request: %w", err)
	}
	data = append(data, '\n')

	// The deadline covers both the write and the read below: a slow or
	// wedged daemon on either side of that round trip must not hang
	// this call forever. net.Conn.SetDeadline is an absolute time, so
	// setting it once here — rather than once per Write/Read — is
	// exactly equivalent and simpler.
	if err := c.conn.SetDeadline(time.Now().Add(callTimeoutFor(method))); err != nil {
		return Response{}, &TransportError{Err: fmt.Errorf("set deadline: %w", err)}
	}

	if _, err := c.conn.Write(data); err != nil {
		return Response{}, &TransportError{Err: fmt.Errorf("send request: %w", err)}
	}

	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return Response{}, &TransportError{Err: fmt.Errorf("read response: %w", err)}
	}

	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, &TransportError{Err: fmt.Errorf("decode response: %w", err)}
	}
	return resp, nil
}

// Status asks the daemon for its current Doctor-style report: the same
// text `printkeeper doctor` would print running in-process, captured
// instead from the daemon's own already-warm view of every adopted
// printer.
func (c *Client) Status() (string, error) {
	return c.outputCall("status")
}

// Heal asks the daemon to run one repair sweep now and reports what, if
// anything, it had to say about it — the same text `printkeeper heal`
// would print running in-process.
func (c *Client) Heal() (string, error) {
	return c.outputCall("heal")
}

// outputCall drives the shared request/response shape "status" and
// "heal" both use: a plain string of human-readable output, or an
// error.
func (c *Client) outputCall(method string) (string, error) {
	resp, err := c.Call(method, nil)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("%s", resp.Error)
	}

	var result outputResult
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return "", fmt.Errorf("decode result: %w", err)
		}
	}
	return result.Output, nil
}

// Command printkeeperd is the background half of printkeeper: the
// process that notices a protected printer went missing without
// anyone having to remember to run anything. CUPS itself cannot tell —
// a spike confirmed it reports a dead printer "idle" and "accepting
// requests" seconds after it was unplugged, with no event and no flag
// to subscribe to — so something has to ask the device directly, on
// its own schedule, with nobody watching. That is this binary's whole
// job. `printkeeper` (cmd/printkeeper) is the tool a person runs by
// hand; it prefers talking to this daemon over its socket whenever one
// is running, and runs the same logic in-process when one is not.
//
// A daemon with a visible footprint is a failed daemon — measurable
// battery drain or constant wakeups get it uninstalled, same as
// printing to the wrong printer gets the whole tool uninstalled. So
// probing is event-driven first (on daemon start, on a network change,
// on wake from sleep) with a slow heartbeat covering silent failures
// while the network looks stable; see internal/daemon.NextProbeAt for
// the schedule itself. A successful repair stays exactly as silent
// here as it is in the CLI — there is no terminal to interrupt, so any
// notification this process raised would just be an interruption with
// nobody who asked for it. What it finds and does is written to its
// own log file, never pushed at the user.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/phpfc/printkeeper/internal/cli"
	pkdaemon "github.com/phpfc/printkeeper/internal/daemon"
	"github.com/phpfc/printkeeper/internal/ipc"
	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/resolve"
	"github.com/phpfc/printkeeper/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "printkeeperd:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := appSupportDir()
	if err != nil {
		return fmt.Errorf("find where to keep daemon state: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	logFile, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()

	disco := newDiscoverer()
	app := &cli.App{
		Disco:      disco,
		Queues:     newAdmin(),
		Store:      store.NewFileStore(filepath.Join(dir, "adopted.json")),
		Keyer:      newNetworkKeyer(),
		Out:        logFile,
		Strategies: resolve.DefaultStrategies(disco),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// launchd's "launchctl bootout" sends SIGTERM; a plain Ctrl-C sends
	// SIGINT when run by hand. Either must shut down cleanly — closing
	// the socket and leaving no stale file behind for the next launch
	// to trip over on "address already in use" — rather than being
	// killed mid-write.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cancel()
	}()

	socketPath := filepath.Join(dir, "sock")

	// cancel() here is what makes a Serve failure (most commonly: the
	// socket could not even be bound) bring the whole process down
	// promptly, rather than leaving runProbeLoop running forever below
	// with no socket and nothing watching for one ever coming back. On
	// the ordinary clean-shutdown path, ctx is already cancelled by the
	// signal handler by the time Serve returns, so this second cancel
	// is a harmless no-op.
	serveErr := make(chan error, 1)
	go func() {
		err := ipc.Serve(ctx, socketPath, app)
		serveErr <- err
		cancel()
	}()

	runProbeLoop(ctx, app, logFile)

	return <-serveErr
}

// appSupportDir is where the daemon keeps everything it owns: the
// adopted-printer store, the IPC socket, and its own log — the same
// per-user Application Support directory cmd/printkeeper's store
// already lives under, so both binaries agree on one location without
// either importing the other's unexported path logic.
func appSupportDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the current user's home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "printkeeper"), nil
}

// probeTimeout bounds each adopted printer's direct IPP round trip
// within one cycle of the loop below.
const probeTimeout = 10 * time.Second

// debounceWindow is how long runProbeLoop waits after the first
// network-change signal before acting on it, draining any further
// signals that arrive in the meantime. A routing change is usually
// several kernel messages for one real event (an interface going down
// touches its addresses and routes separately, for instance), so
// without this a single real event could trigger several back-to-back
// probe cycles instead of one.
const debounceWindow = 500 * time.Millisecond

// runProbeLoop is the daemon's event loop: it probes every adopted
// printer, and repairs what the ladder is confident enough to repair,
// whenever spec §6.6 says it should — immediately on entry (daemon
// start), immediately again whenever the kernel reports a routing
// table change (see netwatch_darwin.go), and otherwise no more often
// than internal/daemon.NextProbeAt's own schedule calls for.
//
// There is no separate timer-based "did we just wake from sleep?"
// check here, unlike an earlier version of this loop that polled
// probe.HasNetwork() every few seconds and compared consecutive ticks
// for a large gap. Waking from sleep on a real machine reassociates
// Wi-Fi, renews a DHCP lease, or reconfigures an interface almost
// every time, which the routing-change watcher already observes
// directly; the heartbeat remains the backstop for the rare case that
// a wake produces no routing message at all. Trading a dedicated wake
// heuristic for that backstop is what let the steady-state network
// check become an actual kernel block instead of a 5-second poll — a
// poll this daemon's own anti-goal (spec: "measurable battery drain,
// constant wakeups") explicitly rules out paying for twice.
func runProbeLoop(ctx context.Context, app *cli.App, logger io.Writer) {
	changed := make(chan struct{}, 1)

	watcher, err := newNetworkChangeWatcher()
	if err != nil {
		// Honest degradation, not a silent one: without a watcher this
		// loop still runs correctly on NextProbeAt's own heartbeat and
		// backoff, just slower to notice a network change than it
		// otherwise would be.
		logf(logger, "network-change watcher unavailable, falling back to the heartbeat alone: %v", err)
	} else {
		go func() {
			defer watcher.Close()
			for {
				if waitErr := watcher.Wait(); waitErr != nil {
					// Either a real socket failure, or Close() was
					// called below in response to ctx.Done() — either
					// way, this goroutine's job is done.
					return
				}
				select {
				case changed <- struct{}{}:
				default:
					// A change notification is already pending; see
					// debounceWindow above for why a burst collapses
					// into one signal rather than queuing several.
				}
			}
		}()
		go func() {
			<-ctx.Done()
			watcher.Close() // unblocks the goroutine's Wait() above.
		}()
	}

	nextProbeAt := time.Now() // spec §6.6: probe on daemon start, unconditionally.

	for {
		wait := time.Until(nextProbeAt)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-changed:
			timer.Stop()
			drainBurst(changed, debounceWindow)
		}

		nextProbeAt = time.Now().Add(healCycle(ctx, app, logger))
	}
}

// drainBurst discards anything else arriving on changed for window,
// coalescing a burst of routing messages from one real network event
// into the single re-probe runProbeLoop is about to do anyway.
func drainBurst(changed <-chan struct{}, window time.Duration) {
	deadline := time.NewTimer(window)
	defer deadline.Stop()
	for {
		select {
		case <-changed:
		case <-deadline.C:
			return
		}
	}
}

// healCycle runs one full probe-and-repair pass and returns how long to
// wait before the next one. It probes each adopted printer directly
// first — not because repair.Sweep (called via app.Heal below) does
// not also probe every one of them itself, but because Sweep's result
// is a repair.Decision, which does not carry the probe.State NextProbeAt
// needs to schedule the next pass; this is the deliberate cost of
// keeping that scheduling decision in internal/daemon as a small, pure
// function rather than widening repair.Decision's shape (an interface
// three earlier tasks already built against) just to carry one more
// field for this task's benefit. Each adopted printer is probed twice
// per cycle as a result — negligible at the cadence NextProbeAt itself
// enforces (minutes, not seconds).
//
// The whole pass runs inside ipc.RunExclusive: this loop and
// ipc.Serve's own request dispatch both read and write the same
// *cli.App (app.Store, app.Queues, and — through app.Heal — app.Out)
// from different goroutines, and RunExclusive is the lock they share
// to keep that safe. Found in self-review, not by a test: nothing
// here would have failed loudly, just occasionally raced.
func healCycle(ctx context.Context, app *cli.App, logger io.Writer) time.Duration {
	next := pkdaemon.Heartbeat

	_ = ipc.RunExclusive(ctx, func() error {
		adopted, err := app.Store.Load()
		if err != nil {
			logf(logger, "could not load adopted printers: %v", err)
			return nil
		}
		if len(adopted) == 0 {
			return nil
		}

		queues, err := app.Queues.List(ctx)
		if err != nil {
			logf(logger, "could not list queues: %v", err)
			return nil
		}

		battery := onBattery()

		haveSchedule := false
		for _, a := range adopted {
			q, ok := findQueue(queues, a.QueueName)
			if !ok {
				continue
			}

			result := probe.Probe(ctx, q.DeviceURI, probeTimeout)
			interval := pkdaemon.NextProbeAt(mostRecentSeen(a), result.State, battery)
			if interval == 0 {
				// StateUnknown: no probe scheduled for this printer;
				// the network-change check above is what will wake
				// this loop up again, not a timer.
				continue
			}
			if !haveSchedule || interval < next {
				next = interval
				haveSchedule = true
			}
		}

		if err := app.Heal(ctx); err != nil {
			logf(logger, "heal cycle failed: %v", err)
		}
		return nil
	})

	return next
}

// findQueue looks up the queue named name among queues, if any.
func findQueue(queues []queue.Queue, name string) (queue.Queue, bool) {
	for _, q := range queues {
		if q.Name == name {
			return q, true
		}
	}
	return queue.Queue{}, false
}

// mostRecentSeen returns the latest Seen.At across every network a has
// ever been seen on, or the zero time if it has never been seen at
// all — in which case NextProbeAt's own backoff treats it as maximally
// overdue, which is the safe default for "we don't actually know".
func mostRecentSeen(a store.Adopted) time.Time {
	var most time.Time
	for _, seen := range a.LastSeen {
		if seen.At.After(most) {
			most = seen.At
		}
	}
	return most
}

// logf writes one timestamped line to the daemon's own log. This is
// strictly diagnostic — nothing here is a user-facing notification;
// spec's rule that success stays silent and failure is only visible to
// someone who goes looking applies just as much to a daemon with no
// terminal as it does to the CLI, so this never does anything that
// would interrupt anyone.
func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
}

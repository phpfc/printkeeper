package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/phpfc/printkeeper/internal/probe"
	"github.com/phpfc/printkeeper/internal/queue"
)

// doctorProbeTimeout bounds each printer's direct IPP round trip —
// generous enough for a slow device, short enough that one unreachable
// printer cannot stall a Doctor run checking several.
const doctorProbeTimeout = 10 * time.Second

// Doctor checks every adopted printer's current address directly over
// the network and reports its health: working, present but unhealthy
// (out of paper, cover open, ...), or not responding at all. Unlike
// Heal, Doctor never repairs anything — it only reports what it found.
// A deliberate "how is my printer doing" check is exactly the moment
// spec §2 goal 3's silence would be the wrong choice: the user asked,
// so they get an answer, every time, not just when something is wrong.
func (a *App) Doctor(ctx context.Context) error {
	adopted, err := a.Store.Load()
	if err != nil {
		return fmt.Errorf("load adopted printers: %w", err)
	}
	if len(adopted) == 0 {
		fmt.Fprintln(a.Out, "No printers are being protected yet. Run \"printkeeper adopt\" to choose one.")
		return nil
	}

	queues, err := a.Queues.List(ctx)
	if err != nil {
		return errors.New(queueErrorMessage(err))
	}

	for _, ad := range adopted {
		name := a.nameForQueue(adopted, ad.QueueName)

		q, ok := findQueueByName(queues, ad.QueueName)
		if !ok {
			fmt.Fprintf(a.Out, "%s is no longer set up on this computer.\n", name)
			continue
		}

		result := probe.Probe(ctx, q.DeviceURI, doctorProbeTimeout)
		fmt.Fprintln(a.Out, DescribeProbe(name, result))
	}
	return nil
}

// findQueueByName looks up the queue named name among queues, if any.
func findQueueByName(queues []queue.Queue, name string) (queue.Queue, bool) {
	for _, q := range queues {
		if q.Name == name {
			return q, true
		}
	}
	return queue.Queue{}, false
}

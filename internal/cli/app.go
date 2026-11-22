package cli

import (
	"io"

	"github.com/phpfc/printkeeper/internal/discovery"
	"github.com/phpfc/printkeeper/internal/queue"
	"github.com/phpfc/printkeeper/internal/resolve"
	"github.com/phpfc/printkeeper/internal/store"
)

// App wires every port and core package Tasks 1-7 produced into the
// three commands a person runs: Adopt, Doctor, and Heal. Every field is
// an interface (or a slice of one), so a test can hand it fakes instead
// of talking to a real network, a real cupsd, or a real disk.
//
// Strategies is not in the brief's original struct literal — the
// controller ruling for this task adds it, because without it the CLI
// has no way to call resolve.Resolve at all, and explaining what was
// tried when a repair is impossible is one of the design's four goals
// (spec §2 goal 4). It is used only to build the human-readable "what
// we tried" sentence for ActionReport; Heal's own repair decisions still
// come from repair.Sweep, not from Resolve.
type App struct {
	Disco      discovery.Discoverer
	Queues     queue.Admin
	Store      store.Store
	Keyer      store.NetworkKeyer
	Out        io.Writer
	Strategies []resolve.Strategy
}

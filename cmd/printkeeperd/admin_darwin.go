package main

import "github.com/phpfc/printkeeper/internal/queue"

// newAdmin returns the platform's real queue.Admin. On darwin that is
// the local cupsd, administered via lpadmin; see admin_other.go for
// every other platform. Mirrors cmd/printkeeper's own newAdmin — two
// binaries, each wiring the same platform adapters into their own
// *cli.App, rather than one binary importing the other's unexported
// wiring.
func newAdmin() queue.Admin {
	return queue.NewDarwinAdmin()
}

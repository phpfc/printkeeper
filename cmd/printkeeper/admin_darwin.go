package main

import "github.com/phpfc/printkeeper/internal/queue"

// newAdmin returns the platform's real queue.Admin. On darwin that is
// the local cupsd, administered via lpadmin; see admin_other.go for
// every other platform.
func newAdmin() queue.Admin {
	return queue.NewDarwinAdmin()
}

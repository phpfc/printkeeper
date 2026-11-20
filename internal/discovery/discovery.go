// Package discovery finds IPP printers on the network without connecting to
// them: everything a Candidate carries comes from Bonjour/mDNS service
// records (service name, URI, and TXT fields), never from an IPP probe.
package discovery

import (
	"context"
	"time"
)

// Candidate is a printer found by discovery, described entirely by what a
// Bonjour browse reveals before any connection is made to the device.
type Candidate struct {
	ServiceName  string
	URI          string
	UUID         string
	Model        string
	ResourcePath string
	PDLs         []string
	Host         string
	Port         int
}

// Discoverer finds printer Candidates on the network within a timeout.
type Discoverer interface {
	Browse(ctx context.Context, timeout time.Duration) ([]Candidate, error)
}

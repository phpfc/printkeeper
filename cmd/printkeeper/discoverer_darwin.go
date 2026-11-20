package main

import "github.com/phpfc/printkeeper/internal/discovery"

// newDiscoverer returns the platform's real Discoverer. On darwin that
// shells out to ippfind; see discoverer_other.go for every other platform.
func newDiscoverer() discovery.Discoverer {
	return discovery.NewDarwinDiscoverer()
}

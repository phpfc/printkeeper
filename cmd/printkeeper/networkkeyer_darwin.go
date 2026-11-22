package main

import "github.com/phpfc/printkeeper/internal/store"

// newNetworkKeyer returns the platform's real store.NetworkKeyer; see
// networkkeyer_other.go for every other platform.
func newNetworkKeyer() store.NetworkKeyer {
	return store.NewDarwinNetworkKeyer()
}

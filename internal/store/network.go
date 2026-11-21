package store

// NetworkKeyer identifies the network the machine is currently on, as a
// stable string a Store's Adopted.LastSeen map can be keyed by. Two
// calls on the same network should return the same key; calls on
// different networks should (ordinarily) return different keys. There
// is no promise that the key is human-readable or meaningful beyond
// equality — callers compare it, they do not parse it.
type NetworkKeyer interface {
	Key() string
}

// Package store is printkeeper's memory: which printers the user actually
// adopted, and where each was last reachable. It is the only thing that
// distinguishes "this printer is mine" from "this is some printer that
// showed up on the network" — nothing else in printkeeper records a
// choice, only this package.
//
// Address history is kept per network (spec §11.4), not globally: a
// laptop that visits a home network and an office network can have the
// same adopted printer answer at a different address on each, and an
// address learned on one network must never be tried against another.
// NetworkKeyer (network.go) is what tells two networks apart.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/phpfc/printkeeper/internal/identity"
)

// Seen is a single last-known-good sighting of an adopted printer: the
// device URI it answered at, and when that was observed.
type Seen struct {
	URI string
	At  time.Time
}

// Adopted is a printer the user explicitly chose to protect. QueueName
// joins against queue.Queue.Name — it is the CUPS queue printkeeper
// created for this printer. Fingerprint is what later discovery results
// are matched against to recognise this same physical printer again.
// LastSeen records, per network key (as produced by a NetworkKeyer), the
// most recent address this printer was reachable at on that network.
type Adopted struct {
	QueueName   string
	Fingerprint identity.Fingerprint
	AdoptedAt   time.Time
	LastSeen    map[string]Seen
}

// Store persists the set of adopted printers. Load and Save both operate
// on the whole set — there is no partial update — because the file is
// small and this keeps every write atomic with respect to every field,
// including LastSeen's per-network map.
type Store interface {
	// Load returns every adopted printer. A store that has never been
	// written to (a fresh install) returns an empty slice, not an
	// error.
	Load() ([]Adopted, error)

	// Save replaces the entire stored set. It must not leave a
	// truncated or missing file behind if interrupted partway through.
	Save([]Adopted) error
}

// fileStore is a Store backed by a single JSON file on disk.
type fileStore struct {
	path string
}

// NewFileStore returns a Store that persists to path as JSON. The file
// and its parent directory are created on first Save with permissions
// that reflect what this file records — which printers someone uses and
// where those printers live on their network: 0700 on the directory,
// 0600 on the file.
func NewFileStore(path string) Store {
	return fileStore{path: path}
}

// Load reads the stored set from disk. A missing file is the normal
// state for a store nothing has ever been saved to, and is reported as
// an empty slice rather than an error.
func (s fileStore) Load() ([]Adopted, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Adopted{}, nil
		}
		return nil, fmt.Errorf("read store %s: %w", s.path, err)
	}

	var adopted []Adopted
	if err := json.Unmarshal(data, &adopted); err != nil {
		return nil, fmt.Errorf("decode store %s: %w", s.path, err)
	}
	return adopted, nil
}

// Save writes the whole adopted set to disk. It writes to a temporary
// file in the same directory as the target and renames it into place,
// so a crash or a full disk during the write can only ever leave the
// previous version intact or the new version complete — never a
// truncated or empty file. The directory is created with 0700 and the
// final file with 0600 if either does not already exist.
func (s fileStore) Save(adopted []Adopted) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create store directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(adopted, "", "  ")
	if err != nil {
		return fmt.Errorf("encode store: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".adopted-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp store file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once Rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp store file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("set store file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp store file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace store file %s: %w", s.path, err)
	}
	return nil
}

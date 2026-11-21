package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phpfc/printkeeper/internal/identity"
)

// TestPerNetworkHistoryKeepsBothNetworks is the laptop case from spec
// §11.4: a printer adopted at home, where it answers at one URI, is the
// same adopted printer at the office, where it answers at a different
// one. Recording a last-known-good address under each network's key must
// not let one overwrite the other, and reading back under "net-home"
// must return the home URI, not whatever was written most recently.
func TestPerNetworkHistoryKeepsBothNetworks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adopted.json")
	s := NewFileStore(path)

	adoptedAt := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	homeSeenAt := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	officeSeenAt := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

	adopted := Adopted{
		QueueName: "Home_Office_Printer",
		Fingerprint: identity.Fingerprint{
			UUID: "4509a320-00a0-00a0-00a0-000000000001",
		},
		AdoptedAt: adoptedAt,
		LastSeen: map[string]Seen{
			"net-home": {
				URI: "ipp://192.168.1.50:631/ipp/print",
				At:  homeSeenAt,
			},
			"net-office": {
				URI: "ipp://10.0.5.20:631/ipp/print",
				At:  officeSeenAt,
			},
		},
	}

	if err := s.Save([]Adopted{adopted}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("Load() returned %d entries, want 1", len(loaded))
	}

	got := loaded[0]
	if len(got.LastSeen) != 2 {
		t.Fatalf("LastSeen has %d entries, want 2 (home and office both kept): %+v", len(got.LastSeen), got.LastSeen)
	}

	home, ok := got.LastSeen["net-home"]
	if !ok {
		t.Fatalf("LastSeen[%q] missing, want present", "net-home")
	}
	if home.URI != "ipp://192.168.1.50:631/ipp/print" {
		t.Errorf("LastSeen[%q].URI = %q, want the home URI, not the office one or a merge of the two", "net-home", home.URI)
	}
	if !home.At.Equal(homeSeenAt) {
		t.Errorf("LastSeen[%q].At = %v, want %v", "net-home", home.At, homeSeenAt)
	}

	office, ok := got.LastSeen["net-office"]
	if !ok {
		t.Fatalf("LastSeen[%q] missing, want present", "net-office")
	}
	if office.URI != "ipp://10.0.5.20:631/ipp/print" {
		t.Errorf("LastSeen[%q].URI = %q, want the office URI", "net-office", office.URI)
	}
}

// TestLoadWithNoFileReturnsEmpty exercises first run: nothing has ever
// been adopted, so the store file does not exist yet. That is not an
// error; it is the normal state for a fresh install.
func TestLoadWithNoFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "adopted.json")
	s := NewFileStore(path)

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load() on a missing file returned error = %v, want nil", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("Load() on a missing file returned %d entries, want 0", len(loaded))
	}
}

// TestSaveCreatesDirectoryAndFilePermissions pins the permission model
// the brief requires: the directory is 0700 and the file is 0600,
// because this file records what printer hardware someone uses and
// where it lives on their network.
func TestSaveCreatesDirectoryAndFilePermissions(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "printkeeper")
	path := filepath.Join(storeDir, "adopted.json")
	s := NewFileStore(path)

	if err := s.Save([]Adopted{}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	dirInfo, err := os.Stat(storeDir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0700 {
		t.Errorf("directory permissions = %o, want %o", perm, 0700)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0600 {
		t.Errorf("file permissions = %o, want %o", perm, 0600)
	}
}

// TestSaveLeavesNoTempFileBehind checks that Save's write-then-rename
// does not leak its temporary file into the store directory on success,
// which would otherwise be mistaken for a second store file or left to
// accumulate across repeated saves.
func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adopted.json")
	s := NewFileStore(path)

	if err := s.Save([]Adopted{}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := s.Save([]Adopted{}); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("store directory contains %v, want only adopted.json", names)
	}
}

// TestSaveNeverTruncatesOnFailedWrite is the crash-safety case the brief
// calls for: if a previous Save succeeded, a later Save that fails
// partway through (simulated here by making the store directory
// read-only, so the write cannot even begin) must not have destroyed
// the previously valid file. Save writes to a temp file and renames it
// into place, so nothing touches the original until that final rename.
func TestSaveNeverTruncatesOnFailedWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adopted.json")
	s := NewFileStore(path)

	good := []Adopted{{QueueName: "Known_Good"}}
	if err := s.Save(good); err != nil {
		t.Fatalf("initial Save() error = %v", err)
	}

	// Make the directory read-only so Save cannot even create its temp
	// file, simulating a write failure partway through. Nothing about
	// Save's atomic-replace design touches the real path until the
	// final os.Rename, so the previously saved file must survive
	// completely untouched no matter which step failed.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	_ = s.Save([]Adopted{{QueueName: "Should_Not_Land"}})

	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatalf("restore dir perms: %v", err)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load() after failed Save error = %v", err)
	}
	if len(loaded) != 1 || loaded[0].QueueName != "Known_Good" {
		t.Fatalf("Load() after a failed Save returned %+v, want the original Known_Good entry untouched", loaded)
	}
}

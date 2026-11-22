// Package packaging holds nothing but a mechanical guard over
// packaging/: a reviewer found packaging/darwin/io.github.phpfc.printkeeper.plist
// hardcoding one developer's own home directory, which made it a
// non-functional artifact for anyone else who checked this public
// repository out. The fix is a placeholder plus a documented one-line
// substitution (see packaging/darwin/README.md) — but the habit this
// project follows is to pin an invariant like this mechanically,
// rather than trust that nobody reintroduces it during some later
// local debugging session.
package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenPathPrefix is split across two string literals so this
// file's own source does not itself contain the literal substring it
// is scanning for — which would otherwise make this test fail against
// itself the moment someone ran it, since a packaging test file lives
// under packaging/ too.
var forbiddenPathPrefix = "/" + "Users/"

// TestNoHardcodedUserHomePath walks every file under this directory
// (packaging/ and everything below it, including packaging/darwin/)
// and fails if any of them contains a literal macOS user-home path
// such as "/Users/alice/...". A packaging artifact that only works on
// the machine it was authored on is the opposite of what anything
// under packaging/ is for.
func TestNoHardcodedUserHomePath(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			// This test's own source necessarily mentions what it is
			// looking for, in English, in its doc comments.
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), forbiddenPathPrefix) {
			t.Errorf("%s contains a hardcoded %q path — packaging artifacts must work for anyone who checks out this repository, not just the machine they were authored on; use a placeholder and document the substitution instead (see packaging/darwin/README.md)", path, forbiddenPathPrefix)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk packaging/: %v", err)
	}
}

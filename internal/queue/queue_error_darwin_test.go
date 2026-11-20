package queue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyLpadminError pins spec §5: v1 detects that queue
// administration needs authentication and explains it, rather than
// shipping a privileged helper. These stderr samples are recorded shapes
// of what lpadmin actually prints for each case, not synthetic text
// invented to make the test pass.
func TestClassifyLpadminError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   error
	}{
		{
			name:   "forbidden ipp status from a standard user",
			stderr: `lpadmin: Server returned "forbidden" status: Forbidden`,
			want:   ErrNeedsAuthorization,
		},
		{
			name:   "not authorized ipp status",
			stderr: "lpadmin: client-error-not-authorized",
			want:   ErrNeedsAuthorization,
		},
		{
			name:   "cupsd not running",
			stderr: `lpadmin: Unable to connect to "localhost:631": Connection refused`,
			want:   ErrServiceUnavailable,
		},
		{
			name:   "unrelated failure is not classified",
			stderr: "lpadmin: Printer drivers are deprecated and will stop working in a future major release of macOS.\nlpadmin: Unknown argument \"-Z\"!",
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLpadminError(tc.stderr)
			if !errors.Is(got, tc.want) {
				t.Errorf("classifyLpadminError(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestLpadminCommandForcesCLocale pins the fix for a real gap: lpadmin's
// stderr is translated through CUPS's own locale-specific message
// catalogs — the same mechanism that made the spike's `lpstat` print
// "impressora spike_test está ociosa" on this machine — so
// classifyLpadminError's English keyword matching is only sound if the
// subprocess is forced to speak English regardless of the operator's
// locale. This asserts the forcing directly on the constructed command,
// so removing it later fails here even if every classifyLpadminError
// sample still happens to be in English.
func TestLpadminCommandForcesCLocale(t *testing.T) {
	cmd := buildLpadminCmd(context.Background(), "-x", "whatever")

	if got := envValue(cmd.Env, "LC_ALL"); got != "C" {
		t.Errorf("LC_ALL = %q, want %q", got, "C")
	}
	if got := envValue(cmd.Env, "LANG"); got != "C" {
		t.Errorf("LANG = %q, want %q", got, "C")
	}
}

// TestLpadminCommandLocaleOverridesInheritedEnv confirms the forced
// locale wins even when the process's own environment already sets
// LC_ALL/LANG to something else — the override must replace the
// inherited value, not merely sit behind it where getenv's first-match
// semantics would still find the original.
func TestLpadminCommandLocaleOverridesInheritedEnv(t *testing.T) {
	t.Setenv("LC_ALL", "pt_BR.UTF-8")
	t.Setenv("LANG", "pt_BR.UTF-8")

	cmd := buildLpadminCmd(context.Background(), "-x", "whatever")

	if got := envValue(cmd.Env, "LC_ALL"); got != "C" {
		t.Errorf("LC_ALL = %q, want %q (inherited pt_BR value must not survive)", got, "C")
	}
	if n := countEnv(cmd.Env, "LC_ALL"); n != 1 {
		t.Errorf("LC_ALL appears %d times in cmd.Env, want exactly 1 (a duplicate entry risks the wrong one winning)", n)
	}
}

// envValue returns the value of the last occurrence of key in env
// (os/exec.Cmd.Env, like os.Environ, is a flat "KEY=value" slice), or ""
// if key is absent.
func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], prefix); ok {
			return v
		}
	}
	return ""
}

// countEnv counts how many entries in env set key, of any value.
func countEnv(env []string, key string) int {
	prefix := key + "="
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			n++
		}
	}
	return n
}

// TestCreateAndSetDeviceURISurfaceClassifiedError confirms the
// classification actually reaches the caller through Create and
// SetDeviceURI, not just through classifyLpadminError in isolation.
// lpadminPath is swapped for a stub script that behaves like a real
// lpadmin denying a standard user, so this stays a unit test — no real
// lpadmin, no real queue — while still exercising the full call path the
// CLI will depend on.
func TestCreateAndSetDeviceURISurfaceClassifiedError(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "lpadmin-stub.sh")
	script := "#!/bin/sh\necho 'lpadmin: Server returned \"forbidden\" status: Forbidden' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub lpadmin: %v", err)
	}

	original := lpadminPath
	lpadminPath = stub
	t.Cleanup(func() { lpadminPath = original })

	admin := NewDarwinAdmin()
	ctx := context.Background()

	if err := admin.Create(ctx, "whatever", "ipp://example.invalid/", "d"); !errors.Is(err, ErrNeedsAuthorization) {
		t.Errorf("Create against a forbidding lpadmin = %v, want ErrNeedsAuthorization", err)
	}
	if err := admin.SetDeviceURI(ctx, "whatever", "ipp://example.invalid/"); !errors.Is(err, ErrNeedsAuthorization) {
		t.Errorf("SetDeviceURI against a forbidding lpadmin = %v, want ErrNeedsAuthorization", err)
	}
	if err := admin.Delete(ctx, "whatever"); !errors.Is(err, ErrNeedsAuthorization) {
		t.Errorf("Delete against a forbidding lpadmin = %v, want ErrNeedsAuthorization", err)
	}
}

// TestRunLpadminReportsUnclassifiedFailureVerbatim confirms a failure
// classifyLpadminError does not recognise is still reported, stderr and
// all, rather than silently swallowed.
func TestRunLpadminReportsUnclassifiedFailureVerbatim(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "lpadmin-stub.sh")
	script := "#!/bin/sh\necho 'lpadmin: Unknown argument \"-Z\"!' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub lpadmin: %v", err)
	}

	original := lpadminPath
	lpadminPath = stub
	t.Cleanup(func() { lpadminPath = original })

	err := NewDarwinAdmin().Create(context.Background(), "whatever", "ipp://example.invalid/", "d")
	if err == nil {
		t.Fatal("Create against a failing lpadmin = nil error, want a reported failure")
	}
	if errors.Is(err, ErrNeedsAuthorization) || errors.Is(err, ErrServiceUnavailable) {
		t.Errorf("Create misclassified an unrelated failure as %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "Unknown argument") {
		t.Errorf("Create error = %q, want it to carry the stub's stderr", got)
	}
}

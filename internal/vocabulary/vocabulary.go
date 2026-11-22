// Package vocabulary is the automated guard behind spec §4's third
// anti-goal: no domain vocabulary — CUPS, IPP, mDNS, a raw address, a
// sanitised queue name, the daemon's own internal machinery — ever
// reaches a person using printkeeper, not even in an error message.
//
// It exists as its own importable package, not as a helper duplicated
// inside internal/cli's tests where it started, because that scoped
// it to one package's output. Nothing enforced the same promise
// against internal/ipc's own user-facing text — the "daemon is busy"
// message that reopened a Critical finding from an earlier slice
// leaked exactly because this guard could not reach the package that
// produced it. Any package that renders a sentence a non-technical
// user might read should test it against Leaked or Assert from here,
// not maintain its own copy of the list.
package vocabulary

import (
	"regexp"
	"strings"
	"testing"
)

// Forbidden is every acronym or URI scheme a rendered, user-facing
// sentence must never contain, checked case-sensitively. The
// case-sensitivity here is deliberate and protects against a real
// false positive: an ordinary English word such as "cups of coffee"
// must never trip this check, only the acronym/scheme itself in its
// actual, consistently-rendered form. "ipp://" alone does not cover
// "ipps://" — the TLS-preferred scheme — so both are listed; a bare,
// lower-case "ipp" or "ipps" as a standalone word (not merely a
// substring — see bareLowercaseIPPWord below) is covered separately,
// since a leaked URI or protocol mention will not always carry the
// "://" suffix.
var Forbidden = []string{"ipp://", "ipps://", "IPP", "mDNS", "CUPS", "URI"}

// ForbiddenCaseInsensitive is checked without regard to case, for two
// different reasons depending on the term. "queue" is checked this
// way because a sanitised CUPS queue name is always already
// lower-case, so a case-sensitive check alone would miss a literal
// raw name leaking through. "daemon", "launchd", and "socket" are
// checked this way for a reviewer's finding: unlike the acronyms in
// Forbidden above, these are ordinary prose words, and the form they
// will actually appear in is capitalised at the start of a sentence —
// "Daemon is busy" is exactly the shape of the leak this list exists
// to catch, and a case-sensitive check lets it straight through.
// Crucially, there is no false-positive risk here to protect against
// the way there is for "CUPS" or "IPP": no innocent English sentence
// in a printer tool happens to contain "daemon", "launchd", or
// "socket", so the asymmetry the acronyms need buys nothing for these
// three and only costs the capitalised form of the word that actually
// leaked.
var ForbiddenCaseInsensitive = []string{"queue", "daemon", "launchd", "socket"}

// bareLowercaseIPPWord matches a standalone, lower-case "ipp" or
// "ipps" — not merely the substring: "equipped", "flipped", and
// "tipped" are ordinary English words that happen to contain "ipp" in
// that order, and must never trip this check by accident the way a
// plain Contains would. "ipps" needs the identical word-boundary
// treatment for the identical reason, and this one pattern catches
// both the bare word and "ipps://"/"ipp://": the colon immediately
// after "ipp"/"ipps" is itself a non-word character, so \b still holds
// there. The upper-case acronym forms are already covered by Forbidden
// above.
var bareLowercaseIPPWord = regexp.MustCompile(`\bipps?\b`)

// Leaked reports every forbidden term found in s.
func Leaked(s string) []string {
	var found []string
	for _, term := range Forbidden {
		if strings.Contains(s, term) {
			found = append(found, term)
		}
	}
	lower := strings.ToLower(s)
	for _, term := range ForbiddenCaseInsensitive {
		if strings.Contains(lower, term) {
			found = append(found, term)
		}
	}
	if m := bareLowercaseIPPWord.FindString(s); m != "" {
		found = append(found, m+" (bare word)")
	}
	return found
}

// Assert fails t if s contains any forbidden term, naming every term
// found rather than stopping at the first.
func Assert(t *testing.T, s string) {
	t.Helper()
	for _, term := range Leaked(s) {
		t.Errorf("rendered output leaked domain vocabulary %q: %q", term, s)
	}
}

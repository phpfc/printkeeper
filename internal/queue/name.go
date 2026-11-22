package queue

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// maxCUPSNameLength is the longest name cupsd will accept for a print
// queue.
const maxCUPSNameLength = 127

// SanitiseName turns an advertised printer name into a valid, permanent
// CUPS queue name. CUPS names allow printable ASCII except space, "/",
// and "#", and cap at maxCUPSNameLength characters.
//
// The result is deterministic for a given (advertised, taken) pair, but
// it is a one-way transform: accents are folded to their base letter and
// every run of disallowed characters collapses to a single "_", so two
// different advertised names can legitimately sanitise to the same base
// string (e.g. "A/B" and "A#B" both fold to "a_b"). taken is how the
// caller — which is tracking every name already assigned — prevents that
// from becoming a silent collision: SanitiseName never returns a name
// present in taken, instead appending "_2", "_3", and so on until it
// finds one that is free.
//
// The collision check against taken is case-insensitive: cupsd itself
// folds printer names case-insensitively (confirmed by hand against a
// real installation — "lpstat -p epson_l4260_series" resolved the exact
// same queue as an existing "EPSON_L4260_Series"), and foldToCUPSCharset
// always lower-cases whatever name it produces. A case-sensitive check
// here would miss a collision with any taken name that was not already
// lower-cased — including, in particular, a queue a user configured by
// hand outside this tool, in whatever case its own name happens to use.
// Missing that collision means the caller's eventual Create silently
// repoints a real queue nothing here was told to touch, so this check
// is this package's responsibility, not something every caller must
// remember to normalise before calling in.
func SanitiseName(advertised string, taken []string) string {
	base := foldToCUPSCharset(advertised)
	if base == "" {
		base = "printer"
	}
	base = truncate(base, maxCUPSNameLength)

	takenSet := make(map[string]struct{}, len(taken))
	for _, t := range taken {
		takenSet[strings.ToLower(t)] = struct{}{}
	}

	name := base
	for n := 2; ; n++ {
		if _, exists := takenSet[name]; !exists {
			return name
		}
		suffix := fmt.Sprintf("_%d", n)
		name = truncate(base, maxCUPSNameLength-len(suffix)) + suffix
	}
}

// foldToCUPSCharset lowercases s, folds accented Latin letters to their
// unaccented base (so "Térrea" becomes "terrea", not "t_rrea"), and
// replaces every run of characters CUPS disallows in a queue name — space,
// "/", "#", anything outside printable ASCII — with a single "_".
func foldToCUPSCharset(s string) string {
	decomposed := norm.NFD.String(s)

	var b strings.Builder
	inRun := false
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			// A combining mark left behind by NFD decomposition — the
			// accent itself, already separated from its base letter.
			// Dropping it is the fold.
			continue
		}
		r = unicode.ToLower(r)
		if isCUPSNameRune(r) {
			b.WriteRune(r)
			inRun = false
			continue
		}
		if !inRun {
			b.WriteByte('_')
			inRun = true
		}
	}
	return b.String()
}

// isCUPSNameRune reports whether r is allowed, unescaped, in a CUPS
// queue name: printable ASCII other than space, "/", and "#".
func isCUPSNameRune(r rune) bool {
	if r < 0x21 || r > 0x7e {
		return false
	}
	return r != '/' && r != '#'
}

// truncate shortens s to at most n bytes. Every byte foldToCUPSCharset
// and the "_N" suffix can produce is single-byte ASCII, so byte-length
// truncation never splits a multi-byte rune.
func truncate(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}

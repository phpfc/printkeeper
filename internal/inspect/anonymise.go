package inspect

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"

	"github.com/phpfc/printkeeper/internal/discovery"
)

// anonymiseSalt is fixed so that the same real value always hashes to the
// same fake value, both within one report and across separate runs of this
// tool. It is not a secret: it only needs to be stable, not hidden.
const anonymiseSalt = "printkeeper-fixture-anonymise-v1"

var (
	uuidPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	macPattern  = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(:[0-9a-f]{2}){5}\b`)
	// serialPattern matches a "serial"/"s/n"-labelled value so the label can
	// be kept (it is harmless) while only the value itself is scrubbed.
	serialPattern = regexp.MustCompile(`(?i)(serial(?:\s*number)?|s/?n)([\s:=]+)([A-Za-z0-9][A-Za-z0-9-]{3,})`)
)

// Anonymise returns a copy of r with every identifying value replaced by a
// deterministic fake: UUIDs, MAC addresses, serial numbers, and any
// substring matching a hostname found on the candidate. Replacement is a
// pure function of the original value (a salted hash), so the same real
// value always maps to the same fake everywhere it appears in the report,
// and repeated calls on the same input are identical.
func Anonymise(r Report) Report {
	hostnames := candidateHostnames(r.Candidate)

	out := r
	out.Candidate.ServiceName = scrub(r.Candidate.ServiceName, hostnames)
	out.Candidate.URI = scrub(r.Candidate.URI, hostnames)
	out.Candidate.UUID = scrub(r.Candidate.UUID, hostnames)
	out.Candidate.Model = scrub(r.Candidate.Model, hostnames)
	out.Candidate.ResourcePath = scrub(r.Candidate.ResourcePath, hostnames)
	out.Candidate.Host = scrub(r.Candidate.Host, hostnames)

	if r.Candidate.PDLs != nil {
		pdls := make([]string, len(r.Candidate.PDLs))
		for i, p := range r.Candidate.PDLs {
			pdls[i] = scrub(p, hostnames)
		}
		out.Candidate.PDLs = pdls
	}

	if r.Attributes != nil {
		attrs := make(map[string][]string, len(r.Attributes))
		for name, values := range r.Attributes {
			scrubbed := make([]string, len(values))
			for i, v := range values {
				scrubbed[i] = scrub(v, hostnames)
			}
			attrs[name] = scrubbed
		}
		out.Attributes = attrs
	}

	return out
}

// candidateHostnames collects every hostname the candidate reveals: the Host
// field itself, plus whatever hostname is embedded in the URI, if any.
func candidateHostnames(c discovery.Candidate) []string {
	var hosts []string
	if c.Host != "" {
		hosts = append(hosts, c.Host)
	}
	if h := hostnameFromURI(c.URI); h != "" && h != c.Host {
		hosts = append(hosts, h)
	}
	return hosts
}

func hostnameFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// hostnamePattern matches host case-insensitively, with an optional
// trailing dot — the FQDN form IPP servers use when they echo their own
// hostname back inside attribute values such as printer-icons.
func hostnamePattern(host string) *regexp.Regexp {
	return regexp.MustCompile("(?i)" + regexp.QuoteMeta(host) + `\.?`)
}

// scrub replaces every identifying substring of s with a deterministic fake.
func scrub(s string, hostnames []string) string {
	if s == "" {
		return s
	}

	s = uuidPattern.ReplaceAllStringFunc(s, fakeUUIDFor)
	s = macPattern.ReplaceAllStringFunc(s, fakeMACFor)

	for _, host := range hostnames {
		if host == "" {
			continue
		}
		// IPP servers echo the same hostname back in lower-cased,
		// FQDN (trailing-dot) form inside URIs such as printer-icons or
		// printer-uri-supported. Match case-insensitively, with an
		// optional trailing dot, so that variant is scrubbed too.
		re := hostnamePattern(host)
		s = re.ReplaceAllStringFunc(s, func(string) string {
			return fakeHostnameFor(host)
		})
	}

	s = serialPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := serialPattern.FindStringSubmatch(m)
		return parts[1] + parts[2] + fakeSerialFor(parts[3])
	})

	return s
}

func fakeUUIDFor(real string) string {
	h := hashHex(anonymiseSalt + "|uuid|" + strings.ToLower(real))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func fakeMACFor(real string) string {
	h := hashHex(anonymiseSalt + "|mac|" + strings.ToLower(real))
	return h[0:2] + ":" + h[2:4] + ":" + h[4:6] + ":" + h[6:8] + ":" + h[8:10] + ":" + h[10:12]
}

func fakeHostnameFor(real string) string {
	h := hashHex(anonymiseSalt + "|host|" + strings.ToLower(real))
	return "fake-" + h[0:12] + ".example.invalid"
}

func fakeSerialFor(real string) string {
	h := hashHex(anonymiseSalt + "|serial|" + strings.ToLower(real))
	return "FAKE" + strings.ToUpper(h[0:12])
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

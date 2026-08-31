package guard

import "bytes"

// forEachOccurrence calls fn for every occurrence of needle in body.
func forEachOccurrence(body, needle []byte, fn func(start, end int)) {
	for i := 0; ; {
		j := bytes.Index(body[i:], needle)
		if j < 0 {
			return
		}
		j += i
		fn(j, j+len(needle))
		i = j + 1
	}
}

// pathRule is one sensitive-path category: a hit means the request body
// references a credential-bearing location (intent-level signal, before any
// secret value appears). Path hits are reported by category name only and are
// never redacted — rewriting paths would corrupt legitimate coding work.
type pathRule struct {
	category string
	literals [][]byte
	// boundary requires the literal to be delimited by non-path characters
	// (not [A-Za-z0-9_-]) on both sides, so e.g. ".env" does not fire on
	// "foo.env" or "foo.env.bar".
	boundary bool
}

// builtinPaths is the closed sensitive-path table (high-confidence locations
// only — the false-positive cost of a vague path is paid on every request).
// Entries sharing a category must stay adjacent: ScanPaths dedupes by
// comparing with the last reported category.
var builtinPaths = []pathRule{
	{"ssh", pathLiterals("~/.ssh"), false},
	// Bare key-file names get dotenv-style boundaries, so "did_rsakey" or
	// "my_id_rsa" do not fire while "id_rsa.pub" still does.
	{"ssh", pathLiterals("id_rsa", "id_ed25519"), true},
	{"aws_creds", pathLiterals("~/.aws/credentials"), false},
	{"proxy_creds", pathLiterals("~/.model-proxy"), false},
	{"gnupg", pathLiterals("~/.gnupg"), false},
	{"kube", pathLiterals("~/.kube/config"), false},
	{"docker", pathLiterals("~/.docker/config.json"), false},
	{"gcloud", pathLiterals("~/.config/gcloud"), false},
	{"dotenv", pathLiterals(".env"), true},
}

func pathLiterals(lits ...string) [][]byte {
	out := make([][]byte, len(lits))
	for i, lit := range lits {
		out[i] = []byte(lit)
	}
	return out
}

// isPathChar reports whether c can be part of a file name, for dotenv-style
// boundary checks.
func isPathChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-'
}

// pathNeedleHit post-filters one automaton hit of a path needle ending at
// end, reporting whether it counts as an occurrence. Boundary rules require
// non-path characters on both sides — the exact check the per-literal
// bytes.Contains gate applies (so ".env" still ignores "foo.env"). Used when
// the gate verdict rides a shared automaton pass (findAllGated).
func (s *Scanner) pathNeedleHit(body []byte, ref needleRef, end int) bool {
	switch ref.kind {
	case needlePath:
		p := builtinPaths[ref.idx]
		if !p.boundary {
			return true
		}
		start := end - len(p.literals[ref.vidx])
		beforeOK := start == 0 || !isPathChar(body[start-1])
		afterOK := end == len(body) || !isPathChar(body[end])
		return beforeOK && afterOK
	case needlePathExtra:
		return true
	}
	return false
}

// hit reports whether the rule matches body.
func (p pathRule) hit(body []byte) bool {
	for _, lit := range p.literals {
		if !p.boundary {
			if bytes.Contains(body, lit) {
				return true
			}
			continue
		}
		found := false
		forEachOccurrence(body, lit, func(start, end int) {
			if found {
				return
			}
			beforeOK := start == 0 || !isPathChar(body[start-1])
			afterOK := end == len(body) || !isPathChar(body[end])
			if beforeOK && afterOK {
				found = true
			}
		})
		if found {
			return true
		}
	}
	return false
}

// ScanPaths returns the deduplicated category names of the sensitive paths
// found in body, in table order. extra_paths hits report as "custom_path".
// Category names are safe to log (they contain no secret material).
//
// Standalone, the per-literal bytes.Contains gate beats a dedicated automaton
// pass (memchr is ~25x cheaper per byte than the automaton walk, and there
// are only ~10 literals); the live request path never pays this gate at all —
// it runs ScanSecretsAndPaths, where the gate verdict rides the secrets
// scan's automaton pass for free.
func (s *Scanner) ScanPaths(body []byte) []string {
	var names []string
	for _, p := range builtinPaths {
		if p.hit(body) && (len(names) == 0 || names[len(names)-1] != p.category) {
			names = append(names, p.category)
		}
	}
	for _, lit := range s.extraPaths {
		if bytes.Contains(body, lit) {
			names = append(names, "custom_path")
			break
		}
	}
	return names
}

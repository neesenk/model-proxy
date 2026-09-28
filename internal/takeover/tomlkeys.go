package takeover

import "strings"

// parseTOMLKeyPath splits a dotted TOML key (e.g. `providers."model-proxy"`)
// into its semantic parts: dots OUTSIDE quotes separate parts, whitespace
// around each part is trimmed, and quoted parts are unquoted (basic "..."
// with the common escapes, literal '...'). Two headers that differ only in
// spelling — [a."b.c"], [ a . 'b.c' ], [a.b.c] when b.c needs no quotes —
// parse to the same path.
//
// ok=false only for structurally broken input (unterminated quote, empty
// part): callers treat that as "does not match" rather than corrupting text.
func parseTOMLKeyPath(s string) ([]string, bool) {
	var parts []string
	var cur strings.Builder
	inBasic, inLiteral := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inBasic:
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else if c == '"' {
				inBasic = false
			}
		case inLiteral:
			cur.WriteByte(c)
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			cur.WriteByte(c)
			inBasic = true
		case c == '\'':
			cur.WriteByte(c)
			inLiteral = true
		case c == '.':
			part := strings.TrimSpace(cur.String())
			if part == "" {
				return nil, false
			}
			parts = append(parts, unquoteTOMLKeyPart(part))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if inBasic || inLiteral {
		return nil, false
	}
	part := strings.TrimSpace(cur.String())
	if part == "" {
		return nil, false
	}
	return append(parts, unquoteTOMLKeyPart(part)), true
}

// unquoteTOMLKeyPart strips one level of quoting from a single key part.
// Malformed quotes (a quote somewhere but not wrapping the whole part) are
// returned verbatim — matching stays conservative, never mangling text.
func unquoteTOMLKeyPart(part string) string {
	if len(part) >= 2 && part[0] == '\'' && part[len(part)-1] == '\'' {
		return part[1 : len(part)-1]
	}
	if len(part) < 2 || part[0] != '"' || part[len(part)-1] != '"' {
		return part
	}
	inner := part[1 : len(part)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' || i+1 >= len(inner) {
			b.WriteByte(c)
			continue
		}
		i++
		switch inner[i] {
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case '"', '\\':
			b.WriteByte(inner[i])
		case 'u', 'U':
			width := 4
			if inner[i] == 'U' {
				width = 8
			}
			if i+width < len(inner) {
				if r, ok := parseHexRune(inner[i+1 : i+1+width]); ok {
					b.WriteRune(r)
					i += width
					continue
				}
			}
			b.WriteByte('\\')
			b.WriteByte(inner[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(inner[i])
		}
	}
	return b.String()
}

func parseHexRune(hex string) (rune, bool) {
	var r rune
	for i := 0; i < len(hex); i++ {
		d := rune(hex[i])
		switch {
		case d >= '0' && d <= '9':
			d -= '0'
		case d >= 'a' && d <= 'f':
			d -= 'a' - 10
		case d >= 'A' && d <= 'F':
			d -= 'A' - 10
		default:
			return 0, false
		}
		r = r<<4 | d
	}
	return r, true
}

// tomlHeaderPath reports whether a trimmed line is a table header and returns
// its semantic key path. `[[array]]` headers count as headers (they terminate
// a section body) with isArray=true — they never equal a plain-section target,
// so replace/remove never touches them.
func tomlHeaderPath(trimmedLine string) (parts []string, isArray bool, ok bool) {
	if !strings.HasPrefix(trimmedLine, "[") || !strings.HasSuffix(trimmedLine, "]") {
		return nil, false, false
	}
	inner := trimmedLine[1 : len(trimmedLine)-1]
	if strings.HasPrefix(inner, "[") && strings.HasSuffix(inner, "]") {
		inner = inner[1 : len(inner)-1]
		isArray = true
	}
	parts, ok = parseTOMLKeyPath(inner)
	return parts, isArray, ok
}

// equalTOMLKeyPath compares two parsed key paths.
func equalTOMLKeyPath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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

// tomlStructuralLines scans the whole file once and reports which lines are
// STRUCTURAL TOML (headers, key lines, comments, blanks) versus CONTENT of a
// multi-line construct — a """...""" / ”'...”' string or an array whose
// brackets span lines. Header recognition and section-boundary scans must
// only look at structural lines: a line starting with `[` inside a
// multi-line array, or a `[section]`-shaped line inside a multi-line string,
// is data — treating it as a header replaces/removes user content or cuts a
// section body in the middle of a value, producing unparseable TOML.
func tomlStructuralLines(lines []string) []bool {
	structural := make([]bool, len(lines))
	var mlString byte // '"' or '\'' while inside a multi-line string
	arrayDepth := 0   // unclosed [ ] outside strings (arrays spanning lines)
	for i, line := range lines {
		structural[i] = mlString == 0 && arrayDepth == 0
		inBasic, inLiteral := false, false
	scan:
		for j := 0; j < len(line); j++ {
			c := line[j]
			switch {
			case mlString == '"':
				if c == '\\' {
					j++ // escaped char (incl. \") is not a delimiter
				} else if c == '"' && j+2 < len(line) && line[j+1] == '"' && line[j+2] == '"' {
					mlString = 0
					j += 2
				}
			case mlString == '\'':
				if c == '\'' && j+2 < len(line) && line[j+1] == '\'' && line[j+2] == '\'' {
					mlString = 0
					j += 2
				}
			case inBasic:
				if c == '\\' {
					j++
				} else if c == '"' {
					inBasic = false
				}
			case inLiteral:
				if c == '\'' {
					inLiteral = false
				}
			case c == '#':
				break scan // comment: the rest of the line carries no structure
			case c == '"':
				if j+2 < len(line) && line[j+1] == '"' && line[j+2] == '"' {
					if close := indexTripleQuote(line, j+3, '"'); close >= 0 {
						j = close + 2 // opens and closes on this line
					} else {
						mlString = '"'
						break scan
					}
				} else {
					inBasic = true
				}
			case c == '\'':
				if j+2 < len(line) && line[j+1] == '\'' && line[j+2] == '\'' {
					if close := indexTripleQuote(line, j+3, '\''); close >= 0 {
						j = close + 2
					} else {
						mlString = '\''
						break scan
					}
				} else {
					inLiteral = true
				}
			case c == '[':
				arrayDepth++
			case c == ']':
				if arrayDepth > 0 {
					arrayDepth--
				}
			}
		}
	}
	return structural
}

// indexTripleQuote finds the next triple-quote delimiter (""" or ”') at or
// after from. A delimiter whose first quote is backslash-escaped (basic
// strings only) is content, not a delimiter.
func indexTripleQuote(line string, from int, q byte) int {
	for j := from; j+2 < len(line); j++ {
		if line[j] != q || line[j+1] != q || line[j+2] != q {
			continue
		}
		if q == '"' {
			slashes := 0
			for k := j - 1; k >= 0 && line[k] == '\\'; k-- {
				slashes++
			}
			if slashes%2 == 1 {
				continue
			}
		}
		return j
	}
	return -1
}

// stripTOMLLineComment returns the line with any trailing comment removed:
// the comment starts at the first `#` OUTSIDE quotes. A `#` inside a quoted
// key part (`[providers."a#b"]`) is data, not a comment. An unterminated
// quote leaves the line untouched — parseTOMLKeyPath then rejects it.
func stripTOMLLineComment(line string) string {
	inBasic, inLiteral := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inBasic:
			if c == '\\' {
				i++
			} else if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == '#':
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return line
}

// tomlHeaderPath reports whether a trimmed line is a table header and returns
// its semantic key path. `[[array]]` headers count as headers (they terminate
// a section body) with isArray=true — they never equal a plain-section target,
// so replace/remove never touches them. A trailing comment after the closing
// bracket (`[providers.model-proxy] # note`) is stripped before the suffix
// check: TOML allows it, and missing it would treat the header as absent and
// append a duplicate table. The line must be a STRUCTURAL line (see
// tomlStructuralLines): a header-shaped line inside a multi-line string or
// array is content and must never reach this parser.
func tomlHeaderPath(trimmedLine string) (parts []string, isArray bool, ok bool) {
	trimmedLine = stripTOMLLineComment(trimmedLine)
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

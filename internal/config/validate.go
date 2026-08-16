package config

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidationIssue is one config problem with a best-effort 1-based source line
// (0 = not locatable, e.g. a whole-document error like "no providers
// configured"). It powers POST /api/config/validate so the Web Raw YAML editor
// can lint as you type instead of only on save.
type ValidationIssue struct {
	Line    int
	Message string
}

// ValidateYAML runs the exact parse+validate pipeline of LoadConfigFromBytes
// but reports structured issues instead of a single error. A nil result means
// the bytes are a valid config. The path argument of LoadConfigFromBytes only
// feeds error messages / relative-path resolution, so validation passes "".
func ValidateYAML(data []byte) []ValidationIssue {
	// Parse into a yaml.Node tree once: on success its key locations pin
	// semantic errors to a line; on failure the parse error itself carries the
	// line and the tree is simply unused.
	var doc yaml.Node
	_ = yaml.Unmarshal(data, &doc)

	_, err := LoadConfigFromBytes("", data)
	if err == nil {
		return nil
	}
	// Type errors carry one "line N: ..." entry per offending field — report
	// each separately so every broken field shows up, not just the first.
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) && len(typeErr.Errors) > 0 {
		issues := make([]ValidationIssue, 0, len(typeErr.Errors))
		for _, entry := range typeErr.Errors {
			line, message := splitLinePrefix(entry)
			issues = append(issues, ValidationIssue{Line: line, Message: message})
		}
		// LoadConfigFromBytes appends a fix hint for known opaque errors (e.g.
		// the old map-form models: block); keep it on the first issue.
		if hint := hintSuffix(err.Error()); hint != "" {
			issues[0].Message += "\n" + hint
		}
		return issues
	}
	message := err.Error()
	if line, _ := splitLinePrefix(message); line > 0 {
		// Syntax error: yaml.v3 embeds the exact line in the message.
		return []ValidationIssue{{Line: line, Message: message}}
	}
	// Semantic errors (Config.validate) name the offending key in prose;
	// locate it in the node tree on a best-effort basis.
	return []ValidationIssue{{Line: locateIssueLine(&doc, message), Message: message}}
}

var yamlLineRe = regexp.MustCompile(`line (\d+)`)

// splitLinePrefix extracts a "line N: " prefix (yaml.v3 syntax/type error
// shape) and returns the line plus the message with the prefix stripped.
func splitLinePrefix(message string) (int, string) {
	if strings.HasPrefix(message, "line ") {
		if end := strings.Index(message, ": "); end > 0 {
			if n, convErr := strconv.Atoi(message[len("line "):end]); convErr == nil {
				return n, message[end+2:]
			}
		}
	}
	if match := yamlLineRe.FindStringSubmatch(message); match != nil {
		n, _ := strconv.Atoi(match[1])
		return n, message
	}
	return 0, message
}

// hintSuffix returns the "\nhint: ..." guidance LoadConfigFromBytes appends to
// opaque yaml errors, if present.
func hintSuffix(message string) string {
	if index := strings.Index(message, "\nhint:"); index >= 0 {
		return strings.TrimPrefix(message[index:], "\n")
	}
	return ""
}

var (
	// "scheduling.quota_poll_interval: invalid duration ..." (duration checks
	// report the dotted field path at the message start).
	dottedFieldRe = regexp.MustCompile(`^((?:scheduling|cache|pricing|stats|request_log)\.[a-z_]+):`)
	// `provider "x": ...` / `route "x" target N: ...` / `shadow "x": ...` etc.
	sectionKeyRe  = regexp.MustCompile(`^(provider|route|claude_mapping|shadow|fusion) "([^"]+)"`)
	routeTargetRe = regexp.MustCompile(`^route "[^"]+" target (\d+)`)
)

// sectionRoot maps the singular noun used in validate messages to the
// top-level YAML key holding that section.
func sectionRoot(noun string) string {
	switch noun {
	case "provider":
		return "providers"
	case "route":
		return "routes"
	default:
		return noun
	}
}

// locateIssueLine maps a semantic validate error to the 1-based line of the
// offending key, or 0 when no location can be derived.
func locateIssueLine(doc *yaml.Node, message string) int {
	root := doc
	if root != nil && root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return 0
	}
	if match := dottedFieldRe.FindStringSubmatch(message); match != nil {
		if line := keyPathLine(root, strings.Split(match[1], ".")); line > 0 {
			return line
		}
	}
	if match := sectionKeyRe.FindStringSubmatch(message); match != nil {
		section := sectionRoot(match[1])
		name := match[2]
		if target := routeTargetRe.FindStringSubmatch(message); target != nil {
			if index, convErr := strconv.Atoi(target[1]); convErr == nil {
				if line := seqItemLine(root, section, name, index); line > 0 {
					return line
				}
			}
		}
		if line := keyPathLine(root, []string{section, name}); line > 0 {
			return line
		}
	}
	// Bare top-level keys reported by name: "listen ...", "shadow_sample_rate ...".
	for _, key := range []string{"listen", "shadow_sample_rate", "shadow_max_concurrent"} {
		if strings.HasPrefix(message, key) {
			if line := keyPathLine(root, []string{key}); line > 0 {
				return line
			}
		}
	}
	return 0
}

// keyPathLine walks mapping keys and returns the line of the final key node.
func keyPathLine(root *yaml.Node, path []string) int {
	node := root
	for depth, key := range path {
		if node == nil || node.Kind != yaml.MappingNode {
			return 0
		}
		found := false
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				if depth == len(path)-1 {
					return node.Content[index].Line
				}
				node = node.Content[index+1]
				found = true
				break
			}
		}
		if !found {
			return 0
		}
	}
	return 0
}

// seqItemLine returns the line of the index-th item of the sequence at
// root[section][name] (e.g. routes.<name> target N).
func seqItemLine(root *yaml.Node, section, name string, index int) int {
	sectionNode := lookupMapValue(root, section)
	if sectionNode == nil {
		return 0
	}
	seq := lookupMapValue(sectionNode, name)
	if seq == nil || seq.Kind != yaml.SequenceNode || index >= len(seq.Content) {
		return 0
	}
	return seq.Content[index].Line
}

func lookupMapValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

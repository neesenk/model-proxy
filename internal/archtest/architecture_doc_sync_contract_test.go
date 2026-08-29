package archtest

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestOverviewDependencyDocMirrorsPolicy locks the "互为镜像——两处必须同步
// 修改" contract in docs/architecture/overview.md: the doc's dependency lists
// must match internalRepositoryImportPolicy exactly. Before this guard the two
// had drifted (stale leaf classifications, missing packages) with nothing red.
func TestOverviewDependencyDocMirrorsPolicy(t *testing.T) {
	raw, err := os.ReadFile(repoRooted(t, "docs/architecture/overview.md"))
	if err != nil {
		t.Fatalf("read overview.md: %v", err)
	}
	doc := string(raw)
	marker := "互为镜像——两处必须同步修改："
	start := strings.Index(doc, marker)
	if start < 0 {
		t.Fatalf("overview.md lost the mirror marker %q", marker)
	}
	section := doc[start+len(marker):]
	end := strings.Index(section, "`internal/takeover` 拥有")
	if end < 0 {
		t.Fatal("overview.md dependency section end anchor missing")
	}
	section = section[:end]

	docDeps := map[string]map[string]bool{}
	leafRe := regexp.MustCompile(`(?s)叶子包.*?：(.*?)；`)
	leaves := leafRe.FindStringSubmatch(section)
	if leaves == nil {
		t.Fatal("overview.md leaf-package list missing")
	}
	for _, name := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(leaves[1], -1) {
		docDeps["model-proxy/internal/"+name[1]] = nil
	}
	// Entries may carry trailing explanatory notes after the closing backtick.
	edgeRe := regexp.MustCompile("- `([\\w/]+) → ([^`]+)`")
	for _, m := range edgeRe.FindAllStringSubmatch(section, -1) {
		deps := map[string]bool{}
		for _, d := range strings.Split(m[2], ",") {
			deps["model-proxy/internal/"+strings.TrimSpace(d)] = true
		}
		docDeps["model-proxy/internal/"+m[1]] = deps
	}

	policy := internalRepositoryImportPolicy()
	for pkg := range policy {
		got, ok := docDeps[pkg]
		if !ok {
			t.Errorf("overview.md missing dependency entry for %s (policy: %v)", pkg, sortedKeys(policy[pkg]))
			continue
		}
		if !sameKeySet(got, policy[pkg]) {
			t.Errorf("overview.md deps for %s = %v, policy = %v", pkg, sortedKeys(got), sortedKeys(policy[pkg]))
		}
	}
	for pkg := range docDeps {
		if _, ok := policy[pkg]; !ok {
			t.Errorf("overview.md lists unknown package %s (stale doc entry)", pkg)
		}
	}
}

func sameKeySet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// small n: simple insertion sort keeps the helper dependency-free
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

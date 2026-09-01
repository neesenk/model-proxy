package app

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
)

// This file pins docs sync for the /api/config surface (docs/web-api.md 契约:
// "新增或修改 /api/* 字段时先更新 docs/web-api.md，再更新前端"): the JSON keys
// the response struct emits must be exactly the keys documented in the
// endpoint table row, in both directions. Adding a field without documenting
// it — or documenting a removed field — fails here.

// documentedConfigKeys extracts the `{a, b, ...}` key list from the
// `| GET | `/api/config` | ... |` row of docs/web-api.md.
func documentedConfigKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../docs/web-api.md")
	if err != nil {
		t.Fatalf("read docs/web-api.md: %v", err)
	}
	var row string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "| GET |") && strings.Contains(line, "`/api/config`") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("docs/web-api.md has no `| GET | `/api/config` |` row — docs contract is blind")
	}
	m := regexp.MustCompile(`\{([^{}]*)\}`).FindStringSubmatch(row)
	if m == nil {
		t.Fatalf("api/config row documents no `{...}` key list: %s", row)
	}
	out := map[string]bool{}
	for _, k := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' }) {
		if k != "" {
			out[k] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("api/config row documents an empty key list: %s", row)
	}
	return out
}

func configDocumentJSONKeys(t *testing.T) map[string]bool {
	t.Helper()
	typ := reflect.TypeOf(appapi.ConfigDocument{})
	out := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("ConfigDocument field %s has no json tag — every field is API surface", typ.Field(i).Name)
		}
		out[name] = true
	}
	return out
}

func TestAPIConfigDocsMatchResponseStruct(t *testing.T) {
	docs := documentedConfigKeys(t)
	code := configDocumentJSONKeys(t)

	var missingDocs, staleDocs []string
	for k := range code {
		if !docs[k] {
			missingDocs = append(missingDocs, k)
		}
	}
	for k := range docs {
		if !code[k] {
			staleDocs = append(staleDocs, k)
		}
	}
	sort.Strings(missingDocs)
	sort.Strings(staleDocs)
	if len(missingDocs) > 0 {
		t.Errorf("GET /api/config emits undocumented field(s) %s — document them in docs/web-api.md first (assets/AGENTS.md rule)", strings.Join(missingDocs, ", "))
	}
	if len(staleDocs) > 0 {
		t.Errorf("docs/web-api.md documents field(s) %s that GET /api/config no longer emits — stale docs?", strings.Join(staleDocs, ", "))
	}
}

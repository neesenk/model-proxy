package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	observeseclog "model-proxy/internal/observe/seclog"
)

// writeAuditRecord appends one record to the audit log dir via the same
// writer the producers use (timestamps in unix milliseconds).
func writeAuditRecord(t *testing.T, dir string, rec *observeseclog.Record) {
	t.Helper()
	if err := observeseclog.AppendSync(dir, rec); err != nil {
		t.Fatalf("AppendSync: %v", err)
	}
}

func TestParseAuditFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    AuditOpts
		wantErr bool
	}{
		{"defaults", nil, AuditOpts{Limit: 50}, false},
		{"space form", []string{"--from", "1h", "--to", "now", "--kind", "drift", "--limit", "10", "--json"},
			AuditOpts{From: "1h", To: "now", Kind: "drift", Limit: 10, JSON: true}, false},
		{"equals form", []string{"--from=2026-08-01T00:00:00Z", "--kind=secret", "--limit=0"},
			AuditOpts{From: "2026-08-01T00:00:00Z", Kind: "secret", Limit: 0}, false},
		{"bad limit", []string{"--limit", "abc"}, AuditOpts{Limit: 50}, true},
		{"bad limit equals", []string{"--limit="}, AuditOpts{Limit: 50}, true},
		{"unknown flag", []string{"--bogus"}, AuditOpts{Limit: 50}, true},
		{"unknown positional", []string{"drift"}, AuditOpts{Limit: 50}, true},
		{"missing from value", []string{"--from"}, AuditOpts{Limit: 50}, true},
		{"missing kind value", []string{"--json", "--kind"}, AuditOpts{Limit: 50, JSON: true}, true},
		{"missing limit value", []string{"--limit"}, AuditOpts{Limit: 50}, true},
		// --config is resolved by configPath from the full args; the parser
		// only skips it (both forms) instead of flagging it unknown.
		{"config space form skipped", []string{"--config", "/tmp/x.yaml", "--kind", "drift"},
			AuditOpts{Kind: "drift", Limit: 50}, false},
		{"config equals form skipped", []string{"--config=/tmp/x.yaml"}, AuditOpts{Limit: 50}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAuditFlags(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAuditFlags(%v): want error, got %+v", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAuditFlags(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("ParseAuditFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseAuditTime(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"now", now.UnixMilli(), false},
		{"1h", now.Add(-time.Hour).UnixMilli(), false},
		{"30m", now.Add(-30 * time.Minute).UnixMilli(), false},
		{"1750000000", 1750000000 * 1000, false},
		{"2026-08-25T10:00:00Z", time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC).UnixMilli(), false},
		{"-1h", 0, true}, // negative duration is a future timestamp — always a typo
		{"garbage", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseAuditTime(tc.in, now)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseAuditTime(%q): want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAuditTime(%q): %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("ParseAuditTime(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestRenderAudit covers the offline viewer end to end: table columns, kind/
// time/limit filters, exact --json fields, empty/missing-dir notes, and the
// invalid --kind error.
func TestRenderAudit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	secretRec := &observeseclog.Record{
		Ts:        now.Add(-30 * time.Minute).UnixMilli(),
		Kind:      observeseclog.KindSecret,
		RequestID: "req-1",
		Agent:     "claude-code",
		Protocol:  "anthropic",
		Exposed:   "glm-5.2",
		Names:     []string{"known_secret", "aws_key"},
		Action:    "log",
	}
	oldRec := &observeseclog.Record{
		Ts:    now.Add(-2 * time.Hour).UnixMilli(),
		Kind:  observeseclog.KindPath,
		Agent: "codex",
		Names: []string{"ssh"},
	}
	writeAuditRecord(t, dir, secretRec)
	writeAuditRecord(t, dir, oldRec)
	writeAuditRecord(t, dir, &observeseclog.Record{
		Ts: now.Add(-time.Minute).UnixMilli(), Kind: observeseclog.KindDrift,
		Agent: "doctor", Detail: "client=opencode expected=h1 actual=h2",
	})

	// Table: header columns + one row per record, names comma-joined.
	out, err := RenderAudit(dir, AuditOpts{Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"time", "kind", "agent", "route", "names", "action", "detail",
		"secret", "claude-code", "glm-5.2", "known_secret,aws_key", "log",
		"path", "codex", "ssh",
		"drift", "doctor", "client=opencode expected=h1 actual=h2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}

	// --kind narrows to one kind.
	out, err = RenderAudit(dir, AuditOpts{Kind: "drift", Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "doctor") || strings.Contains(out, "claude-code") || strings.Contains(out, "codex") {
		t.Errorf("--kind drift table wrong:\n%s", out)
	}

	// --from 1h drops the 2h-old record.
	out, err = RenderAudit(dir, AuditOpts{From: "1h", Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "ssh") {
		t.Errorf("--from 1h should drop the 2h-old record:\n%s", out)
	}
	if !strings.Contains(out, "claude-code") || !strings.Contains(out, "doctor") {
		t.Errorf("--from 1h should keep recent records:\n%s", out)
	}

	// --limit 1 keeps only the newest.
	out, err = RenderAudit(dir, AuditOpts{Limit: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "doctor") || strings.Contains(out, "claude-code") {
		t.Errorf("--limit 1 should keep only the newest record:\n%s", out)
	}

	// --json: parseable array with the exact record fields, newest first.
	out, err = RenderAudit(dir, AuditOpts{JSON: true, Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	var records []observeseclog.Record
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("--json output not parseable: %v\n%s", err, out)
	}
	if len(records) != 3 {
		t.Fatalf("--json records = %d, want 3", len(records))
	}
	if records[0].Kind != "drift" || records[2].Kind != "path" {
		t.Errorf("--json not newest-first: %+v", records)
	}
	got := records[1] // the secret record
	if got.Kind != "secret" || got.RequestID != "req-1" || got.Agent != "claude-code" ||
		got.Protocol != "anthropic" || got.Exposed != "glm-5.2" || got.Action != "log" ||
		len(got.Names) != 2 || got.Names[0] != "known_secret" || got.Names[1] != "aws_key" {
		t.Errorf("--json secret record fields wrong: %+v", got)
	}

	// Invalid kind is an error, invalid time is an error.
	if _, err := RenderAudit(dir, AuditOpts{Kind: "bogus"}, now); err == nil ||
		!strings.Contains(err.Error(), `invalid --kind "bogus"`) {
		t.Errorf("invalid --kind: got %v", err)
	}
	if _, err := RenderAudit(dir, AuditOpts{From: "bogus"}, now); err == nil ||
		!strings.Contains(err.Error(), "invalid --from") {
		t.Errorf("invalid --from: got %v", err)
	}
	// --from after --to is an empty window — report it instead of rendering
	// an empty table that looks like "no records".
	if _, err := RenderAudit(dir, AuditOpts{From: "30m", To: "1h"}, now); err == nil ||
		!strings.Contains(err.Error(), "--from is after --to") {
		t.Errorf("--from > --to: got %v", err)
	}

	// Empty (existing) dir and missing dir both render a friendly note.
	out, err = RenderAudit(t.TempDir(), AuditOpts{Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(no security audit records in ") {
		t.Errorf("empty dir note missing:\n%s", out)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	out, err = RenderAudit(missing, AuditOpts{Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(no security audit records yet") || !strings.Contains(out, "does not exist") {
		t.Errorf("missing dir note wrong:\n%s", out)
	}

	// --json on an empty dir is still valid JSON ([]), for jq.
	out, err = RenderAudit(t.TempDir(), AuditOpts{JSON: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	var empty []observeseclog.Record
	if err := json.Unmarshal([]byte(out), &empty); err != nil || len(empty) != 0 {
		t.Errorf("empty --json = %q, want []: %v", out, err)
	}
}

// TestFormatAuditTableEmpty locks the no-records note.
func TestFormatAuditTableEmpty(t *testing.T) {
	out := FormatAuditTable(nil, "/tmp/x")
	if !strings.Contains(out, "(no security audit records in /tmp/x)") {
		t.Errorf("empty table note wrong: %q", out)
	}
}

// TestFormatAuditTableSanitizesDetail: control characters in a record's
// detail (from a future producer, or a tampered log file) must never break
// the table layout — they render as spaces on a single line.
func TestFormatAuditTableSanitizesDetail(t *testing.T) {
	records := []*observeseclog.Record{{
		Ts:     time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC).UnixMilli(),
		Kind:   observeseclog.KindDrift,
		Agent:  "doctor",
		Detail: "client=pi\nexpected=h1\tactual=h2\x1b[31m",
	}}
	out := FormatAuditTable(records, "/tmp/x")
	if strings.Contains(out, "client=pi\n") || strings.ContainsAny(out, "\t\x1b") {
		t.Errorf("detail control characters leaked into the table:\n%q", out)
	}
	if !strings.Contains(out, "client=pi expected=h1 actual=h2 [31m") {
		t.Errorf("sanitized detail missing from the table:\n%q", out)
	}
	// Header + exactly one record line.
	if lines := strings.Count(out, "\n"); lines != 2 {
		t.Errorf("table lines = %d, want 2 (header + 1 record):\n%q", lines, out)
	}
}

// TestAuditCLISubprocess drives the real command through the subprocess
// harness: config-derived audit dir, table on stdout, --json parseable,
// invalid --kind on stderr with exit 1.
func TestAuditCLISubprocess(t *testing.T) {
	home := t.TempDir()
	cfgPath := writeTempConfig(t, minimalConfig)
	dir := filepath.Join(home, ".model-proxy")
	writeAuditRecord(t, dir, &observeseclog.Record{
		Kind: observeseclog.KindDrift, Agent: "doctor",
		Detail: "client=pi expected=h1 actual=h2",
	})

	stdout, stderr, code := runCLIWithHome(t, home, "audit", cfgPath)
	if code != 0 {
		t.Fatalf("audit exit = %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"kind", "doctor", "drift", "client=pi"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit stdout missing %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, code = runCLIWithHome(t, home, "audit", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("audit --json exit = %d, stderr:\n%s", code, stderr)
	}
	var records []observeseclog.Record
	if err := json.Unmarshal([]byte(stdout), &records); err != nil || len(records) != 1 {
		t.Fatalf("audit --json = %q: %v", stdout, err)
	}
	if records[0].Kind != "drift" || records[0].Agent != "doctor" {
		t.Errorf("audit --json record wrong: %+v", records[0])
	}

	stdout, stderr, code = runCLIWithHome(t, home, "audit", cfgPath, "--kind", "bogus")
	if code != 1 {
		t.Errorf("audit --kind bogus exit = %d, want 1 (stdout %q)", code, stdout)
	}
	if !strings.Contains(stderr, `invalid --kind "bogus"`) {
		t.Errorf("audit --kind bogus stderr = %q", stderr)
	}

	// No log yet: friendly note on stdout, exit 0 (offline semantics).
	emptyHome := t.TempDir()
	stdout, _, code = runCLIWithHome(t, emptyHome, "audit", cfgPath)
	if code != 0 {
		t.Fatalf("audit (no log) exit = %d", code)
	}
	if !strings.Contains(stdout, "no security audit records") {
		t.Errorf("audit (no log) stdout = %q", stdout)
	}
}

// TestAuditCLIRespectsConfigAuditPath: guard.audit_path redirects the log
// location; audit must read from there, not the default.
func TestAuditCLIRespectsConfigAuditPath(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "sec")
	cfgPath := writeTempConfig(t, minimalConfig+"guard:\n  audit_path: "+filepath.Join(custom, "security.log")+"\n")
	writeAuditRecord(t, custom, &observeseclog.Record{
		Kind: observeseclog.KindSecret, Agent: "pi", Names: []string{"known_secret"}, Action: "block",
	})
	stdout, _, code := runCLIWithHome(t, home, "audit", cfgPath)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "known_secret") || !strings.Contains(stdout, "block") {
		t.Errorf("audit did not read guard.audit_path:\n%s", stdout)
	}
}

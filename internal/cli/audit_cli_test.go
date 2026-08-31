package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
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
		{"stats flag", []string{"--stats"}, AuditOpts{Limit: 50, Stats: true}, false},
		{"stats combined", []string{"--stats", "--json", "--kind", "secret", "--from", "7d"},
			AuditOpts{From: "7d", Kind: "secret", Limit: 50, JSON: true, Stats: true}, false},
		{"equals form", []string{"--from=2026-08-01T00:00:00Z", "--kind=secret", "--limit=0"},
			AuditOpts{From: "2026-08-01T00:00:00Z", Kind: "secret", Limit: 0}, false},
		{"bad limit", []string{"--limit", "abc"}, AuditOpts{Limit: 50}, true},
		{"bad limit equals", []string{"--limit="}, AuditOpts{Limit: 50}, true},
		{"unknown flag", []string{"--bogus"}, AuditOpts{Limit: 50}, true},
		{"unknown positional", []string{"drift"}, AuditOpts{Limit: 50}, true},
		{"missing from value", []string{"--from"}, AuditOpts{Limit: 50}, true},
		{"missing kind value", []string{"--json", "--kind"}, AuditOpts{Limit: 50, JSON: true}, true},
		{"missing limit value", []string{"--limit"}, AuditOpts{Limit: 50}, true},
		// A negative limit must error, not silently mean "no cap": the user
		// mistyped a value, and a silently ignored sign hides the typo the
		// same way an unparseable value would.
		{"negative limit", []string{"--limit", "-5"}, AuditOpts{Limit: 50}, true},
		{"negative limit equals", []string{"--limit=-3"}, AuditOpts{Limit: 50}, true},
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
		{"7d", now.AddDate(0, 0, -7).UnixMilli(), false},
		{"1d", now.AddDate(0, 0, -1).UnixMilli(), false},
		{"1750000000", 1750000000 * 1000, false},
		{"2026-08-25T10:00:00Z", time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC).UnixMilli(), false},
		{"-1h", 0, true}, // negative duration is a future timestamp — always a typo
		{"-7d", 0, true},
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

// TestRenderAuditStats covers the --stats aggregate view: exact counts across
// every dimension over the whole filtered set, kind/time filter combination,
// --limit being ignored, empty results, and exact --json fields.
func TestRenderAuditStats(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	write := func(ts time.Time, kind, agent string, names []string, action string) {
		writeAuditRecord(t, dir, &observeseclog.Record{
			Ts: ts.UnixMilli(), Kind: kind, Agent: agent, Names: names, Action: action,
		})
	}
	write(now.Add(-10*time.Minute), "secret", "codex", []string{"aws_key", "known_secret"}, "block")
	write(now.Add(-20*time.Minute), "secret", "claude-code", []string{"aws_key"}, "log")
	write(now.Add(-30*time.Minute), "path", "codex", []string{"ssh"}, "log")
	write(now.Add(-2*time.Hour), "drift", "doctor", nil, "")

	// parseSection extracts one "  <name> <count>" count table into a map.
	parseSection := func(out, title string) map[string]int {
		t.Helper()
		lines := strings.Split(out, "\n")
		for i, line := range lines {
			if line != title {
				continue
			}
			counts := map[string]int{}
			for _, row := range lines[i+1:] {
				if row == "" {
					return counts
				}
				fields := strings.Fields(row)
				if len(fields) != 2 {
					t.Fatalf("malformed stats row %q in section %q:\n%s", row, title, out)
				}
				n, err := strconv.Atoi(fields[1])
				if err != nil {
					t.Fatalf("non-integer count in row %q: %v", row, err)
				}
				counts[fields[0]] = n
			}
			return counts
		}
		t.Fatalf("section %q missing:\n%s", title, out)
		return nil
	}

	// Full set: header + exact counts in every section.
	out, err := RenderAudit(dir, AuditOpts{Stats: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "security audit stats  range: - .. -  total: 4 records") {
		t.Errorf("stats header wrong:\n%s", out)
	}
	for title, want := range map[string]map[string]int{
		"by kind":             {"secret": 2, "path": 1, "drift": 1},
		"top names (top 10)":  {"aws_key": 2, "known_secret": 1, "ssh": 1},
		"top agents (top 10)": {"codex": 2, "claude-code": 1, "doctor": 1},
		"by action":           {"log": 2, "block": 1},
	} {
		if got := parseSection(out, title); !reflect.DeepEqual(got, want) {
			t.Errorf("section %q = %v, want %v:\n%s", title, got, want, out)
		}
	}
	// Ordering: count desc, then name asc for ties.
	if strings.Index(out, "aws_key") > strings.Index(out, "known_secret") {
		t.Errorf("top names not count-ordered:\n%s", out)
	}
	if strings.Index(out, "claude-code") > strings.Index(out, "doctor") {
		t.Errorf("tied agents not name-ordered:\n%s", out)
	}

	// --stats ignores --limit (the table pager): the whole set is counted.
	out, err = RenderAudit(dir, AuditOpts{Stats: true, Limit: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "total: 4 records") {
		t.Errorf("--stats must ignore --limit 1:\n%s", out)
	}

	// --kind narrows the aggregated set.
	out, err = RenderAudit(dir, AuditOpts{Stats: true, Kind: "secret"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "total: 2 records") {
		t.Errorf("--stats --kind secret total wrong:\n%s", out)
	}
	if got, want := parseSection(out, "by kind"), map[string]int{"secret": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("--kind secret by kind = %v, want %v:\n%s", got, want, out)
	}
	if strings.Contains(out, "doctor") || strings.Contains(out, "ssh") {
		t.Errorf("--kind secret leaked other kinds:\n%s", out)
	}

	// --from combines with --stats; the header echoes the query window.
	out, err = RenderAudit(dir, AuditOpts{Stats: true, From: "1h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "total: 3 records") {
		t.Errorf("--stats --from 1h should drop the 2h-old record:\n%s", out)
	}
	window := time.UnixMilli(now.Add(-time.Hour).UnixMilli()).Format("2006-01-02 15:04:05")
	if !strings.Contains(out, "range: "+window+" .. -") {
		t.Errorf("stats header missing the --from window %q:\n%s", window, out)
	}

	// Empty result: header with total 0 plus the no-records note.
	out, err = RenderAudit(t.TempDir(), AuditOpts{Stats: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "total: 0 records") || !strings.Contains(out, "(no security audit records in ") {
		t.Errorf("empty --stats output wrong:\n%s", out)
	}

	// --json: parseable aggregate object with exact fields.
	out, err = RenderAudit(dir, AuditOpts{Stats: true, JSON: true, Kind: "secret", From: "1h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	var got AuditStats
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--stats --json not parseable: %v\n%s", err, out)
	}
	want := AuditStats{
		From:      now.Add(-time.Hour).UnixMilli(),
		Total:     2,
		ByKind:    map[string]int{"secret": 2},
		ByAction:  map[string]int{"block": 1, "log": 1},
		TopNames:  []AuditStatCount{{Name: "aws_key", Count: 2}, {Name: "known_secret", Count: 1}},
		TopAgents: []AuditStatCount{{Name: "claude-code", Count: 1}, {Name: "codex", Count: 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--stats --json = %+v, want %+v", got, want)
	}
	if !strings.Contains(out, `"by_kind":{"secret":2}`) || !strings.Contains(out, `"total":2`) {
		t.Errorf("--stats --json field names wrong:\n%s", out)
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

	stdout, stderr, code = runCLIWithHome(t, home, "audit", cfgPath, "--stats")
	if code != 0 {
		t.Fatalf("audit --stats exit = %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"total: 1 record", "by kind", "drift", "doctor"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit --stats stdout missing %q:\n%s", want, stdout)
		}
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

// TestRenderAuditStatsTruncation: --stats caps its scan at auditStatsLimit
// (newest first). When more records match than the cap retains, the output
// must say so — a bare "total" below the real count is misleading. Both
// renderings are asserted: the JSON gains "truncated": true and the table
// gains an explicit truncation note. Below the cap neither marker appears.
func TestRenderAuditStatsTruncation(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	write := func(dir string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			writeAuditRecord(t, dir, &observeseclog.Record{
				Ts:   now.Add(time.Duration(i) * time.Second).UnixMilli(),
				Kind: observeseclog.KindDrift, Agent: "doctor", Action: "warn",
			})
		}
	}

	// Over the cap: truncated in both renderings.
	dir := t.TempDir()
	write(dir, auditStatsLimit+1)
	opts := AuditOpts{Stats: true, JSON: true}
	out, err := RenderAudit(dir, opts, now)
	if err != nil {
		t.Fatalf("RenderAudit JSON: %v", err)
	}
	var stats AuditStats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatalf("parse stats JSON: %v\n%s", err, out)
	}
	if !stats.Truncated {
		t.Errorf("stats JSON missing \"truncated\": true:\n%s", out)
	}
	if stats.Total != auditStatsLimit {
		t.Errorf("stats.Total = %d, want %d (the retained newest prefix)", stats.Total, auditStatsLimit)
	}

	opts.JSON = false
	out, err = RenderAudit(dir, opts, now)
	if err != nil {
		t.Fatalf("RenderAudit table: %v", err)
	}
	if want := fmt.Sprintf("truncated at %d newest records", auditStatsLimit); !strings.Contains(out, want) {
		t.Errorf("stats table missing %q:\n%s", want, out)
	}

	// Below the cap: no truncation markers in either rendering.
	small := t.TempDir()
	write(small, 3)
	opts.JSON = true
	out, err = RenderAudit(small, opts, now)
	if err != nil {
		t.Fatalf("RenderAudit small JSON: %v", err)
	}
	if strings.Contains(out, "truncated") {
		t.Errorf("small stats JSON unexpectedly truncated:\n%s", out)
	}
	opts.JSON = false
	out, err = RenderAudit(small, opts, now)
	if err != nil {
		t.Fatalf("RenderAudit small table: %v", err)
	}
	if strings.Contains(out, "truncated") {
		t.Errorf("small stats table unexpectedly truncated:\n%s", out)
	}
}

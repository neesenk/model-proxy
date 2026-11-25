package app

// r6_usage_ledger_index_dir_test.go pins where the boot-time usage-ledger
// rebuild (observestats.Bootstrap → Store.RebuildTokenLedger) looks for the
// request-log index: the directory the logger and the tailing indexer write,
// i.e. RequestLogConfig.ResolvedDir(). The rebuild used to join "index.db"
// onto the RAW configured dir — empty when `dir` is not written — so it
// opened (and created) ./index.db relative to the process working directory
// and never saw the index the request log actually keeps.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	obscounters "model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/testsyn"
)

// TestInitStatsLedgerRebuildReadsRequestLogDir loads request_log from YAML
// (so "dir not written" is the loader's real value, not a hand-built Config),
// puts a REAL index — built by requestlog.Indexer over a real JSONL file —
// into the directory the request log resolves to, and requires the rebuild
// to rewrite the historical bucket from it. The working directory must stay
// clean: the rebuild only reads the index.
func TestInitStatsLedgerRebuildReadsRequestLogDir(t *testing.T) {
	// One provider satisfies the loader's validation; nothing here touches
	// providers or credentials — only the request_log block matters.
	const provider = "providers:\n  zhipu: {provider_id: zhipu, openai_base_url: https://x}\n"
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "dir not written: the default request-log directory",
			yaml: provider + "request_log:\n  enabled: true\n",
		},
		{
			name: "dir written with a ~ prefix",
			yaml: provider + "request_log:\n  enabled: true\n  dir: ~/reqlog\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			// os.UserHomeDir reads HOME (unix) or USERPROFILE (windows); both
			// are isolated so the default request-log directory is the test's.
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			workDir := t.TempDir()
			t.Chdir(workDir)

			cfg, err := configdomain.LoadConfigFromBytes("test", []byte(tc.yaml))
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			logDir := cfg.RequestLog.ResolvedDir()
			if !filepath.IsAbs(logDir) {
				t.Fatalf("request-log dir %q is not absolute — the test needs an isolated, predictable location", logDir)
			}
			indexPath := indexRequestLogDir(t, logDir)

			minute := time.Date(2026, 10, 9, 4, 33, 0, 0, time.UTC).Unix() // 2026-10-09T12:33+08:00, pre-cutoff
			key := observestats.Key{Provider: "deepseek", Model: "m"}
			statsPath := filepath.Join(t.TempDir(), "stats.db")
			seed, err := observestats.Open(observestats.Options{Path: statsPath})
			if err != nil {
				t.Fatalf("open seed store: %v", err)
			}
			// The committed pre-cutoff bucket carries the historical double
			// count (input inclusive of the cache hit) and the 4xx-inclusive
			// request count.
			if err := seed.Flush(minute, map[observestats.Key]observestats.Counters{
				key: {Requests: 4, Input: 500, Output: 90, CacheRead: 200, TokenRequests: 3},
			}); err != nil {
				_ = seed.Close()
				t.Fatalf("seed stats: %v", err)
			}
			if err := seed.Close(); err != nil {
				t.Fatalf("close seed store: %v", err)
			}

			proxy := &Proxy{
				processServices: processServices{
					metrics: obscounters.NewMetricsStore(),
					tokens:  obscounters.NewTokenCounter(),
					agents:  obscounters.NewAgentCounter(),
				},
			}
			proxy.cfg = cfg
			proxy.initStats(configdomain.StatsConfig{DBPath: statsPath, Retention: "0"})
			if proxy.stats == nil || proxy.flusher == nil {
				t.Fatal("initStats did not bind the durable store and runtime flusher")
			}
			t.Cleanup(func() { _ = proxy.stats.Close() })

			// The index ledger: three 200s of 100/10/66/5, the 400 uncounted.
			rows, err := proxy.stats.QueryRange(minute, minute, key.Provider, key.Model, 0)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows = %+v, %v; want exactly one for minute %d", rows, err, minute)
			}
			if got := rows[0]; got.Requests != 3 || got.Input != 300 || got.Output != 30 ||
				got.CacheRead != 198 || got.CacheCreation != 15 {
				t.Errorf("rebuilt bucket = {requests %d input %d output %d cache_read %d cache_creation %d}, want {3 300 30 198 15} from the index at %s",
					got.Requests, got.Input, got.Output, got.CacheRead, got.CacheCreation, indexPath)
			}
			// The in-memory baseline is seeded AFTER the rebuild, so the live
			// counters start from the corrected ledger.
			if got, want := proxy.tokens.Snapshot()[obscounters.PMKey{Provider: key.Provider, Model: key.Model}],
				(obscounters.TokenUsage{Input: 300, Output: 30, CacheRead: 198, CacheCreation: 15, Requests: 3}); got != want {
				t.Errorf("restored tokens = %+v, want %+v", got, want)
			}
			if _, err := os.Stat(filepath.Join(workDir, "index.db")); !os.IsNotExist(err) {
				t.Errorf("the rebuild left an index.db in the working directory (stat err: %v) — it must only read the request-log index at %q", err, indexPath)
			}
		})
	}
}

// indexRequestLogDir writes one request-log JSONL file (the Record encoding
// the logger writes) into dir, lets the real Indexer tail it in one pass,
// closes the index and returns the index.db path.
func indexRequestLogDir(t *testing.T, dir string) string {
	t.Helper()
	body := func() string {
		return `{"id":"msg_1","usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":66,"cache_creation_input_tokens":5}}`
	}
	var records []requestlog.Record
	for i, ts := range []string{"2026-10-09T04:33:10Z", "2026-10-09T04:33:20Z", "2026-10-09T04:33:30Z"} {
		records = append(records, requestlog.Record{
			Ts: ts, RequestID: "live-" + string(rune('a'+i)), Protocol: "anthropic", Method: "POST", Path: "/v1/messages",
			CalledModel: "m", UpstreamModel: "m", Exposed: "m", Provider: "deepseek", Agent: "claude-code",
			Attempt: 1, Status: 200, ResponseSize: 90, RequestBody: `{}`, ResponseBody: body(),
		})
	}
	records = append(records, requestlog.Record{
		Ts: "2026-10-09T04:33:40Z", RequestID: "live-400", Protocol: "anthropic", Method: "POST", Path: "/v1/messages",
		CalledModel: "m", UpstreamModel: "m", Exposed: "m", Provider: "deepseek", Agent: "claude-code",
		Attempt: 1, Status: 400, ResponseSize: 24, RequestBody: `{}`, ResponseBody: `{"error":"bad request"}`,
	})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "requests-20261009-043310.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for i := range records {
		if err := encoder.Encode(&records[i]); err != nil {
			_ = file.Close()
			t.Fatalf("encode record %d: %v", i, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	// The Indexer's Run loop is the one background owner this helper starts;
	// it must be gone again before it returns.
	goroutines := testsyn.Goroutines()
	indexer, err := requestlog.NewIndexer(dir)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		indexer.Run()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := indexer.WaitReconciled(ctx); err != nil {
		t.Fatalf("first index pass: %v", err)
	}
	// The index must hold the records before the rebuild reads it — otherwise
	// the assertions above would pass or fail for the wrong reason.
	summaries, _, err := indexer.SummariesWithFacets(requestlog.Filter{Limit: 10})
	if err != nil || len(summaries) != len(records) {
		t.Fatalf("index holds %d summaries (%v), want %d — the harness did not index the records", len(summaries), err, len(records))
	}
	indexer.Shutdown()
	<-runDone
	testsyn.LeakCheck(t, goroutines, "indexer Run loop")
	return filepath.Join(dir, "index.db")
}

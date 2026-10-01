package archtest

// timing_discipline_contract_test.go — 时间不得作为同步原语的可执行契约。
//
// 政策权威在 docs/engineering/testing.md §「时序纪律契约」；本文件是执行。
// 三条规则：
//  1. 生产代码的 time.Sleep 引用（含作为函数值传递）精确登记——Sleep 只允许
//     有界退避/节流，顺序必须来自同步原语；新增即 FAIL，直到在下方登记并
//     写明分类理由。
//  2. 生产代码的 .ModTime 引用精确登记——mtime 是文件状态的观察值，不是
//     互斥依据；凡「看起来陈旧就破坏互斥」的恢复路径必须有存活证据
//     （pitfalls #40：账号池锁的 PID 探活先例）。新增即 FAIL。
//  3. 测试代码的 time.Sleep 按 (file, top-level func) 登记允许数量——数量
//     增加即 FAIL（先改成确定性同步：channel/barrier/轮询可观察量/决策相对
//     窗口；若 Sleep 本身是被测现象或时钟输入，更新本契约并注明分类）；
//     数量减少同样 FAIL（删干净后同步收紧快照，防止基线漂移）。
//
// 本契约是快照门禁（同 scripts/cover.sh 的 floor 模式）：不判断既有 site 的
// 合法性（那由 2026-10-01 的全仓时序审计逐处完成），只保证任何变化都显式过
// 本文件。采集器有合成正/负自控（architecture_ast_contract 的 guard 惯例）。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// timingRef is one reference to a watched timing primitive, attributed to its
// enclosing top-level declaration.
type timingRef struct {
	File string // module-root-relative path
	Func string // enclosing top-level func name
}

// prodSleepAllowlist: file → func → allowed time.Sleep references. Classify:
// supervisor.waitForExit — pacing poll over process-exit signals (arbiter is
// the signal, not the sleep); store.WithLock — passes time.Sleep as the
// withLock wait seam (the arbiter is O_EXCL).
var prodSleepAllowlist = map[string]map[string]int{
	"internal/accounts/store.go":       {"WithLock": 1},
	"internal/cli/serve/supervisor.go": {"waitForProcessExit": 1},
	"scripts/e2eguard/main.go":         {"poll": 1},
}

// prodModTimeAllowlist: file → func → allowed .ModTime references. Each entry
// is an mtime OBSERVATION with its coordination story:
//   - modelcaps persist/adopt + noteModelCapsFileState: guarded by
//     modelCapsPersistMu (external-write detection, pitfalls #39);
//   - catalog disccache memo key (leaf mutex, not a lock);
//   - cli/models check conflict re-stat (cross-process, pitfalls #33);
//   - accounts pool-lock staleness — now backed by the PID liveness probe
//     (pitfalls #40: mtime alone must not break the mutex);
//   - requestlog index fileState columns (SQLite-serialized);
//   - stats legacy import minute floor (boot-only, single goroutine);
//   - logfile retention cutoff (single writer goroutine).
var prodModTimeAllowlist = map[string]map[string]int{
	"internal/app/modelcaps.go": {
		"(*Proxy).persistModelCapsNow":    1,
		"(*Proxy).noteModelCapsFileState": 1,
	},
	"internal/catalog/disccache.go":        {"(*DiskCache).Load": 2},
	"internal/cli/models/check.go":         {"persistModelCaps": 2},
	"internal/accounts/store.go":           {"Store.withLock": 1},
	"internal/observe/requestlog/index.go": {"(*Indexer).reconcile": 1},
	"internal/observe/stats/legacy.go":     {"(*Store).ImportLegacyTokens": 1},
	"internal/observe/logfile/logfile.go":  {"(*Logger).sweep": 1},
}

// testSleepSnapshot: file → top-level func → allowed time.Sleep references.
// Every entry was individually classified by the 2026-10-01 timing audit
// (poll backoff / measured phenomenon / clock input / yield) — see
// docs/engineering/testing.md §「时序纪律契约」 for the classes.
var testSleepSnapshot = map[string]map[string]int{
	"internal/accounts/store_test.go":                     {"TestWithLockHeartbeatKeepsAgedLockFromBeingStolen": 3, "TestWithPoolLock_MutualExclusion": 1, "TestWithPoolLock_StaleButAliveHolderIsNotStolen": 1},
	"internal/adjudicate/adjudicate_test.go":              {"TestBlockedContentIndex_Semantics": 1, "TestLLMUsageStats_CountsCallsNotCacheHits": 1, "waitFor": 1},
	"internal/app/forward_retry_test.go":                  {"TestUC_ClientCancelDuringHeadersStopsFailoverAndKeepsCircuitClosed": 2},
	"internal/app/forward_test.go":                        {"TestForward_CommittedSSEStreamMidFailureIsNotRecalled": 1},
	"internal/app/fusion_test.go":                         {"awaitFusionState": 1, "delayedResponder": 1, "hitScript": 1},
	"internal/app/guard_adjudication_integration_test.go": {"TestGuardAdjudication_HighVerdictBlocksAndUnblocks": 1, "TestGuardAdjudication_RepeatHighContentIntercepted": 1, "waitForAuditVerdict": 1, "waitForBlock": 1},
	"internal/app/health_test.go":                         {"TestStickyDwell_HoldsThenReEvaluates": 1},
	"internal/app/lifecycle_test.go":                      {"TestProxyRequestLogIndexLifecycle": 1},
	"internal/app/metrics_test.go":                        {"TestForward_RecordsLatency": 1},
	"internal/app/model_lock_test.go":                     {"TestEmpty200_ClientCancelNoLock": 2},
	"internal/app/proxy_integration_test.go":              {"TestUC_ClientDisconnectStopsUpstream": 1, "waitUntil": 1},
	"internal/app/proxy_mcp_route_test.go":                {"TestMCPRoute_RequestLogProjection": 1, "TestMCPRoute_ToolsCallStatsCreditBackend": 1, "serve": 1},
	"internal/app/proxy_mcp_test.go":                      {"TestMCPGateway_LegacySSE": 1, "TestMCPGateway_LiveEndCarriesTool": 1, "TestMCPGateway_LiveEventAttribution": 1, "TestMCPGateway_LiveEvents": 1, "TestMCPGateway_LiveProviderIsServingAccount": 1, "TestMCPGateway_RequestLogAttribution": 2, "TestMCPGateway_RequestLogKind": 1, "TestMCPGateway_RequestLogSplitStream": 1, "TestMCPGateway_StatsSurfaced": 1},
	"internal/app/proxy_race_test.go":                     {"TestForward_CfgReadNoRaceWithReload": 2},
	"internal/app/quota_async_test.go":                    {"TestBudgetWatcher_EndToEndAlertThroughPorts": 1},
	"internal/app/quota_test.go":                          {"Quota": 1, "TestQuotaTracker_PollAfter": 1},
	"internal/app/request_log_test.go":                    {"TestForward_RequestLog_CapturesAgent": 1, "TestRequestLog_SmallCapTruncatesBodiesEndToEnd": 1},
	"internal/app/shadow_test.go":                         {"TestReload_ShadowDisabledStopsFiring": 1, "TestShadow_LogsResult": 1, "TestShadow_PooledProvider": 1},
	"internal/app/state_isolation_test.go":                {"TestReloadRefreshesCatalogThroughLoader": 1},
	"internal/app/web_accounts_test.go":                   {"waitForLoginDone": 1, "waitForLoginTerminal": 1},
	"internal/app/wirecap_test.go":                        {"TestWireCap_ProbePassesSerialized": 2, "TestWireCap_ProbeTimeoutUnknown": 1, "TestWireProbePass_CoalescesStormDispatches": 2},
	"internal/forward/forward_test.go":                    {"TestServeClientGoneDuringCooldownWait": 1},
	"internal/forward/fusion_select_test.go":              {"TestCallFusionSelectorFailureClasses": 1},
	"internal/forward/fusion_test.go":                     {"TestCallFusionLegGates": 2},
	"internal/observe/budget/watcher_test.go":             {"TestWatcher_LoopChecksAtStartThenEachMinute": 1},
	"internal/observe/events/hub_ctx_test.go":             {"TestHubPublishSubscribeExactlyOnce": 1, "TestSubscribeContextAutoReleases": 1},
	"internal/observe/events/progress_test.go":            {"TestProgressReaderPublishesOnThrottleTime": 1},
	"internal/observe/events/sse_test.go":                 {"waitFor": 1},
	"internal/probe/probe_test.go":                        {"TestExchangeMeasuresLatency": 1},
	"internal/probe/protocols_test.go":                    {"TestProbeModelsBatchOrderAndConcurrency": 1},
	"internal/runtime/quota_lifecycle_test.go":            {"pollFor": 1},
	"scripts/restartserve/restart_e2e_test.go":            {"waitFor": 1, "waitGone": 1},
	"scripts/soak/one_request_test.go":                    {"TestOneRequestStreamMeasuresTTFT": 1},
}

// collectTimingRefs parses one file and returns every watched reference,
// attributed to its enclosing top-level declaration. Anonymous function
// literals attribute to the top-level func they nest in (granularity of the
// snapshot). A parse failure is reported by the caller.
func collectTimingRefs(f *ast.File) (sleeps, modtimes []timingRef) {
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "time" && sel.Sel.Name == "Sleep" {
					sleeps = append(sleeps, timingRef{Func: fd.Name.Name})
				}
				if sel.Sel.Name == "ModTime" {
					modtimes = append(modtimes, timingRef{Func: methodOwnerName(fd)})
				}
			}
			return true
		})
	}
	return sleeps, modtimes
}

// methodOwnerName renders a FuncDecl's name including its receiver type
// marker (e.g. "(*Store).withLock") so allowlist keys stay unambiguous
// between functions and methods with the same name.
func methodOwnerName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := ""
	switch typ := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := typ.X.(*ast.Ident); ok {
			recv = "(*" + id.Name + ")"
		}
	case *ast.Ident:
		recv = typ.Name
	}
	if recv == "" {
		return fd.Name.Name
	}
	return recv + "." + fd.Name.Name
}

// walkTimingRefs walks the module root and feeds every Go file's refs to
// visit (which returns whether to keep the file's classification).
func walkTimingRefs(t testing.TB, root string, visit func(rel string, isTest bool, sleeps, modtimes []timingRef)) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			switch info.Name() {
			case "testdata", ".git", "jstests", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		sleeps, modtimes := collectTimingRefs(f)
		visit(filepath.ToSlash(rel), strings.HasSuffix(path, "_test.go"), sleeps, modtimes)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

func TestTimingDisciplineContract(t *testing.T) {
	root := repoRoot(t)
	if os.Getenv("MP_TIMING_REGEN") != "" {
		regenTimingSnapshot(t, root)
		t.Skip("regeneration mode: printed the current inventory")
	}
	var viol []string

	countRefs := func(refs []timingRef) map[string]int {
		out := map[string]int{}
		for _, r := range refs {
			out[r.Func]++
		}
		return out
	}

	walkTimingRefs(t, root, func(rel string, isTest bool, sleeps, modtimes []timingRef) {
		if !isTest {
			// Rule 1 & 2: production sites must be registered exactly.
			for _, pair := range []struct {
				kind  string
				refs  []timingRef
				allow map[string]map[string]int
			}{
				{"time.Sleep", sleeps, prodSleepAllowlist},
				{".ModTime", modtimes, prodModTimeAllowlist},
			} {
				got := countRefs(pair.refs)
				allowed := pair.allow[rel]
				for fn, n := range got {
					if allowed[fn] != n {
						viol = append(viol, fmt.Sprintf(
							"production %s: %s → %s has %d reference(s), contract registers %d — timing must not order concurrent access (pacing/backoff only, and mtime is an observation, not a mutex; see docs/engineering/testing.md 时序纪律契约 and pitfalls #39/#40). Justify and register, or replace with a synchronization primitive.",
							pair.kind, rel, fn, n, allowed[fn]))
					}
				}
				for fn := range allowed {
					if got[fn] == 0 {
						viol = append(viol, fmt.Sprintf(
							"production %s: contract registers %s → %s but the code no longer matches — tighten the allowlist", pair.kind, rel, fn))
					}
				}
			}
			return
		}
		// Rule 3: test Sleep counts per (file, top-level func) are a snapshot.
		got := countRefs(sleeps)
		snap := testSleepSnapshot[rel]
		for fn, n := range got {
			if snap == nil {
				viol = append(viol, fmt.Sprintf(
					"test time.Sleep: new file %s (%s) — synchronize deterministically (channel/barrier/poll an observable/decision-relative window); if the sleep IS the measured phenomenon or a clock input, snapshot it here with a classification comment",
					rel, fn))
				continue
			}
			if snap[fn] != n {
				viol = append(viol, fmt.Sprintf(
					"test time.Sleep: %s → %s has %d reference(s), snapshot allows %d — added sleeps must be deterministic sync or an audited classification; removed sleeps must tighten the snapshot",
					rel, fn, n, snap[fn]))
			}
		}
		for fn, n := range snap {
			if n > 0 && got[fn] == 0 {
				viol = append(viol, fmt.Sprintf(
					"test time.Sleep: snapshot allows %s → %s but none remain — tighten the snapshot", rel, fn))
			}
		}
	})

	sort.Strings(viol)
	for _, v := range viol {
		t.Error(v)
	}
	if len(viol) > 0 {
		t.Errorf("timing-discipline contract violations: %d (policy: docs/engineering/testing.md §时序纪律契约)", len(viol))
	}
}

// TestTimingDisciplineCollectorControls: synthetic positive/negative controls
// proving the collector attributes references per top-level function (method
// receivers included) and that the contract logic would flag an unregistered
// site — the guard guards the guard (architecture_ast_contract convention).
func TestTimingDisciplineCollectorControls(t *testing.T) {
	src := `package p
import ("os"; "time")
func prodPace() { time.Sleep(time.Millisecond) }
func (*T) prodObserve(fi os.FileInfo) bool { return fi.ModTime().IsZero() }
func helper() { _ = time.Sleep }
func TestX(t *testing.T) { time.Sleep(1); time.Sleep(2) }
`
	dir := t.TempDir()
	path := filepath.Join(dir, "sample_test.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sleeps, modtimes := collectTimingRefs(f)

	count := func(refs []timingRef, fn string) int {
		n := 0
		for _, r := range refs {
			if r.Func == fn {
				n++
			}
		}
		return n
	}
	// Positive: every reference attributed, method receiver qualified, the
	// function-VALUE form of time.Sleep counted too.
	if got := count(sleeps, "prodPace"); got != 1 {
		t.Errorf("prodPace sleeps = %d, want 1", got)
	}
	if got := count(modtimes, "(*T).prodObserve"); got != 1 {
		t.Errorf("(*T).prodObserve ModTime = %d, want 1 (receiver-qualified)", got)
	}
	if got := count(sleeps, "helper"); got != 1 {
		t.Errorf("helper (function-value time.Sleep) = %d, want 1", got)
	}
	if got := count(sleeps, "TestX"); got != 2 {
		t.Errorf("TestX sleeps = %d, want 2 (closure granularity: top-level func)", got)
	}
}

// regenTimingSnapshot prints the CURRENT inventory as Go map literals — run
// with MP_TIMING_REGEN=1 go test ./internal/archtest -run TestTimingDisciplineContract
// after an intentional timing change, review the diff, and paste it into the
// allowlists above (each new entry needs a classification comment).
func regenTimingSnapshot(t *testing.T, root string) {
	t.Helper()
	prodSleep := map[string]map[string]int{}
	prodModTime := map[string]map[string]int{}
	testSleep := map[string]map[string]int{}
	bump := func(m map[string]map[string]int, file, fn string) {
		if m[file] == nil {
			m[file] = map[string]int{}
		}
		m[file][fn]++
	}
	walkTimingRefs(t, root, func(rel string, isTest bool, sleeps, modtimes []timingRef) {
		if isTest {
			for _, r := range sleeps {
				bump(testSleep, rel, r.Func)
			}
			return
		}
		for _, r := range sleeps {
			bump(prodSleep, rel, r.Func)
		}
		for _, r := range modtimes {
			bump(prodModTime, rel, r.Func)
		}
	})
	dump := func(name string, m map[string]map[string]int) {
		fmt.Printf("\nvar %s = map[string]map[string]int{\n", name)
		files := make([]string, 0, len(m))
		for f := range m {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			fns := make([]string, 0, len(m[f]))
			for fn := range m[f] {
				fns = append(fns, fn)
			}
			sort.Strings(fns)
			var parts []string
			for _, fn := range fns {
				parts = append(parts, fmt.Sprintf("%q: %d", fn, m[f][fn]))
			}
			fmt.Printf("\t%q: {%s},\n", f, strings.Join(parts, ", "))
		}
		fmt.Println("}")
	}
	dump("prodSleepAllowlist", prodSleep)
	dump("prodModTimeAllowlist", prodModTime)
	dump("testSleepSnapshot", testSleep)
}

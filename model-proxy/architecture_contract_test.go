package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Architecture boundary contracts, checked on the parsed AST (selector/call
// semantics) instead of string matching:
//   - webServer owns only the concrete proxyReadView/proxyAdminCommands
//     capabilities, never *Proxy. web.go may use only an explicit allowlist of
//     methods on those ports, and every direct w.p selector is forbidden.
//     Background work must enter through webTaskOwner rather than bare `go`.
//   - fusion.go must not call provider/protocol/conversion helpers directly —
//     Fusion shares targetPlan with normal routes instead of duplicating
//     provider lookup, protocol selection or request conversion.
//   - daemon.go must not start Proxy-level background tasks directly — they
//     are owned by proxyLifecycle (startRuntimeServices / Proxy.Close).
//
// Being AST-based, comments and string literals can no longer false-positive,
// and only actual selector/call expressions are judged. Known limits (accepted,
// documented so future readers don't mistake this for a proof): there is no
// type resolution, so an alias obtained other than by direct assignment
// (function return `q := getP(w)`, parameter, struct field) is not tracked,
// and same-named fields/methods on unrelated types would also match (none
// exist today — the rules below encode exactly the tokens the old string test
// forbade).
func TestArchitectureBoundaries(t *testing.T) {
	t.Run("web.go depends only on explicit Proxy capabilities", func(t *testing.T) {
		f, fset := parseGoFile(t, "web.go")
		fields := namedStructFields(t, f, "webServer")
		if got := simpleTypeName(fields["reads"]); got != "proxyReadView" {
			t.Errorf("webServer.reads type = %q, want proxyReadView", got)
		}
		if got := simpleTypeName(fields["admin"]); got != "proxyAdminCommands" {
			t.Errorf("webServer.admin type = %q, want proxyAdminCommands", got)
		}
		if got := simpleTypeName(fields["tasks"]); got != "*webTaskOwner" {
			t.Errorf("webServer.tasks type = %q, want *webTaskOwner", got)
		}
		for name, typ := range fields {
			if typeContainsIdent(typ, "Proxy") {
				t.Errorf("webServer.%s must not retain *Proxy", name)
			}
		}
		for _, v := range directSelectorSites(f, fset, "w", "p") {
			t.Errorf("web.go retains forbidden direct Proxy access: %s", v)
		}

		allowed := map[string]map[string]bool{
			"reads": {
				"agentStats": true, "analytics": true, "config": true,
				"dashboard": true, "fusion": true, "logFile": true,
				"pins": true, "pricing": true, "providerConfig": true,
				"providerConfigs": true, "requestLogDirectory": true,
				"stats": true, "tokenUsage": true,
			},
			"admin": {
				"accountProbe": true, "clearPin": true, "refreshQuota": true,
				"reload": true, "resetHealthAndPersist": true,
				"resetStats": true, "setPin": true,
			},
		}
		for _, v := range unexpectedPortAccesses(f, fset, "w", allowed) {
			t.Errorf("web.go uses a capability outside the allowlist: %s", v)
		}
		if got := goStatementCount(f); got != 0 {
			t.Errorf("web.go starts %d bare goroutine(s); use webTaskOwner.run", got)
		}
	})

	t.Run("account probe captures one runtime generation", func(t *testing.T) {
		admin, _ := parseGoFile(t, "proxy_admin_commands.go")
		if got := methodCallCount(admin, "accountProbe", "snapshotRuntime"); got != 1 {
			t.Errorf("proxyAdminCommands.accountProbe snapshotRuntime calls = %d, want exactly 1", got)
		}
		readView, _ := parseGoFile(t, "proxy_read_view.go")
		if methodDeclared(readView, "runtimeProvider") {
			t.Error("proxyReadView.runtimeProvider must not exist; account probes use one runtimeSnapshot")
		}
	})

	t.Run("fusion.go reuses targetPlan helpers instead of duplicating them", func(t *testing.T) {
		f, fset := parseGoFile(t, "fusion.go")
		for _, spec := range f.Imports {
			if strings.Trim(spec.Path.Value, `"`) == "model-proxy/internal/protocol" {
				t.Error("fusion.go must not import internal/protocol directly; use targetPlan adapters")
			}
		}
		forbiddenCalls := map[string]bool{
			"providerConfig": true, "resolvedBackendProto": true,
			"convertRequestFor": true, "catalogSnapshot": true,
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, nil) {
			t.Errorf("fusion.go bypasses targetPlan: %s", v)
		}
	})

	t.Run("daemon.go leaves Proxy background tasks to proxyLifecycle", func(t *testing.T) {
		f, fset := parseGoFile(t, "daemon.go")
		forbiddenCalls := map[string]bool{
			"initStats": true, "initRequestLog": true, "statsFlushLoop": true,
		}
		// Chained internal components: <x>.reqLog.loop(), <x>.flusher.flush().
		forbiddenChains := [][2]string{
			{"reqLog", "loop"}, {"reqLog", "shutdown"}, {"flusher", "flush"},
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, forbiddenChains) {
			t.Errorf("daemon.go bypasses proxyLifecycle: %s", v)
		}
	})

	t.Run("internal pricing remains a repository-leaf package", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/pricing")
	})

	t.Run("internal protocol owns conversion and remains a repository-leaf package", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/protocol")
		for _, legacy := range []string{
			"conversion_registry.go",
			"convert.go",
			"convert_capabilities.go",
			"convert_citations.go",
			"convert_custom_tool.go",
			"convert_namespace.go",
			"convert_reasoning_replay.go",
			"convert_responses.go",
			"convert_responses_stream.go",
			"image_guard.go",
			"responses_state.go",
			"stream_mode.go",
		} {
			if _, err := os.Stat(legacy); err == nil {
				t.Errorf("%s must live under internal/protocol, not the root package", legacy)
			} else if !os.IsNotExist(err) {
				t.Fatalf("stat %s: %v", legacy, err)
			}
		}
	})
}

func assertRepositoryLeafPackage(t *testing.T, directory string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("%s has no Go files", directory)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, _ := parseGoFile(t, path)
		for _, spec := range f.Imports {
			importPath := strings.Trim(spec.Path.Value, `"`)
			if strings.HasPrefix(importPath, "model-proxy/") {
				t.Errorf("%s imports repository package %q; %s must remain a leaf",
					filepath.Base(path), importPath, directory)
			}
		}
	}
}

// TestTargetExecutionArchitecture protects the next layer below targetPlan:
// targetAttempt is a deliberately small data contract, the executor has no
// composition-root escape hatch, Fusion's client-facing synthesis reuses that
// executor, and pool resolution reaches health only through resolverState.
func TestTargetExecutionArchitecture(t *testing.T) {
	t.Run("targetAttempt has only its five execution contract fields", func(t *testing.T) {
		f, _ := parseGoFile(t, "dispatch_context.go")
		fields := namedStructFields(t, f, "targetAttempt")
		want := map[string]bool{
			"runtime": true, "plan": true, "exchange": true, "scope": true, "policy": true,
		}
		if len(fields) != len(want) {
			t.Errorf("targetAttempt fields = %v, want exactly %v", sortedFieldNames(fields), sortedBoolNames(want))
		}
		for name := range fields {
			if !want[name] {
				t.Errorf("targetAttempt must not retain %q outside its five-field contract", name)
			}
		}
		for name := range want {
			if _, ok := fields[name]; !ok {
				t.Errorf("targetAttempt missing required contract field %q", name)
			}
		}
	})

	t.Run("attempt groups keep exact fields and no owner duplication", func(t *testing.T) {
		f, _ := parseGoFile(t, "dispatch_context.go")
		contracts := []struct {
			name string
			want map[string]bool
		}{
			{
				name: "attemptExchange",
				want: map[string]bool{"request": true, "writer": true, "body": true},
			},
			{
				name: "attemptScope",
				want: map[string]bool{
					"calledModel": true, "agent": true, "cacheKey": true, "log": true,
					"responseContext": true, "responsesHistory": true, "responsesSession": true,
				},
			},
			{
				name: "attemptPolicy",
				want: map[string]bool{"force": true, "lastTarget": true, "contextRetry": true},
			},
		}
		forbiddenTypes := map[string]bool{
			"Proxy": true, "Config": true, "responseCache": true,
			"runtimeSnapshot": true, "targetPlan": true,
		}
		for _, contract := range contracts {
			if got := structContractViolations(namedStructFields(t, f, contract.name), contract.want, forbiddenTypes); len(got) != 0 {
				t.Errorf("%s contract violations: %v", contract.name, got)
			}
		}
	})

	t.Run("attemptCommit exposes only the post-commit request body", func(t *testing.T) {
		f, _ := parseGoFile(t, "attempt_executor.go")
		want := map[string]bool{"requestBody": true}
		forbiddenTypes := map[string]bool{
			"Proxy": true, "Config": true, "responseCache": true,
			"runtimeSnapshot": true, "targetPlan": true,
			"proxyLifecycle": true, "shadowRuntime": true,
		}
		if got := structContractViolations(namedStructFields(t, f, "attemptCommit"), want, forbiddenTypes); len(got) != 0 {
			t.Errorf("attemptCommit contract violations: %v", got)
		}
	})

	t.Run("only newTargetAttempt may construct targetAttempt", func(t *testing.T) {
		factoryCount := 0
		literalSites := []string{}
		for _, path := range productionGoFiles(t) {
			f, fset := parseGoFile(t, path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if fn.Name.Name == "newTargetAttempt" {
					factoryCount++
				}
				for _, site := range compositeLiteralSites(fn.Body, "targetAttempt") {
					if fn.Name.Name != "newTargetAttempt" {
						literalSites = append(literalSites, describe(fset, site, fn.Name.Name+" constructs targetAttempt"))
					}
				}
			}
		}
		if factoryCount != 1 {
			t.Errorf("newTargetAttempt declarations = %d, want exactly 1", factoryCount)
		}
		if len(literalSites) != 0 {
			t.Errorf("targetAttempt composite literals must be confined to newTargetAttempt: %v", literalSites)
		}
		proxyFile, proxySet := parseGoFile(t, "proxy.go")
		if got := compositeLiteralSites(proxyFile, "targetAttempt"); len(got) != 0 {
			t.Errorf("proxy.go must call newTargetAttempt, not construct targetAttempt: %s", describeNodes(proxySet, got, "targetAttempt literal"))
		}
		serveOnce := namedMethod(t, proxyFile, "Proxy", "serveOnce")
		if !assignedFactoryValueExecuted(serveOnce.Body, "newTargetAttempt", "targetExecutor", "execute") {
			t.Error("Proxy.serveOnce must pass the attempt assigned from newTargetAttempt to targetExecutor().execute")
		}
		executePos := firstNamedCallPos(serveOnce.Body, "execute")
		shadowPos := firstNamedCallPos(serveOnce.Body, "dispatchShadowAfterCommit")
		if !executePos.IsValid() || !shadowPos.IsValid() || shadowPos <= executePos {
			t.Errorf("Proxy.serveOnce must dispatch post-commit Shadow after execute (execute=%v shadow=%v)", executePos, shadowPos)
		}

		fusionFile, fusionSet := parseGoFile(t, "fusion.go")
		if got := compositeLiteralSites(fusionFile, "targetAttempt"); len(got) != 0 {
			t.Errorf("fusion.go must call newTargetAttempt, not construct targetAttempt: %s", describeNodes(fusionSet, got, "targetAttempt literal"))
		}
		synth := namedMethod(t, fusionFile, "Proxy", "callFusionSynthesizer")
		if !assignedFactoryValueExecuted(synth.Body, "newTargetAttempt", "targetExecutor", "execute") {
			t.Error("callFusionSynthesizer must pass the attempt assigned from newTargetAttempt to targetExecutor().execute")
		}
	})

	t.Run("attemptExecutor cannot regain Proxy scheduling reload or Web access", func(t *testing.T) {
		f, fset := parseGoFile(t, "attempt_executor.go")
		for name, typ := range namedStructFields(t, f, "attemptExecutor") {
			if typeContainsIdent(typ, "Proxy") {
				t.Errorf("attemptExecutor.%s retains forbidden Proxy dependency", name)
			}
		}
		execute := namedMethod(t, f, "attemptExecutor", "execute")
		if functionSignatureContainsIdent(execute, "Proxy") {
			t.Error("attemptExecutor.execute must not receive *Proxy")
		}
		forbidden := map[string]bool{
			"schedule": true, "reload": true, "snapshotRuntime": true,
			"newWebServer": true, "serveHTTPUntilShutdown": true,
			"startRuntimeServices": true, "contextOverflowRetry": true,
			"runShadow": true, "dispatchShadowAfterCommit": true,
			"shouldSample": true, "runBeforeLogDrain": true,
		}
		if got := forbiddenCallSites(f, fset, forbidden, nil); len(got) != 0 {
			t.Errorf("attempt_executor.go crossed orchestration boundary: %v", got)
		}
		forbiddenTypes := map[string]bool{
			"proxyLifecycle": true, "shadowRuntime": true, "ShadowTarget": true,
		}
		if got := forbiddenIdentifierSites(f, fset, forbiddenTypes); len(got) != 0 {
			t.Errorf("attempt_executor.go retained forbidden owner/orchestration types: %v", got)
		}
		proxyMethods := receiverMethodNames(t, productionGoFiles(t), "Proxy")
		targetExecutor := namedMethod(t, f, "Proxy", "targetExecutor")
		if got := methodValueInjections(targetExecutor.Body, "attemptExecutor", "p", proxyMethods); len(got) != 0 {
			t.Errorf("targetExecutor injects Proxy method values into attemptExecutor: %v", describeExprNodes(fset, got, "Proxy method value"))
		}
	})

	t.Run("Fusion synthesizer delegates client delivery to target executor", func(t *testing.T) {
		f, fset := parseGoFile(t, "fusion.go")
		synth := namedMethod(t, f, "Proxy", "callFusionSynthesizer")
		if !assignedFactoryValueExecuted(synth.Body, "newTargetAttempt", "targetExecutor", "execute") {
			t.Error("callFusionSynthesizer must execute the exact value returned by newTargetAttempt")
		}
		if got := namedCallCountInNode(synth.Body, "Do"); got != 0 {
			t.Errorf("callFusionSynthesizer must not add a parallel client.Do pipeline: found %d call(s)", got)
		}
		forbiddenShadow := map[string]bool{"runShadow": true, "dispatchShadowAfterCommit": true}
		if got := forbiddenCallSites(synth.Body, fset, forbiddenShadow, nil); len(got) != 0 {
			t.Errorf("callFusionSynthesizer must not dispatch Shadow: %v", got)
		}
	})

	t.Run("resolver depends on resolverState instead of Proxy", func(t *testing.T) {
		f, _ := parseGoFile(t, "resolve.go")
		fields := namedStructFields(t, f, "resolver")
		if got := simpleTypeName(fields["state"]); got != "resolverState" {
			t.Errorf("resolver.state type = %q, want resolverState", got)
		}
		for name, typ := range fields {
			if typeContainsIdent(typ, "Proxy") {
				t.Errorf("resolver.%s retains forbidden Proxy dependency", name)
			}
		}
		factory := namedFunction(t, f, "newResolver")
		if functionSignatureContainsIdent(factory, "Proxy") {
			t.Error("newResolver must receive resolverState, not *Proxy")
		}
	})
}

// TestArchitectureBoundaryChecker is the positive control for the AST rules
// above: the real checks pass vacuously when the tree is clean, so feed the
// checkers synthetic sources that violate each rule (including the `q := w.p`
// alias the old string test missed, and a comment-only mention that must NOT
// fire).
func TestArchitectureBoundaryChecker(t *testing.T) {
	parse := func(src string) (*ast.File, *token.FileSet) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "synthetic.go", "package main\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f, fset
	}

	// w.p.mu: comment must not fire; direct chain and alias must.
	f, fset := parse(`func h() {
	// w.p.mu in a comment is fine
	q := w.p
	q.mu.RLock()
	w.p.health.Lock()
	_ = w.p.readView()
}`)
	fields := map[string]bool{"mu": true, "health": true}
	got := forbiddenFieldAccesses(f, fset, fields, directAliases(f, "p"))
	if len(got) != 2 {
		t.Errorf("field check: got %v, want exactly the q.mu and w.p.health accesses", got)
	}

	// The Web capability checks must reject a retained *Proxy, direct w.p, and
	// an unreviewed port method while accepting an allowlisted method.
	f, fset = parse(`type webServer struct {
	reads proxyReadView
	admin proxyAdminCommands
	backend *Proxy
}
func (w *webServer) h() {
	_ = w.p
	w.reads.dashboard(now)
	w.admin.unreviewed()
}`)
	webFields := namedStructFields(t, f, "webServer")
	if !typeContainsIdent(webFields["backend"], "Proxy") {
		t.Error("webServer *Proxy field positive control did not fire")
	}
	if got := directSelectorSites(f, fset, "w", "p"); len(got) != 1 {
		t.Errorf("direct w.p check: got %v, want exactly one access", got)
	}
	allowed := map[string]map[string]bool{
		"reads": {"dashboard": true},
		"admin": {"reload": true},
	}
	if got := unexpectedPortAccesses(f, fset, "w", allowed); len(got) != 1 {
		t.Errorf("port allowlist check: got %v, want only admin.unreviewed", got)
	}
	f, _ = parse(`func h() { go unowned() }`)
	if got := goStatementCount(f); got != 1 {
		t.Errorf("bare goroutine check: got %d, want 1", got)
	}

	// Call rules: plain and receiver calls fire; comments don't.
	f, fset = parse(`func h() {
	// providerConfig( in a comment is fine
	providerConfig(x)
	p.convertRequestFor(y)
}`)
	gotCalls := forbiddenCallSites(f, fset, map[string]bool{"providerConfig": true, "convertRequestFor": true}, nil)
	if len(gotCalls) != 2 {
		t.Errorf("call check: got %v, want both call sites", gotCalls)
	}

	// Chained rules: p.reqLog.loop() fires, p.reqLog.flush() does not.
	f, fset = parse(`func h() {
	p.reqLog.loop(ctx)
	p.reqLog.flush()
}`)
	gotCalls = forbiddenCallSites(f, fset, nil, [][2]string{{"reqLog", "loop"}})
	if len(gotCalls) != 1 {
		t.Errorf("chain check: got %v, want only reqLog.loop", gotCalls)
	}

	// Target execution checks: each synthetic regression must be detectable so
	// the production checks above cannot pass merely because a matcher is wrong.
	f, fset = parse(`type targetAttempt struct { runtime int; extra int }
type attemptExchange struct { request int; writer int; body []byte; owner *Proxy }
type attemptScope struct {
	calledModel string; agent string; cacheKey string; log int
	responseContext int; responsesHistory []any; responsesSession string
	cfg *Config
}
	type attemptPolicy struct { force bool; lastTarget bool; contextRetry func(); cache *responseCache }
	type attemptCommit struct { requestBody []byte; runtime runtimeSnapshot }
	func bad() { _ = targetAttempt{} }
func newTargetAttempt() targetAttempt { return targetAttempt{} }
	type attemptExecutor struct { proxy *Proxy; lifecycle *proxyLifecycle; hook func(); hook2 func() }
func (p attemptExecutor) execute(root *Proxy) {
	root.reload()
	p.lifecycle.runBeforeLogDrain(nil)
}
	func (p *Proxy) runShadow() {}
	func (p *Proxy) targetExecutor() attemptExecutor {
		q := p
		return attemptExecutor{hook: p.runShadow, hook2: wrap(q.runShadow)}
	}
type resolver struct { state resolverState; root *Proxy }
func newResolver(root *Proxy) *resolver { return nil }
func (p *Proxy) callFusionSynthesizer() { p.client.Do(nil) }`)
	attemptFields := namedStructFields(t, f, "targetAttempt")
	if _, found := attemptFields["extra"]; !found {
		t.Error("targetAttempt field positive control did not expose extra field")
	}
	bad := namedFunction(t, f, "bad")
	if got := compositeLiteralSites(bad.Body, "targetAttempt"); len(got) != 1 {
		t.Errorf("targetAttempt factory confinement positive control: got %d literals, want 1", len(got))
	}
	nestedForbidden := map[string]bool{
		"Proxy": true, "Config": true, "responseCache": true,
		"runtimeSnapshot": true, "targetPlan": true,
	}
	nestedContracts := []struct {
		name string
		want map[string]bool
	}{
		{"attemptExchange", map[string]bool{"request": true, "writer": true, "body": true}},
		{"attemptScope", map[string]bool{
			"calledModel": true, "agent": true, "cacheKey": true, "log": true,
			"responseContext": true, "responsesHistory": true, "responsesSession": true,
		}},
		{"attemptPolicy", map[string]bool{"force": true, "lastTarget": true, "contextRetry": true}},
	}
	for _, contract := range nestedContracts {
		if got := structContractViolations(namedStructFields(t, f, contract.name), contract.want, nestedForbidden); len(got) == 0 {
			t.Errorf("%s nested contract positive control did not fire", contract.name)
		}
	}
	if got := structContractViolations(
		namedStructFields(t, f, "attemptCommit"),
		map[string]bool{"requestBody": true},
		nestedForbidden,
	); len(got) == 0 {
		t.Error("attemptCommit contract positive control did not fire")
	}
	if !typeContainsIdent(namedStructFields(t, f, "attemptExecutor")["proxy"], "Proxy") {
		t.Error("attemptExecutor Proxy field positive control did not fire")
	}
	execute := namedMethod(t, f, "attemptExecutor", "execute")
	if !functionSignatureContainsIdent(execute, "Proxy") {
		t.Error("attemptExecutor Proxy parameter positive control did not fire")
	}
	if got := forbiddenCallSites(execute.Body, token.NewFileSet(), map[string]bool{"reload": true}, nil); len(got) != 1 {
		t.Errorf("executor orchestration positive control: got %v, want reload", got)
	}
	if got := forbiddenCallSites(execute.Body, fset, map[string]bool{"runBeforeLogDrain": true}, nil); len(got) != 1 {
		t.Errorf("executor Shadow orchestration positive control: got %v, want runBeforeLogDrain", got)
	}
	if got := forbiddenIdentifierSites(f, fset, map[string]bool{"proxyLifecycle": true}); len(got) == 0 {
		t.Error("executor forbidden owner type positive control did not fire")
	}
	targetExecutor := namedMethod(t, f, "Proxy", "targetExecutor")
	if got := methodValueInjections(targetExecutor.Body, "attemptExecutor", "p", map[string]bool{"runShadow": true}); len(got) != 2 {
		t.Errorf("Proxy method-value injection positive control: got %d, want direct plus aliased/wrapped injections", len(got))
	}
	if !typeContainsIdent(namedStructFields(t, f, "resolver")["root"], "Proxy") {
		t.Error("resolver Proxy field positive control did not fire")
	}
	if !functionSignatureContainsIdent(namedFunction(t, f, "newResolver"), "Proxy") {
		t.Error("resolver factory Proxy parameter positive control did not fire")
	}
	synth := namedMethod(t, f, "Proxy", "callFusionSynthesizer")
	if got := namedCallCountInNode(synth.Body, "Do"); got != 1 {
		t.Errorf("Fusion parallel pipeline positive control: got %d client.Do calls, want 1", got)
	}
	f, _ = parse(`func newTargetAttempt() int { return 0 }
func (p *Proxy) targetExecutor() attemptExecutor { return attemptExecutor{} }
func (p *Proxy) callFusionSynthesizer() {
	attempt := newTargetAttempt()
	p.targetExecutor().execute(attempt)
}`)
	synth = namedMethod(t, f, "Proxy", "callFusionSynthesizer")
	if got := chainedCallCount(synth.Body, "targetExecutor", "execute"); got != 1 {
		t.Errorf("Fusion executor delegation positive control: got %d chained calls, want 1", got)
	}
	if !assignedFactoryValueExecuted(synth.Body, "newTargetAttempt", "targetExecutor", "execute") {
		t.Error("factory-to-executor dataflow positive control did not accept direct assignment")
	}
	f, _ = parse(`func newTargetAttempt() int { return 0 }
func unrelated() int { return 0 }
func (p *Proxy) targetExecutor() attemptExecutor { return attemptExecutor{} }
func (p *Proxy) serveOnce() {
	attempt := unrelated()
	newTargetAttempt()
	p.targetExecutor().execute(attempt)
}`)
	falseGreen := namedMethod(t, f, "Proxy", "serveOnce")
	if assignedFactoryValueExecuted(falseGreen.Body, "newTargetAttempt", "targetExecutor", "execute") {
		t.Error("factory-to-executor dataflow accepted unrelated factory/execute calls")
	}

	f, _ = parse(`func (p *Proxy) execute() {}
func (p *Proxy) dispatchShadowAfterCommit() {}
func (p *Proxy) ordered() {
	p.execute()
	p.dispatchShadowAfterCommit()
}
func (p *Proxy) reversed() {
	p.dispatchShadowAfterCommit()
	p.execute()
}`)
	ordered := namedMethod(t, f, "Proxy", "ordered")
	if firstNamedCallPos(ordered.Body, "dispatchShadowAfterCommit") <= firstNamedCallPos(ordered.Body, "execute") {
		t.Error("post-commit ordering positive control rejected execute-before-shadow")
	}
	reversed := namedMethod(t, f, "Proxy", "reversed")
	if firstNamedCallPos(reversed.Body, "dispatchShadowAfterCommit") > firstNamedCallPos(reversed.Body, "execute") {
		t.Error("post-commit ordering positive control accepted shadow-before-execute")
	}
}

func parseGoFile(t *testing.T, path string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f, fset
}

func namedStructFields(t *testing.T, f *ast.File, name string) map[string]ast.Expr {
	t.Helper()
	out := map[string]ast.Expr{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}
			st, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("%s is not a struct", name)
			}
			for _, field := range st.Fields.List {
				for _, fieldName := range field.Names {
					out[fieldName.Name] = field.Type
				}
			}
			return out
		}
	}
	t.Fatalf("struct %s not found", name)
	return nil
}

func simpleTypeName(expr ast.Expr) string {
	switch typ := expr.(type) {
	case *ast.Ident:
		return typ.Name
	case *ast.StarExpr:
		return "*" + simpleTypeName(typ.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func typeContainsIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func directSelectorSites(f *ast.File, fset *token.FileSet, receiver, field string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != field {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == receiver {
			out = append(out, describe(fset, sel, receiver+"."+field))
		}
		return true
	})
	return out
}

func unexpectedPortAccesses(f *ast.File, fset *token.FileSet, receiver string, allowed map[string]map[string]bool) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		outer, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := outer.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := inner.X.(*ast.Ident)
		if !ok || base.Name != receiver {
			return true
		}
		methods, isPort := allowed[inner.Sel.Name]
		if isPort && !methods[outer.Sel.Name] {
			out = append(out, describe(fset, outer, receiver+"."+inner.Sel.Name+"."+outer.Sel.Name))
		}
		return true
	})
	return out
}

func methodCallCount(f *ast.File, method, called string) int {
	count := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != method {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == called {
				count++
			}
			return true
		})
	}
	return count
}

func methodDeclared(f *ast.File, method string) bool {
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == method {
			return true
		}
	}
	return false
}

func goStatementCount(f *ast.File) int {
	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if _, ok := n.(*ast.GoStmt); ok {
			count++
		}
		return true
	})
	return count
}

// directAliases returns the idents bound to `.<field>` by direct assignment
// (`x := w.p`, `x = w.p`), propagated through ident-to-ident copies
// (`y := x`) to a fixpoint.
func directAliases(f *ast.File, field string) map[string]bool {
	aliases := map[string]bool{}
	for changed := true; changed; {
		changed = false
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok || aliases[lhs.Name] {
				return true
			}
			match := false
			switch rhs := as.Rhs[0].(type) {
			case *ast.SelectorExpr:
				match = rhs.Sel.Name == field
			case *ast.Ident:
				match = aliases[rhs.Name]
			}
			if match {
				aliases[lhs.Name] = true
				changed = true
			}
			return true
		})
	}
	return aliases
}

// forbiddenFieldAccesses reports selector expressions `<X>.<name>` where name
// is forbidden and X is either a `.<pField>` chain (`w.p.mu` — the base ident
// is irrelevant, any receiver's `.p` is a Proxy) or a direct alias of one
// (`q.mu` after `q := w.p`).
func forbiddenFieldAccesses(f *ast.File, fset *token.FileSet, forbidden map[string]bool, aliases map[string]bool) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !forbidden[sel.Sel.Name] {
			return true
		}
		switch x := sel.X.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "p" {
				out = append(out, describe(fset, sel, fmt.Sprintf("%s.p.%s", exprName(x.X), sel.Sel.Name)))
			}
		case *ast.Ident:
			if aliases[x.Name] {
				out = append(out, describe(fset, sel, x.Name+"."+sel.Sel.Name))
			}
		}
		return true
	})
	return out
}

// forbiddenCallSites reports call expressions whose function name is in
// names (plain `f(...)` or any receiver's `x.f(...)`), plus chained internal
// component calls `<x>.<chain[0]>.<chain[1]>(...)`.
func forbiddenCallSites(f ast.Node, fset *token.FileSet, names map[string]bool, chains [][2]string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if names[fn.Name] {
				out = append(out, describe(fset, call, fn.Name+"(...)"))
			}
		case *ast.SelectorExpr:
			if names[fn.Sel.Name] {
				out = append(out, describe(fset, call, fn.Sel.Name+"(...)"))
			}
			if inner, ok := fn.X.(*ast.SelectorExpr); ok {
				for _, ch := range chains {
					if inner.Sel.Name == ch[0] && fn.Sel.Name == ch[1] {
						out = append(out, describe(fset, call, ch[0]+"."+ch[1]+"(...)"))
					}
				}
			}
		}
		return true
	})
	return out
}

func describe(fset *token.FileSet, n ast.Node, what string) string {
	return fmt.Sprintf("%s at %s", what, fset.Position(n.Pos()))
}

// exprName renders an identifier base (w) or falls back to a placeholder for
// composite expressions, only for readable violation messages.
func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return strings.TrimSpace(fmt.Sprintf("%T", e))
}

func productionGoFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
	}
	return out
}

func sortedFieldNames(fields map[string]ast.Expr) []string {
	out := make([]string, 0, len(fields))
	for name := range fields {
		out = append(out, name)
	}
	return sortedNames(out)
}

func sortedBoolNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	return sortedNames(out)
}

func structContractViolations(fields map[string]ast.Expr, want, forbiddenTypes map[string]bool) []string {
	var out []string
	for name := range fields {
		if !want[name] {
			out = append(out, "unexpected field "+name)
		}
	}
	for name := range want {
		if _, ok := fields[name]; !ok {
			out = append(out, "missing field "+name)
		}
	}
	for field, typ := range fields {
		for forbidden := range forbiddenTypes {
			if typeContainsIdent(typ, forbidden) {
				out = append(out, fmt.Sprintf("field %s contains forbidden type %s", field, forbidden))
			}
		}
	}
	return sortedNames(out)
}

func sortedNames(names []string) []string {
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

func compositeLiteralSites(n ast.Node, typeName string) []*ast.CompositeLit {
	var out []*ast.CompositeLit
	ast.Inspect(n, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if ok && simpleTypeName(lit.Type) == typeName {
			out = append(out, lit)
		}
		return true
	})
	return out
}

func describeNodes(fset *token.FileSet, nodes []*ast.CompositeLit, what string) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, describe(fset, n, what))
	}
	return out
}

func forbiddenIdentifierSites(n ast.Node, fset *token.FileSet, forbidden map[string]bool) []string {
	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if ok && forbidden[id.Name] {
			out = append(out, describe(fset, id, "forbidden identifier "+id.Name))
		}
		return true
	})
	return out
}

func receiverMethodNames(t *testing.T, paths []string, receiver string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, path := range paths {
		f, _ := parseGoFile(t, path)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			for _, field := range fn.Recv.List {
				if typ := simpleTypeName(field.Type); typ == receiver || typ == "*"+receiver {
					out[fn.Name.Name] = true
				}
			}
		}
	}
	return out
}

// methodValueInjections finds Proxy method references anywhere inside keyed
// values of a specific composite literal. It follows direct receiver aliases
// (`q := p`) and descends through wrappers (`hook: wrap(q.runShadow)`), so a
// callback cannot hide the composition-root escape behind a local expression.
// Plain component fields such as `client: p.client` are allowed because client
// is not a declared Proxy method.
func methodValueInjections(n ast.Node, compositeType, receiver string, methods map[string]bool) []ast.Expr {
	aliases := identifierAliases(n, receiver)
	var out []ast.Expr
	ast.Inspect(n, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || simpleTypeName(lit.Type) != compositeType {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			ast.Inspect(kv.Value, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !methods[sel.Sel.Name] {
					return true
				}
				base, ok := sel.X.(*ast.Ident)
				if ok && aliases[base.Name] {
					out = append(out, sel)
				}
				return true
			})
		}
		return true
	})
	return out
}

func identifierAliases(n ast.Node, root string) map[string]bool {
	aliases := map[string]bool{root: true}
	for changed := true; changed; {
		changed = false
		ast.Inspect(n, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != len(as.Rhs) {
				return true
			}
			for i, rhs := range as.Rhs {
				lhs, lhsOK := as.Lhs[i].(*ast.Ident)
				source, rhsOK := rhs.(*ast.Ident)
				if lhsOK && rhsOK && aliases[source.Name] && !aliases[lhs.Name] {
					aliases[lhs.Name] = true
					changed = true
				}
			}
			return true
		})
	}
	return aliases
}

func describeExprNodes(fset *token.FileSet, nodes []ast.Expr, what string) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, describe(fset, n, what))
	}
	return out
}

// assignedFactoryValueExecuted follows the direct local dataflow used by the
// production paths: `attempt := newTargetAttempt(...)` followed by
// `p.targetExecutor().execute(attempt)`. Merely having unrelated factory and
// execute calls in the same function is deliberately insufficient.
func assignedFactoryValueExecuted(n ast.Node, factory, receiverFactory, terminal string) bool {
	produced := map[string]token.Pos{}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if found {
			return false
		}
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != len(node.Rhs) {
				return true
			}
			for i, rhs := range node.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || callableName(call.Fun) != factory {
					continue
				}
				if lhs, ok := node.Lhs[i].(*ast.Ident); ok {
					produced[lhs.Name] = call.Pos()
				}
			}
		case *ast.CallExpr:
			if !isChainedCall(node, receiverFactory, terminal) || len(node.Args) == 0 {
				return true
			}
			arg, ok := node.Args[0].(*ast.Ident)
			if !ok {
				return true
			}
			if pos, exists := produced[arg.Name]; exists && pos < node.Pos() {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func isChainedCall(call *ast.CallExpr, receiverFactory, terminal string) bool {
	outer, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != terminal {
		return false
	}
	inner, ok := outer.X.(*ast.CallExpr)
	return ok && callableName(inner.Fun) == receiverFactory
}

func firstNamedCallPos(n ast.Node, name string) token.Pos {
	var first token.Pos
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || callableName(call.Fun) != name {
			return true
		}
		if !first.IsValid() || call.Pos() < first {
			first = call.Pos()
		}
		return true
	})
	return first
}

func namedFunction(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("function %s not found", name)
	return nil
}

func namedMethod(t *testing.T, f *ast.File, receiver, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != name {
			continue
		}
		for _, field := range fn.Recv.List {
			if simpleTypeName(field.Type) == receiver || simpleTypeName(field.Type) == "*"+receiver {
				return fn
			}
		}
	}
	t.Fatalf("method %s.%s not found", receiver, name)
	return nil
}

func functionSignatureContainsIdent(fn *ast.FuncDecl, name string) bool {
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			if typeContainsIdent(field.Type, name) {
				return true
			}
		}
	}
	if fn.Type.Results != nil {
		for _, field := range fn.Type.Results.List {
			if typeContainsIdent(field.Type, name) {
				return true
			}
		}
	}
	return false
}

func namedCallCount(f *ast.File, name string) int {
	return namedCallCountInNode(f, name)
}

func namedCallCountInNode(n ast.Node, name string) int {
	count := 0
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == name {
				count++
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == name {
				count++
			}
		}
		return true
	})
	return count
}

// chainedCallCount finds `receiverFactory(...).terminal(...)` calls. It is
// intentionally structural: it permits an executor value but rejects a
// similarly named unrelated standalone helper.
func chainedCallCount(n ast.Node, receiverFactory, terminal string) int {
	count := 0
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		outer, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || outer.Sel.Name != terminal {
			return true
		}
		inner, ok := outer.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callableName(inner.Fun) == receiverFactory {
			count++
		}
		return true
	})
	return count
}

func callableName(expr ast.Expr) string {
	switch fun := expr.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	default:
		return ""
	}
}

package archtest

import (
	"go/ast"
	"go/token"
	"os"
	"testing"
)

func TestArchitectureRuntimeBoundaries(t *testing.T) {
	rootPackage, _ := parseGoPackage(t, "internal/app")

	t.Run("internal runtime owns generation-scoped routing state", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/runtime")

		proxyFile := rootPackage
		proxyFields := namedStructFields(t, proxyFile, "processServices")
		if name, ok := configSelectorName(proxyFields["runtimeState"], "runtimestate"); !ok || name != "Manager" {
			t.Error("Proxy.processServices.runtimeState must be runtimestate.Manager")
		}
		for _, group := range []string{"Proxy", "generationState", "processServices"} {
			groupFields := namedStructFields(t, proxyFile, group)
			for _, forbidden := range []string{
				"healthMu", "runtimeGeneration", "health", "sticky", "pins",
				"modelLocks", "paramBlock", "spreadCtr",
			} {
				if _, ok := groupFields[forbidden]; ok {
					t.Errorf("Proxy must not re-own runtime field %s (found on %s)", forbidden, group)
				}
			}
		}

		quotaFile, _ := parseGoFile(t, "internal/runtime/quota_tracker.go")
		quotaFields := namedStructFields(t, quotaFile, "QuotaTracker")
		runtimePointer, ok := quotaFields["runtime"].(*ast.StarExpr)
		if !ok {
			t.Errorf("QuotaTracker.runtime type = %T, want *Manager", quotaFields["runtime"])
		} else if ident, ok := runtimePointer.X.(*ast.Ident); !ok || ident.Name != "Manager" {
			t.Errorf("QuotaTracker.runtime must be *Manager, got %T", runtimePointer.X)
		}
		if _, ok := quotaFields["state"]; ok {
			t.Error("QuotaTracker must not retain a second quota state map")
		}
		for _, legacy := range []string{"stickySnapshot", "healthSnapshot"} {
			if _, ok := quotaFields[legacy]; ok {
				t.Errorf("QuotaTracker must not retain legacy split snapshot callback %s", legacy)
			}
		}
		if _, ok := quotaFields["refreshHook"]; ok {
			t.Error("QuotaTracker must not expose a test-only refreshHook")
		}
		for _, testOnly := range []string{"setSnapshot", "snapshot", "allSnapshots"} {
			if methodDeclared(quotaFile, testOnly) {
				t.Errorf("quota_tracker.go must not expose test-only QuotaTracker.%s", testOnly)
			}
		}
		trackerConstructor := namedFunction(t, quotaFile, "NewQuotaTracker")
		if trackerConstructor.Type.Params == nil || len(trackerConstructor.Type.Params.List) != 4 {
			t.Fatalf("NewQuotaTracker must require exactly four parameters")
		}
		managerParam, ok := trackerConstructor.Type.Params.List[3].Type.(*ast.StarExpr)
		if !ok {
			t.Errorf(
				"NewQuotaTracker fourth parameter type = %T, want *Manager",
				trackerConstructor.Type.Params.List[3].Type,
			)
		} else if ident, ok := managerParam.X.(*ast.Ident); !ok || ident.Name != "Manager" {
			t.Errorf("NewQuotaTracker fourth parameter must be *Manager, got %T", managerParam.X)
		}

		forbiddenTypes := map[string]bool{
			"providerHealth": true,
			"modelLockKey":   true,
			"modelLockEntry": true,
			"routeSticky":    true,
		}
		for _, path := range productionGoFiles(t) {
			parsed, _ := parseGoFile(t, path)
			for _, declaration := range parsed.Decls {
				generic, ok := declaration.(*ast.GenDecl)
				if !ok || generic.Tok != token.TYPE {
					continue
				}
				for _, specification := range generic.Specs {
					typeSpec, ok := specification.(*ast.TypeSpec)
					if ok && forbiddenTypes[typeSpec.Name.Name] {
						t.Errorf("%s redeclares runtime-owned type %s", path, typeSpec.Name.Name)
					}
				}
			}
		}

		constructor := namedFunction(t, proxyFile, "NewProxyWithStatePath")
		injectedManager := 0
		ast.Inspect(constructor.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || callableName(call.Fun) != "NewQuotaTracker" {
				return true
			}
			for _, argument := range call.Args {
				address, ok := argument.(*ast.UnaryExpr)
				if !ok || address.Op != token.AND {
					continue
				}
				selector, ok := address.X.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "runtimeState" {
					continue
				}
				if base, ok := selector.X.(*ast.Ident); ok && base.Name == "p" {
					injectedManager++
				}
			}
			return true
		})
		if got := namedCallCountInNode(constructor.Body, "NewQuotaTracker"); got != 1 || injectedManager != 1 {
			t.Errorf(
				"NewProxyWithStatePath must inject its one Manager into one runtimestate.NewQuotaTracker call: calls=%d injections=%d",
				got,
				injectedManager,
			)
		}

		reload := namedMethod(t, proxyFile, "Proxy", "Reload")
		if got := namedCallCountInNode(reload.Body, "ReplaceGeneration"); got != 1 {
			t.Errorf("Proxy.Reload ReplaceGeneration calls = %d, want exactly 1", got)
		}
		persist := namedMethod(t, proxyFile, "Proxy", "snapshotPersistedState")
		if got := namedCallCountInNode(persist.Body, "RLock"); got != 1 {
			t.Errorf("snapshotPersistedState RLock calls = %d, want exactly 1", got)
		}
		if got := namedCallCountInNode(persist.Body, "RUnlock"); got != 1 {
			t.Errorf("snapshotPersistedState RUnlock calls = %d, want exactly 1", got)
		}
		if got := namedCallCountInNode(persist.Body, "SnapshotForPersist"); got != 1 {
			t.Errorf("snapshotPersistedState SnapshotForPersist calls = %d, want exactly 1", got)
		}
		lockPos := firstNamedCallPos(persist.Body, "RLock")
		snapshotPos := firstNamedCallPos(persist.Body, "SnapshotForPersist")
		unlockPos := firstNamedCallPos(persist.Body, "RUnlock")
		if !lockPos.IsValid() || !snapshotPos.IsValid() || !unlockPos.IsValid() ||
			!(lockPos < snapshotPos && snapshotPos < unlockPos) {
			t.Error("snapshotPersistedState must hold p.mu.RLock across its single Manager snapshot")
		}
		for _, testOnly := range []string{"scheduleHook", "persistSnapshotHook"} {
			for _, group := range []string{"Proxy", "generationState", "processServices"} {
				if _, ok := namedStructFields(t, proxyFile, group)[testOnly]; ok {
					t.Errorf("Proxy must not retain test-only field %s (found on %s)", testOnly, group)
				}
			}
		}
		for _, legacy := range []string{"allSnapshots", "snapshotHealth", "snapshotSticky"} {
			if got := namedCallCountInNode(persist.Body, legacy); got != 0 {
				t.Errorf("snapshotPersistedState reassembles %s %d time(s)", legacy, got)
			}
			if methodDeclared(proxyFile, legacy) {
				t.Errorf("Proxy must not retain legacy split snapshot method %s", legacy)
			}
		}

		portsFile, _ := parseGoFile(t, "internal/app/web_adapter.go")
		dashboardCapture := keyedFuncLitBody(t, portsFile, "DashboardState")
		if got := namedCallCountInNode(dashboardCapture, "Dashboard"); got != 1 {
			t.Errorf("admin DashboardState port Manager.Dashboard calls = %d, want exactly 1", got)
		}
		if got := namedCallCountInNode(dashboardCapture, "scheduleStatusFromSnapshot"); got != 1 {
			t.Errorf(
				"admin DashboardState port scheduleStatusFromSnapshot calls = %d, want exactly 1",
				got,
			)
		}
		if got := namedCallCountInNode(dashboardCapture, "scheduleStatus"); got != 0 {
			t.Errorf("admin DashboardState port rereads runtime through scheduleStatus %d time(s)", got)
		}
		assertCallPathBetween(
			t,
			dashboardCapture,
			"p.runtimeState.Dashboard",
			"p.mu.RLock",
			"p.mu.RUnlock",
		)

		adminRead, _ := parseGoFile(t, "internal/admin/read.go")
		adminDashboard := namedMethod(t, adminRead, "Service", "Dashboard")
		if got := namedCallCountInNode(adminDashboard.Body, "DashboardState"); got != 1 {
			t.Errorf("admin.Service.Dashboard DashboardState port calls = %d, want exactly 1", got)
		}
		if got := namedCallCountInNode(adminDashboard.Body, "scheduleStatus"); got != 0 {
			t.Errorf("admin.Service.Dashboard re-derives the schedule %d time(s)", got)
		}

		scheduleStatus := namedMethod(t, proxyFile, "Proxy", "scheduleStatus")
		if got := namedCallCountInNode(scheduleStatus.Body, "Dashboard"); got != 1 {
			t.Errorf("Proxy.scheduleStatus Manager.Dashboard calls = %d, want exactly 1", got)
		}
		if got := namedCallCountInNode(scheduleStatus.Body, "scheduleStatusFromSnapshot"); got != 1 {
			t.Errorf(
				"Proxy.scheduleStatus scheduleStatusFromSnapshot calls = %d, want exactly 1",
				got,
			)
		}
		assertCallPathBetween(
			t,
			scheduleStatus.Body,
			"p.runtimeState.Dashboard",
			"p.mu.RLock",
			"p.mu.RUnlock",
		)

		statusFromSnapshot := namedFunction(t, proxyFile, "scheduleStatusFromSnapshot")
		// At most two PreviewOrder calls per route: the pin-applied preview
		// (effective first choice) and — only when a pin is active — the
		// IgnorePins preview (default chain the pin overlays). Both consume the
		// SAME detached snapshot, so the single-generation guarantee holds.
		if got := namedCallCountInNode(statusFromSnapshot.Body, "PreviewOrder"); got < 1 || got > 2 {
			t.Errorf("scheduleStatusFromSnapshot PreviewOrder calls = %d, want 1..2 (pin-applied + unpinned overlay)", got)
		}
		for _, forbidden := range []string{"Dashboard", "DecideOrder", "SchedulingQuotas"} {
			if got := namedCallCountInNode(statusFromSnapshot.Body, forbidden); got != 0 {
				t.Errorf("scheduleStatusFromSnapshot rereads Manager through %s %d time(s)", forbidden, got)
			}
		}

		order := namedMethod(t, proxyFile, "Proxy", "decideOrder")
		if got := namedCallCountInNode(order.Body, "DecideOrder"); got != 1 {
			t.Errorf("Proxy.decideOrder Manager.DecideOrder calls = %d, want exactly 1", got)
		}
		if got := namedCallCountInNode(order.Body, "SchedulingQuotas"); got != 0 {
			t.Errorf("Proxy.decideOrder splits quota projection into %d Manager read(s)", got)
		}
		if got := keyedCompositeFieldCallCount(
			order.Body,
			"ScheduleInput",
			"Generation",
			"GenerationArg",
		); got != 1 {
			t.Errorf("Proxy.decideOrder runtimeGenerationArg-bound ScheduleInput fields = %d, want 1", got)
		}

		managerFile, _ := parseGoPackage(t, "internal/runtime")
		if methodDeclared(managerFile, "SchedulingQuotas") {
			t.Error("runtime.Manager must project quotas inside DecideOrder, not expose SchedulingQuotas")
		}
		managerOrder := namedMethod(t, managerFile, "Manager", "DecideOrder")
		if got := namedCallCountInNode(managerOrder.Body, "decideOrder"); got != 1 {
			t.Errorf("Manager.DecideOrder shared order-core calls = %d, want exactly 1", got)
		}
		preview := namedMethod(t, managerFile, "DashboardSnapshot", "PreviewOrder")
		if got := namedCallCountInNode(preview.Body, "decideOrder"); got != 1 {
			t.Errorf("DashboardSnapshot.PreviewOrder shared order-core calls = %d, want exactly 1", got)
		}
	})

	t.Run("internal runtime wirecap owns endpoint capability state", func(t *testing.T) {

		proxy := rootPackage
		wireStoreType := namedStructFields(t, proxy, "processServices")["wireCaps"]
		if name, ok := configSelectorName(wireStoreType, "runtimewire"); !ok || name != "Store" {
			t.Error("Proxy.processServices.wireCaps must be runtimewire.Store")
		}

		adapter, _ := parseGoFile(t, "internal/app/wirecap.go")
		compatibilityAliases := map[string]string{
			"triState": "Verdict",
			"wireCaps": "Capabilities",
		}
		seenAliases := map[string]int{}
		for _, declaration := range adapter.Decls {
			generic, ok := declaration.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, specification := range generic.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok {
					continue
				}
				want, tracked := compatibilityAliases[typeSpec.Name.Name]
				if !tracked {
					t.Errorf("wirecap.go must not declare application-owned type %s", typeSpec.Name.Name)
					continue
				}
				remote, selector := configSelectorName(typeSpec.Type, "runtimewire")
				if !typeSpec.Assign.IsValid() || !selector || remote != want {
					t.Errorf(
						"wirecap.go %s must remain an alias to runtimewire.%s",
						typeSpec.Name.Name,
						want,
					)
				}
				seenAliases[typeSpec.Name.Name]++
			}
		}
		for name := range compatibilityAliases {
			if seenAliases[name] != 1 {
				t.Errorf("wirecap.go alias %s declarations = %d, want exactly 1", name, seenAliases[name])
			}
		}
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
			if _, err := os.Stat(repoRooted(t, legacy)); err == nil {
				t.Errorf("%s must live under internal/protocol, not the root package", legacy)
			} else if !os.IsNotExist(err) {
				t.Fatalf("stat %s: %v", legacy, err)
			}
		}
	})
}

// keyedFuncLitBody returns the body of the func literal assigned to key in a
// composite literal (e.g. the DashboardState: func(...) {...} port entry), so
// contract assertions can target one port closure exactly.
func keyedFuncLitBody(t *testing.T, file *ast.File, key string) *ast.BlockStmt {
	t.Helper()
	var body *ast.BlockStmt
	ast.Inspect(file, func(node ast.Node) bool {
		if body != nil {
			return false
		}
		kv, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		ident, ok := kv.Key.(*ast.Ident)
		if !ok || ident.Name != key {
			return true
		}
		literal, ok := kv.Value.(*ast.FuncLit)
		if !ok {
			return true
		}
		body = literal.Body
		return false
	})
	if body == nil {
		t.Fatalf("no func literal assigned to key %s", key)
	}
	return body
}

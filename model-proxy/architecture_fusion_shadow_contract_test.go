package main

import (
	"go/ast"
	"os"
	"testing"
)

// TestFusionShadowArchitecture keeps orchestration and detached execution in
// their internal owners while the root remains a generation-bound adapter.
func TestFusionShadowArchitecture(t *testing.T) {
	t.Run("Fusion engine is transport-free and root owns only its adapter", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/fusion")
		for _, path := range productionGoFilesIn(t, "internal/fusion") {
			file, fileSet := parseGoFile(t, path)
			if got := forbiddenIdentifierSites(file, fileSet, map[string]bool{
				"Proxy": true, "runtimeSnapshot": true, "counters.MetricsStore": true,
				"counters.TokenCounter": true, "requestlog": true, "http": true,
			}); len(got) != 0 {
				t.Errorf("%s crosses the Fusion orchestration boundary: %v", path, got)
			}
		}
		if _, err := os.Stat("fusion_obs.go"); err == nil {
			t.Error("legacy root fusion_obs.go must not exist; registry and DTOs belong in internal/fusion")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat fusion_obs.go: %v", err)
		}

		root, rootSet := parseGoFile(t, "fusion.go")
		run := namedMethod(t, root, "Proxy", "runFusion")
		if got := namedCallCountInNode(run.Body, "Run"); got != 1 {
			t.Errorf("Proxy.runFusion Engine.Run calls = %d, want exactly 1", got)
		}
		if got := goStatementsIn(run.Body); got != 0 {
			t.Errorf("Proxy.runFusion starts %d goroutines; fan-out belongs in internal/fusion.Engine", got)
		}
		if got := forbiddenCallSites(run.Body, rootSet, map[string]bool{
			"Admit": true, "Record": true, "CollectResults": true,
			"BuildSynthesisBody": true, "BuildJudgeBody": true,
		}, nil); len(got) != 0 {
			t.Errorf("Proxy.runFusion duplicates engine policy: %v", got)
		}
		engine, _ := parseGoFile(t, "internal/fusion/engine.go")
		if got := goStatementsIn(namedMethod(t, engine, "Engine", "Run").Body); got != 1 {
			t.Errorf("fusion.Engine.Run goroutines = %d, want one fan-out site", got)
		}
		callLeg := namedMethod(t, root, "Proxy", "callFusionLeg")
		configuredPos := firstNamedCallPos(callLeg.Body, "ApplyConfiguredHeaders")
		extraPos := firstNamedCallPos(callLeg.Body, "ExtraHeaders")
		if !configuredPos.IsValid() || !extraPos.IsValid() || configuredPos >= extraPos {
			t.Errorf(
				"Fusion headers must apply configured values before provider extras (configured=%v extra=%v)",
				configuredPos,
				extraPos,
			)
		}

		proxy, _ := parseGoFile(t, "proxy.go")
		field := namedStructFields(t, proxy, "Proxy")["fusionReg"]
		pointer, ok := field.(*ast.StarExpr)
		if !ok {
			t.Errorf("Proxy.fusionReg type = %T, want *fusion.Registry", field)
		} else if name, ok := configSelectorName(pointer.X, "fusion"); !ok || name != "Registry" {
			t.Error("Proxy.fusionReg must be *fusion.Registry")
		}
	})

	t.Run("Shadow runtime owns detached transport and root owns ordering", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/shadow")
		for _, path := range productionGoFilesIn(t, "internal/shadow") {
			file, fileSet := parseGoFile(t, path)
			if got := forbiddenIdentifierSites(file, fileSet, map[string]bool{
				"Proxy": true, "proxyLifecycle": true, "runtimeSnapshot": true,
				"Manager": true, "requestlog": true, "counters.MetricsStore": true,
				"Executor": true,
			}); len(got) != 0 {
				t.Errorf("%s crosses the detached Shadow boundary: %v", path, got)
			}
		}

		root, rootSet := parseGoFile(t, "proxy_shadow.go")
		dispatch := namedMethod(t, root, "Proxy", "dispatchShadowAfterCommit")
		samplePos := firstNamedCallPos(dispatch.Body, "ShouldSample")
		acquirePos := firstNamedCallPos(dispatch.Body, "TryAcquire")
		admitPos := firstNamedCallPos(dispatch.Body, "runBeforeLogDrain")
		if !samplePos.IsValid() || !acquirePos.IsValid() || !admitPos.IsValid() ||
			!(samplePos < acquirePos && acquirePos < admitPos) {
			t.Errorf("Shadow dispatch order must be sample → acquire → lifecycle admission (sample=%v acquire=%v admit=%v)",
				samplePos, acquirePos, admitPos)
		}
		if got := namedCallCountInNode(dispatch.Body, "Release"); got != 2 {
			t.Errorf("Shadow permit Release calls = %d, want admitted and rejected cleanup paths", got)
		}
		run := namedMethod(t, root, "Proxy", "runShadow")
		if got := namedCallCountInNode(run.Body, "Execute"); got != 1 {
			t.Errorf("Proxy.runShadow Runtime.Execute calls = %d, want exactly 1", got)
		}
		if got := forbiddenCallSites(run.Body, rootSet, map[string]bool{
			"Do": true, "ConvertBody": true, "RewriteRequest": true,
			"AuthHeaders": true, "New": true,
		}, nil); len(got) != 0 {
			t.Errorf("Proxy.runShadow duplicates detached transport: %v", got)
		}
		shadowFile, _ := parseGoFile(t, "internal/shadow/runtime.go")
		execute := namedMethod(t, shadowFile, "Runtime", "Execute")
		configuredPos := firstNamedCallPos(execute.Body, "ApplyConfiguredHeaders")
		extraPos := firstNamedCallPos(execute.Body, "ExtraHeaders")
		if !configuredPos.IsValid() || !extraPos.IsValid() || configuredPos >= extraPos {
			t.Errorf(
				"Shadow headers must apply configured values before provider extras (configured=%v extra=%v)",
				configuredPos,
				extraPos,
			)
		}

		snapshot, _ := parseGoFile(t, "dispatch_context.go")
		field := namedStructFields(t, snapshot, "runtimeSnapshot")["shadow"]
		pointer, ok := field.(*ast.StarExpr)
		if !ok {
			t.Errorf("runtimeSnapshot.shadow type = %T, want *shadow.Runtime", field)
		} else if name, ok := configSelectorName(pointer.X, "shadow"); !ok || name != "Runtime" {
			t.Error("runtimeSnapshot.shadow must be *shadow.Runtime")
		}
	})
}

func goStatementsIn(node ast.Node) int {
	count := 0
	ast.Inspect(node, func(node ast.Node) bool {
		if _, ok := node.(*ast.GoStmt); ok {
			count++
		}
		return true
	})
	return count
}

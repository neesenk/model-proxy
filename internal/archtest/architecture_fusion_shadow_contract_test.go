package archtest

import (
	"go/ast"
	"os"
	"strings"
	"testing"
)

// TestFusionShadowArchitecture keeps orchestration and detached execution in
// their internal owners while the root remains a generation-bound adapter.
func TestFusionShadowArchitecture(t *testing.T) {
	t.Run("Fusion engine is transport-free and root owns only its adapter", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/fusion")
		for _, path := range productionGoFilesIn(t, "internal/fusion") {
			file, fileSet := parseGoFile(t, path)
			// Transport-free is enforced on the net/http IMPORT PATH, not on an
			// `http` ident: `import nethttp "net/http"` hides the ident but not
			// the import. (counters/requestlog package deps are closed off by
			// the DAG allowlist.)
			for _, spec := range file.Imports {
				if importPath := strings.Trim(spec.Path.Value, `"`); importPath == "net/http" {
					t.Errorf("%s imports net/http; Fusion orchestration must stay transport-free", path)
				}
			}
			if got := forbiddenIdentifierSites(file, fileSet, map[string]bool{
				"Proxy": true, "RuntimeSnapshot": true,
			}); len(got) != 0 {
				t.Errorf("%s crosses the Fusion orchestration boundary: %v", path, got)
			}
		}
		if _, err := os.Stat(repoRooted(t, "fusion_obs.go")); err == nil {
			t.Error("legacy root fusion_obs.go must not exist; registry and DTOs belong in internal/fusion")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat fusion_obs.go: %v", err)
		}

		root, rootSet := parseGoFile(t, "internal/forward/fusion.go")
		run := namedMethod(t, root, "pipeline", "runFusion")
		if got := namedCallCountInNode(run.Body, "Run"); got != 1 {
			t.Errorf("pipeline.runFusion Engine.Run calls = %d, want exactly 1", got)
		}
		if got := goStatementsIn(run.Body); got != 0 {
			t.Errorf("pipeline.runFusion starts %d goroutines; fan-out belongs in internal/fusion.Engine", got)
		}
		if got := forbiddenCallSites(run.Body, rootSet, map[string]bool{
			"Admit": true, "Record": true, "CollectResults": true,
			"BuildSynthesisBody": true, "BuildJudgeBody": true,
		}, nil); len(got) != 0 {
			t.Errorf("pipeline.runFusion duplicates engine policy: %v", got)
		}
		engine, _ := parseGoFile(t, "internal/fusion/engine.go")
		if got := goStatementsIn(namedMethod(t, engine, "Engine", "Run").Body); got != 1 {
			t.Errorf("fusion.Engine.Run goroutines = %d, want one fan-out site", got)
		}
		callLeg := namedMethod(t, root, "pipeline", "callFusionLeg")
		// The leg send loop is owned by targetexec.BufferedLeg: callFusionLeg
		// wires exactly one BufferedLeg and must not re-grow transport steps.
		bufferedLegs := 0
		ast.Inspect(callLeg.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if name, ok := configSelectorName(literal.Type, "targetexec"); ok && name == "BufferedLeg" {
				bufferedLegs++
			}
			return true
		})
		if bufferedLegs != 1 {
			t.Errorf("pipeline.callFusionLeg targetexec.BufferedLeg composites = %d, want exactly 1", bufferedLegs)
		}
		if got := namedCallCountInNode(callLeg.Body, "Do"); got != 1 {
			t.Errorf("pipeline.callFusionLeg Do calls = %d, want exactly one BufferedLeg.Do (no parallel client pipeline)", got)
		}
		if got := forbiddenCallSites(callLeg.Body, rootSet, map[string]bool{
			"RewriteRequest": true, "AuthHeaders": true,
			"ApplyConfiguredHeaders": true, "ExtraHeaders": true,
			"NewRequestWithContext": true, "ReadAll": true,
		}, nil); len(got) != 0 {
			t.Errorf("pipeline.callFusionLeg duplicates leg transport owned by targetexec.BufferedLeg: %v", got)
		}
		legFile, _ := parseGoFile(t, "internal/targetexec/buffered_leg.go")
		legDo := namedMethod(t, legFile, "BufferedLeg", "Do")
		configuredPos := firstNamedCallPos(legDo.Body, "ApplyConfiguredHeaders")
		extraPos := firstNamedCallPos(legDo.Body, "ExtraHeaders")
		if !configuredPos.IsValid() || !extraPos.IsValid() || configuredPos >= extraPos {
			t.Errorf(
				"Fusion leg headers must apply configured values before provider extras (configured=%v extra=%v)",
				configuredPos,
				extraPos,
			)
		}

		proxy, _ := parseGoFile(t, "internal/app/proxy.go")
		field := namedStructFields(t, proxy, "processServices")["fusionReg"]
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
			// (counters/requestlog package deps are closed off by the DAG
			// allowlist; dotted names like "counters.MetricsStore" can never
			// match a bare identifier and were dead rules.)
			if got := forbiddenIdentifierSites(file, fileSet, map[string]bool{
				"Proxy": true, "proxyLifecycle": true, "RuntimeSnapshot": true,
				"Manager": true, "Executor": true,
			}); len(got) != 0 {
				t.Errorf("%s crosses the detached Shadow boundary: %v", path, got)
			}
		}

		root, rootSet := parseGoFile(t, "internal/app/proxy_shadow.go")
		dispatch := namedMethod(t, root, "Proxy", "dispatchShadowAfterCommit")
		samplePos := firstNamedCallPos(dispatch.Body, "ShouldSample")
		acquirePos := firstNamedCallPos(dispatch.Body, "TryAcquire")
		admitPos := firstNamedCallPos(dispatch.Body, "RunBeforeLogDrain")
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

		snapshot, _ := parseGoFile(t, "internal/forward/snapshot.go")
		field := namedStructFields(t, snapshot, "Snapshot")["Shadow"]
		pointer, ok := field.(*ast.StarExpr)
		if !ok {
			t.Errorf("Snapshot.Shadow type = %T, want *shadow.Runtime", field)
		} else if name, ok := configSelectorName(pointer.X, "shadow"); !ok || name != "Runtime" {
			t.Error("Snapshot.Shadow must be *shadow.Runtime")
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

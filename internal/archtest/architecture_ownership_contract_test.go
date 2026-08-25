package archtest

import (
	"go/ast"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestArchitectureOwnershipBoundaries(t *testing.T) {
	rootPackage, rootSet := parseGoPackage(t, "internal/app")

	t.Run("internal config owns configuration", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/config")
		if _, err := os.Stat(repoRooted(t, "config.go")); err == nil {
			t.Error("legacy root config.go must not exist; configuration belongs in internal/config")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat config.go: %v", err)
		}
		if _, err := os.Stat(repoRooted(t, "config_compat.go")); err == nil {
			t.Error("legacy root config_compat.go must not exist; production code uses internal/config directly")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat config_compat.go: %v", err)
		}
		if _, err := os.Stat(repoRooted(t, "config_alias_test.go")); err == nil {
			t.Error("legacy root config_alias_test.go facade must not exist; tests use internal/config (or the owning package's aliases) directly")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat config_alias_test.go: %v", err)
		}
		// The surviving facade is internal/app/config_alias.go: it must stay a
		// narrow alias+delegate seam and never grow configuration logic.
		facade, _ := parseGoFile(t, "internal/app/config_alias.go")
		for _, violation := range configCompatViolationsForAliases(facade, requiredConfigCompatAliases()) {
			t.Errorf("internal/app/config_alias.go: %s", violation)
		}
	})

	t.Run("internal catalog owns the models.dev source kernel", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/catalog")
		for _, legacy := range []string{"modelsdev.go", "model_catalog_types.go"} {
			if _, err := os.Stat(repoRooted(t, legacy)); err == nil {
				t.Errorf("legacy root %s must not exist; catalog types and source logic belong in internal/catalog", legacy)
			} else if !os.IsNotExist(err) {
				t.Fatalf("stat %s: %v", legacy, err)
			}
		}

		adapter, _ := parseGoFile(t, "internal/app/catalog_adapter.go")
		wantImports := map[string]bool{
			"os":                           true,
			"path/filepath":                true,
			"model-proxy/internal/catalog": true,
			"model-proxy/internal/config":  true,
		}
		for _, spec := range adapter.Imports {
			importPath := strings.Trim(spec.Path.Value, `"`)
			if !wantImports[importPath] {
				t.Errorf("app catalog_adapter.go has unexpected import %q", importPath)
			}
			delete(wantImports, importPath)
		}
		for missing := range wantImports {
			t.Errorf("app catalog_adapter.go is missing required import %q", missing)
		}
		wantFunctions := map[string]int{
			"ModelsCatalogEndpoint": 0,
			"ModelsCatalogPath":     0,
			"LoadModelsCatalog":     0,
			"HydrateModels":         0,
		}
		for _, decl := range adapter.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv != nil {
				t.Errorf("app catalog_adapter.go has unexpected method %s", fn.Name.Name)
				continue
			}
			if _, allowed := wantFunctions[fn.Name.Name]; !allowed {
				t.Errorf("app catalog_adapter.go has unexpected function %s; source/cache logic belongs in internal/catalog", fn.Name.Name)
				continue
			}
			wantFunctions[fn.Name.Name]++
		}
		for name, count := range wantFunctions {
			if count != 1 {
				t.Errorf("app catalog_adapter.go %s declarations = %d, want exactly 1", name, count)
			}
		}

		snapshot, _ := parseGoFile(t, "internal/app/dispatch_context.go")
		catalogType := namedStructFields(t, snapshot, "RuntimeSnapshot")["Catalog"]
		pointer, ok := catalogType.(*ast.StarExpr)
		if !ok {
			t.Errorf("RuntimeSnapshot.Catalog type = %T, want *catalog.Catalog", catalogType)
		} else if name, ok := configSelectorName(pointer.X, "catalog"); !ok || name != "Catalog" {
			t.Errorf("RuntimeSnapshot.Catalog must be *catalog.Catalog")
		}
		forbiddenRefresh := map[string]bool{
			"EnsureFresh": true, "FetchHTTP": true, "loadModelsCatalog": true,
			"modelsCatalogEndpoint": true, "modelsCatalogPath": true,
		}
		for _, path := range []string{"internal/routing/request.go", "internal/app/request_routing_adapter.go"} {
			routingFile, routingSet := parseGoFile(t, path)
			for _, violation := range forbiddenCallSites(routingFile, routingSet, forbiddenRefresh, nil) {
				t.Errorf("%s refreshes or re-reads catalog instead of using RuntimeSnapshot: %s", path, violation)
			}
		}
	})

	t.Run("internal accounts owns pool storage and identity", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/accounts")
		if _, err := os.Stat(repoRooted(t, "pool.go")); err == nil {
			t.Error("legacy root pool.go must not exist; account storage belongs in internal/accounts")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat pool.go: %v", err)
		}

		// The root accounts adapter shell is gone: callers use internal/app's
		// account-store wrappers (or internal/accounts directly).
		if _, err := os.Stat(repoRooted(t, "accounts_adapter.go")); err == nil {
			t.Error("legacy root accounts_adapter.go must not exist; use internal/app account wrappers")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat accounts_adapter.go: %v", err)
		}

		// Provider construction and implicit-route eligibility must share one
		// authoritative account snapshot per provider. Keep these structural
		// guards here so moving the assembly code cannot silently re-introduce a
		// second filesystem probe (and a generation-local TOCTOU decision).
		builder := namedFunction(t, rootPackage, "BuildProviders")
		if got := namedCallCountInNode(builder.Body, "LoadSnapshot"); got != 1 {
			t.Errorf("app.BuildProviders LoadSnapshot calls = %d, want exactly 1 storage decision point", got)
		}
		forbiddenStorageProbes := map[string]bool{
			"loadPool": true, "poolPath": true, "singularPoolPath": true,
			"Load": true, "PoolPath": true, "LegacyPath": true,
			"Stat": true, "ReadFile": true, "Open": true, "OpenFile": true, "ReadDir": true,
		}
		for _, violation := range forbiddenCallSites(builder.Body, rootSet, forbiddenStorageProbes, nil) {
			t.Errorf("app.BuildProviders re-reads or probes account storage outside its snapshot: %s", violation)
		}

		loggedIn := namedFunction(t, rootPackage, "LoggedInProviders")
		if got := namedCallCountInNode(loggedIn.Body, "LoadSnapshot"); got != 1 {
			t.Errorf("app.LoggedInProviders LoadSnapshot calls = %d, want exactly 1 storage decision point", got)
		}
		for _, violation := range forbiddenCallSites(loggedIn.Body, rootSet, forbiddenStorageProbes, nil) {
			t.Errorf("app.LoggedInProviders re-reads or probes account storage outside its snapshot: %s", violation)
		}

		buildFields := namedStructFields(t, rootPackage, "Build")
		wantBuildFields := map[string]bool{
			"Providers": true, "PoolIndex": true, "ParentOf": true, "Eligible": true,
			// Secrets: proxy-managed credential values collected in the same
			// LoadSnapshot pass (+ best-effort OAuth auth files) for the guard
			// known-secret scanner. Memory only.
			"Secrets": true,
		}
		if len(buildFields) != len(wantBuildFields) {
			t.Errorf("app.Build fields = %v, want exactly %v", sortedFieldNames(buildFields), sortedBoolNames(wantBuildFields))
		}
		for name := range wantBuildFields {
			if _, ok := buildFields[name]; !ok {
				t.Errorf("app.Build missing %q", name)
			}
		}

		assertUsesBuildEligibility := func(owner string, body ast.Node) {
			t.Helper()
			if got := namedCallCountInNode(body, "synthesizeImplicitRoutesFrom"); got != 1 {
				t.Errorf("%s synthesizeImplicitRoutesFrom calls = %d, want 1 build-derived eligibility use", owner, got)
			}
			if got := namedCallWithArgsCount(body, "synthesizeImplicitRoutesFrom", "cfg", "built", "Eligible"); got != 1 {
				t.Errorf("%s must pass exactly (cfg, built.Eligible), matches = %d", owner, got)
			}
			for _, forbidden := range []string{
				"LoggedInProviders", "loggedInProviders",
				"SynthesizeImplicitRoutes", "synthesizeImplicitRoutes",
			} {
				if got := namedCallCountInNode(body, forbidden); got != 0 {
					t.Errorf("%s calls %s %d time(s), re-reading account eligibility", owner, forbidden, got)
				}
			}
		}
		constructor := namedFunction(t, rootPackage, "NewProxyWithStatePath")
		assertUsesBuildEligibility("NewProxyWithStatePath", constructor.Body)
		reload := namedMethod(t, rootPackage, "Proxy", "Reload")
		assertUsesBuildEligibility("Proxy.Reload", reload.Body)
	})

	t.Run("internal pricing remains a repository-leaf package", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/pricing")
	})

	t.Run("internal observe events owns the live-event hub", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/observe/events")
		if _, err := os.Stat(repoRooted(t, "live_events.go")); err == nil {
			t.Error("legacy root live_events.go adapter shell must not exist; proxy_http.go calls observeevents.ServeEvents directly")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat live_events.go: %v", err)
		}
		handler, _ := parseGoFile(t, "internal/app/proxy_http.go")
		if got := selectorCountNamed(handler, "ServeEvents"); got != 1 {
			t.Errorf("proxy_http.go observeevents.ServeEvents calls = %d, want exactly 1 direct call", got)
		}
		// The SSE handler and keepalive loop live in the leaf package.
		sse, _ := parseGoFile(t, "internal/observe/events/sse.go")
		sseFunctions := map[string]int{"ServeEvents": 0, "keepaliveLoop": 0}
		for _, decl := range sse.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, tracked := sseFunctions[fn.Name.Name]; tracked {
				sseFunctions[fn.Name.Name]++
			}
		}
		for name, count := range sseFunctions {
			if count != 1 {
				t.Errorf("internal/observe/events/sse.go %s declarations = %d, want exactly 1", name, count)
			}
		}

		proxy := rootPackage
		eventsType := namedStructFields(t, proxy, "Proxy")["events"]
		pointer, ok := eventsType.(*ast.StarExpr)
		if !ok {
			t.Errorf("Proxy.events type = %T, want *observeevents.Hub", eventsType)
		} else if name, ok := configSelectorName(pointer.X, "observeevents"); !ok || name != "Hub" {
			t.Error("Proxy.events must be *observeevents.Hub")
		}
	})

	t.Run("internal observe requestlog owns the JSONL data plane", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/observe/requestlog")
		if _, err := os.Stat(repoRooted(t, "request_log.go")); err == nil {
			t.Error("legacy root request_log.go must not exist; request-log mechanics belong in internal/observe/requestlog")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat request_log.go: %v", err)
		}

		adapter, _ := parseGoFile(t, "internal/app/request_log_adapter.go")
		wantImports := map[string]bool{
			"log": true,
			"model-proxy/internal/observe/requestlog": true,
		}
		wantFunctions := map[string]int{
			"initRequestLog": 0,
		}
		for _, spec := range adapter.Imports {
			importPath := strings.Trim(spec.Path.Value, `"`)
			if !wantImports[importPath] {
				t.Errorf("request_log_adapter.go has unexpected import %q", importPath)
			}
			delete(wantImports, importPath)
		}
		for missing := range wantImports {
			t.Errorf("request_log_adapter.go is missing required import %q", missing)
		}
		for _, decl := range adapter.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				if decl.Tok != token.IMPORT {
					t.Error("request_log_adapter.go must not declare types or package state")
				}
			case *ast.FuncDecl:
				if _, ok := wantFunctions[decl.Name.Name]; !ok {
					t.Errorf("request_log_adapter.go has unexpected function %s", decl.Name.Name)
					continue
				}
				wantFunctions[decl.Name.Name]++
			default:
				t.Errorf("request_log_adapter.go has unexpected top-level declaration %T", decl)
			}
		}
		for name, count := range wantFunctions {
			if count != 1 {
				t.Errorf("request_log_adapter.go %s declarations = %d, want exactly 1", name, count)
			}
		}

		proxy := rootPackage
		loggerType := namedStructFields(t, proxy, "Proxy")["reqLog"]
		pointer, ok := loggerType.(*ast.StarExpr)
		if !ok {
			t.Errorf("Proxy.reqLog type = %T, want *requestlog.Logger", loggerType)
		} else if name, ok := configSelectorName(pointer.X, "requestlog"); !ok || name != "Logger" {
			t.Error("Proxy.reqLog must be *requestlog.Logger")
		}
	})

	t.Run("internal observe stats owns SQLite persistence and the runtime projection", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/observe/stats")
		if _, err := os.Stat(repoRooted(t, "stats.go")); err == nil {
			t.Error("legacy root stats.go must not exist; persistence and the minute-diff projection belong in internal/observe/stats")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat stats.go: %v", err)
		}

		proxy := rootPackage
		storeType := namedStructFields(t, proxy, "Proxy")["stats"]
		pointer, ok := storeType.(*ast.StarExpr)
		if !ok {
			t.Errorf("Proxy.stats type = %T, want *observestats.Store", storeType)
		} else if name, ok := configSelectorName(pointer.X, "observestats"); !ok || name != "Store" {
			t.Error("Proxy.stats must be *observestats.Store")
		}
		flusherType := namedStructFields(t, proxy, "Proxy")["flusher"]
		pointer, ok = flusherType.(*ast.StarExpr)
		if !ok {
			t.Errorf("Proxy.flusher type = %T, want *observestats.Flusher", flusherType)
		} else if name, ok := configSelectorName(pointer.X, "observestats"); !ok || name != "Flusher" {
			t.Error("Proxy.flusher must be *observestats.Flusher")
		}

		// The projection loop owns the Flusher lifecycle: reset accepts no
		// Proxy (cache reset and composition stay at Proxy.resetStats).
		flusher, _ := parseGoFile(t, "internal/observe/stats/flusher.go")
		reset := namedMethod(t, flusher, "Flusher", "Reset")
		for _, field := range reset.Type.Params.List {
			if typeContainsIdent(field.Type, "Proxy") {
				t.Error("Flusher.Reset must not accept *Proxy; cache reset and composition stay at Proxy.resetStats")
			}
		}
	})

	t.Run("internal observe cache owns exact-response storage", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/cache")
		if _, err := os.Stat(repoRooted(t, "cache.go")); err == nil {
			t.Error("legacy root cache.go must not exist; cache mechanics belong in internal/cache")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat cache.go: %v", err)
		}

		adapter, _ := parseGoFile(t, "internal/app/proxy_constructor.go")
		if got := namedCallCount(adapter, "NewResponseCache"); got != 1 {
			t.Errorf("proxy_constructor.go NewResponseCache declarations = %d, want exactly 1", got)
		}

		assertCacheStoreFieldNamed := func(parsed *ast.File, owner, field string) {
			t.Helper()
			fieldType := namedStructFields(t, parsed, owner)[field]
			pointer, ok := fieldType.(*ast.StarExpr)
			if !ok {
				t.Errorf("%s.Cache type = %T, want *responsecache.Store", owner, fieldType)
			} else if name, ok := configSelectorName(pointer.X, "responsecache"); !ok || name != "Store" {
				t.Errorf("%s.Cache must be *responsecache.Store", owner)
			}
		}
		assertCacheStoreFieldNamed(rootPackage, "Proxy", "cache")
		dispatchContext, _ := parseGoFile(t, "internal/app/dispatch_context.go")
		assertCacheStoreFieldNamed(dispatchContext, "RuntimeSnapshot", "Cache")

		forbidden := map[string]bool{
			"responseCache": true,
			"cacheEntry":    true,
			"cacheRecorder": true,
		}
		for _, path := range productionGoFiles(t) {
			parsed, _ := parseGoFile(t, path)
			for _, decl := range parsed.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if ok && forbidden[typeSpec.Name.Name] {
						t.Errorf("%s redeclares root cache type %s", path, typeSpec.Name.Name)
					}
				}
			}
		}
	})

	t.Run("internal transport owns bounded body capture", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/transport/bodycapture")
		forbidden := map[string]bool{"newCaptureReader": true}
		for _, violation := range forbiddenCallSites(rootPackage, rootSet, forbidden, nil) {
			t.Errorf("root package uses legacy capture instead of bodycapture.Reader: %s", violation)
		}
	})

}

package main

import (
	"go/ast"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestArchitectureOwnershipBoundaries(t *testing.T) {
	rootPackage, rootSet := parseGoPackage(t, ".")

	t.Run("internal config owns configuration behind a narrow root facade", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/config")
		if _, err := os.Stat("config.go"); err == nil {
			t.Error("legacy root config.go must not exist; configuration belongs in internal/config")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat config.go: %v", err)
		}
		facade, _ := parseGoFile(t, "config_compat.go")
		if got := configCompatViolations(facade); len(got) != 0 {
			t.Errorf("config_compat.go must contain only internal/config type aliases and direct Load wrappers: %v", got)
		}
	})

	t.Run("internal catalog owns the models.dev source kernel", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/catalog")
		for _, legacy := range []string{"modelsdev.go", "model_catalog_types.go"} {
			if _, err := os.Stat(legacy); err == nil {
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

		snapshot, _ := parseGoFile(t, "dispatch_context.go")
		catalogType := namedStructFields(t, snapshot, "runtimeSnapshot")["catalog"]
		pointer, ok := catalogType.(*ast.StarExpr)
		if !ok {
			t.Errorf("runtimeSnapshot.catalog type = %T, want *catalog.Catalog", catalogType)
		} else if name, ok := configSelectorName(pointer.X, "catalog"); !ok || name != "Catalog" {
			t.Errorf("runtimeSnapshot.catalog must be *catalog.Catalog")
		}
		forbiddenRefresh := map[string]bool{
			"EnsureFresh": true, "FetchHTTP": true, "loadModelsCatalog": true,
			"modelsCatalogEndpoint": true, "modelsCatalogPath": true,
		}
		for _, path := range []string{"internal/routing/request.Go", "request_routing_adapter.go"} {
			routingFile, routingSet := parseGoFile(t, path)
			for _, violation := range forbiddenCallSites(routingFile, routingSet, forbiddenRefresh, nil) {
				t.Errorf("%s refreshes or re-reads catalog instead of using runtimeSnapshot: %s", path, violation)
			}
		}
	})

	t.Run("internal accounts owns pool storage and identity", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/accounts")
		if _, err := os.Stat("pool.go"); err == nil {
			t.Error("legacy root pool.go must not exist; account storage belongs in internal/accounts")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat pool.go: %v", err)
		}

		adapter, _ := parseGoFile(t, "accounts_adapter.go")
		wantImports := map[string]bool{
			"time":                          true,
			"model-proxy/internal/accounts": true,
		}
		for _, spec := range adapter.Imports {
			importPath := strings.Trim(spec.Path.Value, `"`)
			if !wantImports[importPath] {
				t.Errorf("accounts_adapter.go has unexpected import %q", importPath)
			}
			delete(wantImports, importPath)
		}
		for missing := range wantImports {
			t.Errorf("accounts_adapter.go is missing required import %q", missing)
		}

		wantAliases := map[string]string{
			"accountCred":    "Credentials",
			"poolAccount":    "Account",
			"credentialPool": "Pool",
		}
		seenAliases := map[string]int{}
		wantFunctions := map[string]int{
			"accountStore": 0,
			"poolPath":     0,
			"loadPool":     0,
			"savePool":     0,
			"withPoolLock": 0,
			"accountIDFor": 0,
			"nowTS":        0,
		}
		for _, decl := range adapter.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				if decl.Tok == token.IMPORT {
					continue
				}
				if decl.Tok != token.TYPE {
					t.Errorf("accounts_adapter.go has unexpected %s declaration", decl.Tok)
					continue
				}
				for _, spec := range decl.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !typeSpec.Assign.IsValid() {
						t.Error("accounts_adapter.go may contain only type aliases")
						continue
					}
					remote, ok := configSelectorName(typeSpec.Type, "accounts")
					want, expected := wantAliases[typeSpec.Name.Name]
					if !ok || !expected || remote != want {
						t.Errorf("accounts_adapter.go has unexpected alias %s=%s", typeSpec.Name.Name, remote)
						continue
					}
					seenAliases[typeSpec.Name.Name]++
				}
			case *ast.FuncDecl:
				if decl.Recv != nil {
					t.Errorf("accounts_adapter.go has unexpected method %s", decl.Name.Name)
					continue
				}
				if _, allowed := wantFunctions[decl.Name.Name]; !allowed {
					t.Errorf("accounts_adapter.go has unexpected function %s", decl.Name.Name)
					continue
				}
				wantFunctions[decl.Name.Name]++
				if !isAccountsAdapterWrapper(decl) {
					t.Errorf("accounts_adapter.go %s must remain a direct environment/compatibility wrapper", decl.Name.Name)
				}
			default:
				t.Errorf("accounts_adapter.go has unexpected declaration %T", decl)
			}
		}
		for name := range wantAliases {
			if seenAliases[name] != 1 {
				t.Errorf("accounts_adapter.go alias %s declarations = %d, want exactly 1", name, seenAliases[name])
			}
		}
		for name, count := range wantFunctions {
			if count != 1 {
				t.Errorf("accounts_adapter.go %s declarations = %d, want exactly 1", name, count)
			}
		}

		proxyFile, _ := rootPackage, rootSet
		appFile, appSet := parseGoPackage(t, "internal/app")
		builder := namedFunction(t, appFile, "BuildProviders")
		if got := namedCallCountInNode(builder.Body, "LoadSnapshot"); got != 1 {
			t.Errorf("app.BuildProviders LoadSnapshot calls = %d, want exactly 1 storage decision point", got)
		}
		forbiddenStorageProbes := map[string]bool{
			"loadPool": true, "poolPath": true, "singularPoolPath": true,
			"Load": true, "PoolPath": true, "LegacyPath": true,
			"Stat": true, "ReadFile": true, "Open": true, "OpenFile": true, "ReadDir": true,
		}
		for _, violation := range forbiddenCallSites(builder.Body, appSet, forbiddenStorageProbes, nil) {
			t.Errorf("app.BuildProviders re-reads or probes account storage outside its snapshot: %s", violation)
		}
		loggedIn := namedFunction(t, appFile, "LoggedInProviders")
		if got := namedCallCountInNode(loggedIn.Body, "LoadSnapshot"); got != 1 {
			t.Errorf("app.LoggedInProviders LoadSnapshot calls = %d, want exactly 1 storage decision point", got)
		}
		for _, violation := range forbiddenCallSites(loggedIn.Body, appSet, forbiddenStorageProbes, nil) {
			t.Errorf("app.LoggedInProviders re-reads or probes account storage outside its snapshot: %s", violation)
		}

		buildFields := namedStructFields(t, appFile, "Build")
		wantBuildFields := map[string]bool{
			"Providers": true, "PoolIndex": true, "ParentOf": true, "Eligible": true,
		}
		if len(buildFields) != len(wantBuildFields) {
			t.Errorf("app.Build fields = %v, want exactly %v", sortedFieldNames(buildFields), sortedBoolNames(wantBuildFields))
		}
		for name := range wantBuildFields {
			if _, ok := buildFields[name]; !ok {
				t.Errorf("app.Build missing %q", name)
			}
		}

		constructor := namedFunction(t, proxyFile, "newProxyWithStatePath")
		if got := namedCallCountInNode(constructor.Body, "synthesizeImplicitRoutesFrom"); got != 1 {
			t.Errorf("newProxyWithStatePath synthesizeImplicitRoutesFrom calls = %d, want 1 build-derived eligibility use", got)
		}
		if got := namedCallWithArgsCount(constructor.Body, "synthesizeImplicitRoutesFrom", "cfg", "built", "Eligible"); got != 1 {
			t.Errorf("newProxyWithStatePath must pass exactly (cfg, built.Eligible), matches = %d", got)
		}
		if got := namedCallCountInNode(constructor.Body, "loggedInProviders"); got != 0 {
			t.Errorf("newProxyWithStatePath calls loggedInProviders %d time(s), re-reading account eligibility", got)
		}
		if got := namedCallCountInNode(constructor.Body, "synthesizeImplicitRoutes"); got != 0 {
			t.Errorf("newProxyWithStatePath re-reads account eligibility %d time(s)", got)
		}
		reload := namedMethod(t, proxyFile, "Proxy", "reload")
		if got := namedCallCountInNode(reload.Body, "synthesizeImplicitRoutesFrom"); got != 1 {
			t.Errorf("Proxy.reload synthesizeImplicitRoutesFrom calls = %d, want 1 build-derived eligibility use", got)
		}
		if got := namedCallWithArgsCount(reload.Body, "synthesizeImplicitRoutesFrom", "cfg", "built", "Eligible"); got != 1 {
			t.Errorf("Proxy.reload must pass exactly (cfg, built.Eligible), matches = %d", got)
		}
		if got := namedCallCountInNode(reload.Body, "loggedInProviders"); got != 0 {
			t.Errorf("Proxy.reload calls loggedInProviders %d time(s), re-reading account eligibility", got)
		}
		if got := namedCallCountInNode(reload.Body, "synthesizeImplicitRoutes"); got != 0 {
			t.Errorf("Proxy.reload re-reads account eligibility %d time(s)", got)
		}
	})

	t.Run("internal pricing remains a repository-leaf package", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/pricing")
	})

	t.Run("internal observe events owns the live-event hub", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/observe/events")
		adapter, _ := parseGoFile(t, "live_events.go")
		functions := map[string]int{
			"serveEvents": 0,
		}
		for _, decl := range adapter.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				if decl.Tok != token.IMPORT {
					t.Error("live_events.go must not declare package state or types; it is only a Proxy adapter")
				}
			case *ast.FuncDecl:
				if _, ok := functions[decl.Name.Name]; !ok {
					t.Errorf("live_events.go has unexpected function %s; the SSE handler belongs in internal/observe/events", decl.Name.Name)
					continue
				}
				functions[decl.Name.Name]++
			default:
				t.Errorf("live_events.go has unexpected top-level declaration %T", decl)
			}
		}
		for name, count := range functions {
			if count != 1 {
				t.Errorf("live_events.go %s declarations = %d, want exactly 1", name, count)
			}
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
		assertRepositoryLeafPackage(t, "internal/observe/requestlog")
		if _, err := os.Stat("request_log.go"); err == nil {
			t.Error("legacy root request_log.go must not exist; request-log mechanics belong in internal/observe/requestlog")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat request_log.go: %v", err)
		}

		adapter, _ := parseGoFile(t, "request_log_adapter.go")
		wantImports := map[string]bool{
			"log":      true,
			"net/http": true,
			"time":     true,
			"model-proxy/internal/observe/requestlog": true,
		}
		wantFunctions := map[string]int{
			"requestLogInput":    0,
			"completeRequestLog": 0,
			"initRequestLog":     0,
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

	t.Run("internal observe stats owns SQLite persistence", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/observe/stats")
		if _, err := os.Stat("stats.go"); err == nil {
			t.Error("legacy root stats.go must not exist; persistence belongs in internal/observe/stats and runtime projection in stats_runtime.go")
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

		runtime, _ := parseGoFile(t, "stats_runtime.go")
		reset := namedMethod(t, runtime, "statsFlusher", "reset")
		for _, field := range reset.Type.Params.List {
			if typeContainsIdent(field.Type, "Proxy") {
				t.Error("statsFlusher.reset must not accept *Proxy; cache reset and composition stay at Proxy.resetStats")
			}
		}
	})

	t.Run("internal observe cache owns exact-response storage", func(t *testing.T) {
		assertRepositoryLeafPackage(t, "internal/cache")
		if _, err := os.Stat("cache.go"); err == nil {
			t.Error("legacy root cache.go must not exist; cache mechanics belong in internal/cache")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat cache.go: %v", err)
		}

		adapter, _ := parseGoFile(t, "cache_adapter.go")
		for _, spec := range adapter.Imports {
			if path := strings.Trim(spec.Path.Value, `"`); path != "model-proxy/internal/cache" {
				t.Errorf("cache_adapter.go has unexpected import %q", path)
			}
		}
		functions := map[string]int{"newResponseCache": 0}
		for _, decl := range adapter.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				if decl.Tok != token.IMPORT {
					t.Error("cache_adapter.go must not declare package state or types")
				}
			case *ast.FuncDecl:
				if _, ok := functions[decl.Name.Name]; !ok {
					t.Errorf("cache_adapter.go has unexpected function %s", decl.Name.Name)
					continue
				}
				functions[decl.Name.Name]++
			default:
				t.Errorf("cache_adapter.go has unexpected top-level declaration %T", decl)
			}
		}
		if functions["newResponseCache"] != 1 {
			t.Errorf("cache_adapter.go newResponseCache declarations = %d, want exactly 1", functions["newResponseCache"])
		}

		assertCacheStoreField := func(parsed *ast.File, owner string) {
			t.Helper()
			fieldType := namedStructFields(t, parsed, owner)["cache"]
			pointer, ok := fieldType.(*ast.StarExpr)
			if !ok {
				t.Errorf("%s.cache type = %T, want *responsecache.Store", owner, fieldType)
			} else if name, ok := configSelectorName(pointer.X, "responsecache"); !ok || name != "Store" {
				t.Errorf("%s.cache must be *responsecache.Store", owner)
			}
		}
		assertCacheStoreField(rootPackage, "Proxy")
		dispatchContext, _ := parseGoFile(t, "dispatch_context.go")
		assertCacheStoreField(dispatchContext, "runtimeSnapshot")

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

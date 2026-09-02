package archtest

import (
	"go/ast"
	"go/token"
	"testing"
)

// TestArchitectureWebBoundaries keeps HTTP/UI transport independent from the
// composition root and makes the two application ports the only crossing.
func TestArchitectureWebBoundaries(t *testing.T) {
	t.Run("transport imports only reviewed internal collaborators", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/web")
	})

	t.Run("transport owns server task and session state", func(t *testing.T) {
		transport, _ := parseGoPackage(t, "internal/web")
		fields := namedStructFields(t, transport, "Server")
		if got := expressionPath(fields["reads"]); got != "appapi.ReadAPI" {
			t.Errorf("Server.reads type = %q, want appapi.ReadAPI", got)
		}
		if got := expressionPath(fields["commands"]); got != "appapi.CommandAPI" {
			t.Errorf("Server.commands type = %q, want appapi.CommandAPI", got)
		}
		if got := simpleTypeName(fields["tasks"]); got != "*taskOwner" {
			t.Errorf("Server.tasks type = %q, want *taskOwner", got)
		}
		if got := simpleTypeName(fields["sessions"]); got != "*sessionStore" {
			t.Errorf("Server.sessions type = %q, want *sessionStore", got)
		}
		for _, violation := range exactFieldSetViolations(fields, map[string]bool{
			"reads":     true,
			"commands":  true,
			"events":    true,
			"adminAuth": true, // S2: injected closure sourcing the admin bearer check
			"version":   true,
			"assets":    true,
			"assetRoot": true,
			"logFile":   true,
			// Immutable startup input used only by the transport-owned browser
			// origin guard; the composition adapter does not retain it.
			"browserListen": true,
			"tasks":         true,
			"sessions":      true,
		}) {
			t.Errorf("internal/web.Server field boundary: %s", violation)
		}
		// events is the injected SSE port (the composition root binds the
		// proxy-owned events hub; the transport only routes and guards it).
		if !typeContainsIdent(fields["events"], "HandlerFunc") {
			t.Errorf("Server.events type = %q, want http.HandlerFunc port", simpleTypeName(fields["events"]))
		}
		for name, fieldType := range fields {
			if typeContainsIdent(fieldType, "Proxy") {
				t.Errorf("internal/web.Server.%s must not retain Proxy", name)
			}
		}
		if got := goStatementCount(transport); got != 1 {
			t.Errorf("internal/web bare goroutines = %d, want exactly taskOwner.Run", got)
		}
		tasks, _ := parseGoFile(t, "internal/web/tasks.go")
		if got := goStatementCount(tasks); got != 1 {
			t.Errorf("internal/web/tasks.go bare goroutines = %d, want 1", got)
		}
	})

	t.Run("root adapter is composition only", func(t *testing.T) {
		adapter, _ := parseGoFile(t, "internal/app/web_adapter.go")
		fields := namedStructFields(t, adapter, "WebServer")
		if !typeContainsIdent(fields["server"], "Server") {
			t.Error("WebServer.server must retain internal/web.Server")
		}
		apiType, ok := fields["api"].(*ast.StarExpr)
		if !ok || expressionPath(apiType.X) != "admin.Service" {
			t.Errorf("WebServer.api type = %q, want *admin.Service", simpleTypeName(fields["api"]))
		}
		for _, violation := range exactFieldSetViolations(fields, map[string]bool{
			"server":          true,
			"api":             true,
			"adminAuth":       true, // S2: injected closure sourcing the admin bearer check
			"configFile":      true,
			"logFile":         true,
			"events":          true,
			"newAqpClientFn":  true,
			"newCodexOptions": true,
		}) {
			t.Errorf("WebServer field boundary: %s", violation)
		}
		for name, fieldType := range fields {
			if typeContainsIdent(fieldType, "Proxy") {
				t.Errorf("WebServer.%s must not retain *Proxy", name)
			}
		}
		allowedMethods := map[string]bool{
			"Register":   true,
			"Start":      true,
			"Close":      true,
			"Serve":      true,
			"SetLogFile": true,
		}
		for name := range receiverMethodNames(t, []string{"internal/app/web_adapter.go"}, "WebServer") {
			if !allowedMethods[name] {
				t.Errorf("web_adapter.go owns non-composition method %s", name)
			}
		}
		if got := goStatementCount(adapter); got != 0 {
			t.Errorf("web_adapter.go starts %d bare goroutine(s)", got)
		}
	})

	t.Run("admin service owns the application ports", func(t *testing.T) {
		assertInternalPackageImportPolicy(t, "internal/admin")

		// The read/command logic lives in internal/admin: Service carries only
		// its consumer-owned ports and implements both appapi interfaces (the
		// compile-time assertions in commands.go keep this honest).
		adminPackage, adminSet := parseGoPackage(t, "internal/admin")
		fields := namedStructFields(t, adminPackage, "Service")
		for _, violation := range exactFieldSetViolations(fields, map[string]bool{
			"ports": true,
		}) {
			t.Errorf("admin.Service field boundary: %s", violation)
		}
		for name, fieldType := range fields {
			if typeContainsIdent(fieldType, "Proxy") {
				t.Errorf("admin.Service.%s must not retain *Proxy", name)
			}
		}
		for _, sentinel := range []string{"Dashboard", "Accounts", "SaveConfig", "EditConfig", "BeginLogin", "AddPreset"} {
			if !methodDeclared(adminPackage, sentinel) {
				t.Errorf("internal/admin no longer owns application port method %s", sentinel)
			}
		}
		for _, forbidden := range []string{"ResponseWriter", "Request", "ServeMux", "ServeHTTP"} {
			for _, site := range selectorSitesNamed(adminPackage, adminSet, forbidden) {
				t.Errorf("internal/admin depends on HTTP transport %s: %s", forbidden, site)
			}
		}
		if got := goStatementCount(adminPackage); got != 0 {
			t.Errorf("internal/admin starts %d bare goroutine(s)", got)
		}

		// The composition root must not re-grow the old in-app adapter types.
		rootPackage, _ := parseGoPackage(t, "internal/app")
		for _, legacy := range []string{"proxyWebAPI", "proxyReadView", "proxyAdminCommands"} {
			if typeDeclared(rootPackage, legacy) {
				t.Errorf("internal/app re-declares web adapter type %s; the admin service lives in internal/admin", legacy)
			}
		}
	})

	t.Run("all admin config writers share one lock boundary", func(t *testing.T) {
		wholeWriteFile := mustParseFile(t, "internal/admin/config_write.go")
		wholeWrite := namedMethod(t, wholeWriteFile, "Service", "saveAndReload")
		if got := namedCallCountInNode(wholeWrite.Body, "WithConfigLock"); got != 1 {
			t.Errorf("saveAndReload WithConfigLock calls = %d, want 1", got)
		}
		if got := namedCallCountInNode(wholeWrite.Body, "saveAndReloadUnderLock"); got != 1 {
			t.Errorf("saveAndReload saveAndReloadUnderLock calls = %d, want 1", got)
		}

		structuredFile := mustParseFile(t, "internal/admin/config_edit.go")
		structuredWrite := namedMethod(t, structuredFile, "Service", "editConfigNode")
		if got := namedCallCountInNode(structuredWrite.Body, "WithConfigLock"); got != 1 {
			t.Errorf("editConfigNode WithConfigLock calls = %d, want 1", got)
		}
		if got := namedCallCountInNode(structuredWrite.Body, "saveAndReloadUnderLock"); got != 1 {
			t.Errorf("editConfigNode saveAndReloadUnderLock calls = %d, want 1", got)
		}
		if got := namedCallCountInNode(structuredWrite.Body, "saveAndReload"); got != 0 {
			t.Errorf("editConfigNode nested saveAndReload calls = %d, want 0", got)
		}
	})
}

// typeDeclared reports whether the package view declares a type with the
// given name.
func typeDeclared(file *ast.File, name string) bool {
	for _, decl := range file.Decls {
		generic, ok := decl.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, spec := range generic.Specs {
			if typeSpec, ok := spec.(*ast.TypeSpec); ok && typeSpec.Name.Name == name {
				return true
			}
		}
	}
	return false
}

func selectorSitesNamed(file *ast.File, fset *token.FileSet, name string) []string {
	var sites []string
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == name {
			sites = append(sites, describe(fset, selector, name))
		}
		return true
	})
	return sites
}

func exactFieldSetViolations(fields map[string]ast.Expr, allowed map[string]bool) []string {
	var violations []string
	for name := range fields {
		if !allowed[name] {
			violations = append(violations, "unexpected field "+name)
		}
	}
	for name := range allowed {
		if _, ok := fields[name]; !ok {
			violations = append(violations, "missing field "+name)
		}
	}
	return sortedNames(violations)
}

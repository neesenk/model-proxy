package archtest

import (
	"go/ast"
	"go/token"
	"strings"
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
			"version":   true,
			"assets":    true,
			"assetRoot": true,
			"logFile":   true,
			"tasks":     true,
			"sessions":  true,
		}) {
			t.Errorf("internal/web.Server field boundary: %s", violation)
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
		if got := simpleTypeName(fields["api"]); got != "*proxyWebAPI" {
			t.Errorf("WebServer.api type = %q, want *proxyWebAPI", got)
		}
		for _, violation := range exactFieldSetViolations(fields, map[string]bool{
			"server":          true,
			"api":             true,
			"configFile":      true,
			"logFile":         true,
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

	t.Run("application adapter retains capabilities not Proxy", func(t *testing.T) {
		adapter, fset := parseGoFile(t, "internal/app/proxy_web_api.go")
		fields := namedStructFields(t, adapter, "proxyWebAPI")
		if got := simpleTypeName(fields["reads"]); got != "proxyReadView" {
			t.Errorf("proxyWebAPI.reads type = %q, want proxyReadView", got)
		}
		if got := simpleTypeName(fields["commands"]); got != "proxyAdminCommands" {
			t.Errorf("proxyWebAPI.commands type = %q, want proxyAdminCommands", got)
		}
		for _, violation := range exactFieldSetViolations(fields, map[string]bool{
			"reads":           true,
			"commands":        true,
			"configFile":      true,
			"newAqpClientFn":  true,
			"newCodexOptions": true,
		}) {
			t.Errorf("proxyWebAPI field boundary: %s", violation)
		}
		for name, fieldType := range fields {
			if typeContainsIdent(fieldType, "Proxy") {
				t.Errorf("proxyWebAPI.%s must not retain *Proxy", name)
			}
		}
		for name := range receiverMethodNames(t, []string{"internal/app/proxy_web_api.go"}, "proxyWebAPI") {
			if strings.HasPrefix(name, "handle") || strings.HasPrefix(name, "serve") {
				t.Errorf("proxy_web_api.go restores HTTP transport method %s", name)
			}
		}
		for _, forbidden := range []string{"ResponseWriter", "Request", "ServeMux", "ServeHTTP"} {
			for _, site := range selectorSitesNamed(adapter, fset, forbidden) {
				t.Errorf("proxy_web_api.go depends on HTTP transport %s: %s", forbidden, site)
			}
		}
		if got := goStatementCount(adapter); got != 0 {
			t.Errorf("proxy_web_api.go starts %d bare goroutine(s)", got)
		}
		if got := selectorCallsOnIdent(adapter, "proxy"); !sameNames(got, []string{"adminCommands", "readView"}) {
			t.Errorf("newProxyWebAPI Proxy calls = %v, want [adminCommands readView]", got)
		}
	})
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

func selectorCallsOnIdent(file *ast.File, receiver string) []string {
	var names []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if ok && identifier.Name == receiver {
			names = append(names, selector.Sel.Name)
		}
		return true
	})
	return sortedNames(names)
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
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

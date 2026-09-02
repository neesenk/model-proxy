// Package login is the transport-neutral core of provider login and account
// pool mutation: the codex OAuth device flow, the AQP (company Google SSO)
// gateway client, and the apikey/volcengine pool add/remove/validate logic.
// It returns values and errors only — all stdin prompting, stdout printing,
// flag parsing, and daemon reload nudging live in the interactive shell
// (internal/cli/login), which adapts this core for the terminal. The web layer
// (internal/app) drives the same core directly.
package login

package main

// runtimeGenerationArg keeps compatibility with callers that omit a generation
// while letting request-scoped paths bind runtime mutations to their captured
// reload generation.
func runtimeGenerationArg(generations []uint64) uint64 {
	if len(generations) == 0 {
		return 0
	}
	return generations[0]
}

// providerConfig resolves the Provider config for name, resolving a
// credential-pool virtual id ("name#<accountID>") back to its parent. parentOf
// is the snapshot taken under p.mu alongside cfg; for a non-virtual name (incl.
// single-account providers), parentOf[name] is "" and the config is read
// directly. Returns the zero Provider (ok=false) if neither name nor a parent
// is found — callers treat that as an unknown provider.
func providerConfig(cfg *Config, parentOf map[string]string, name string) (Provider, bool) {
	if parent := parentOf[name]; parent != "" {
		name = parent
	}
	p, ok := cfg.Providers[name]
	return p, ok
}

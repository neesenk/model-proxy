package providerbuild

import "model-proxy/internal/provider"

// The legacy singular volcengine credential file is owned by the provider
// package (internal/provider/volcengine_legacy.go): reading it must funnel
// through credstore, and the dependency DAG forbids providerbuild from
// importing credstore directly. These aliases keep the historical call sites
// stable.
type VolcengineCreds = provider.VolcengineCreds

func LoadVolcengineCreds(homeDir, provName string) (*VolcengineCreds, error) {
	return provider.LoadVolcengineCreds(homeDir, provName)
}

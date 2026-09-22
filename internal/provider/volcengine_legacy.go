package provider

import (
	"encoding/json"
	"path/filepath"

	"model-proxy/internal/credstore"
)

// VolcengineCreds is the on-disk format of the legacy singular volcengine
// apikey file (<name>_apikey.json): the Ark API Key (chat) plus the
// Volcengine AK/SK (GetAFPUsage / signed OpenAPI listings).
type VolcengineCreds struct {
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// LoadVolcengineCreds reads the legacy singular volcengine credential file
// through credstore, so keychain-mode credentials reach this path too (a raw
// os.ReadFile would only see the archived .migrated.bak and silently report
// "not logged in"). It lives in the provider package — the DAG forbids
// providerbuild from importing credstore, and the provider owns volcengine
// credential I/O.
func LoadVolcengineCreds(homeDir, provName string) (*VolcengineCreds, error) {
	path := filepath.Join(homeDir, ".model-proxy", provName+"_apikey.json")
	b, err := credstore.NewRef(path).Load()
	if err != nil {
		return nil, err
	}
	var c VolcengineCreds
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

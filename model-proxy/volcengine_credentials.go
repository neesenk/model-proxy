package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// volcengineCreds is the on-disk format of the volcengine apikey file: the Ark
// API Key (chat) plus the Volcengine AK/SK (GetAFPUsage).
type volcengineCreds struct {
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

func loadVolcengineCreds(provName string) (*volcengineCreds, error) {
	path := filepath.Join(homeDir(), ".model-proxy", provName+"_apikey.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c volcengineCreds
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

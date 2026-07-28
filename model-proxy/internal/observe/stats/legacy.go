package stats

import (
	"encoding/json"
	"fmt"
	"os"
)

type legacyTokenUsage struct {
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Requests      uint64 `json:"requests"`
}

// ImportLegacyTokens imports the old token_usage.json map into one baseline
// minute. Import is gated on an empty minute_buckets table and never deletes or
// modifies the source file.
func (s *Store) ImportLegacyTokens(path string) (int, error) {
	empty, err := s.isEmpty()
	if err != nil {
		return 0, err
	}
	if !empty {
		return 0, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var flat map[string]legacyTokenUsage
	if err := json.Unmarshal(data, &flat); err != nil {
		return 0, fmt.Errorf("parse legacy token_usage.json: %w", err)
	}

	var minute int64
	if fileInfo, err := os.Stat(path); err == nil {
		minute = fileInfo.ModTime().Unix() / 60 * 60
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`INSERT INTO minute_buckets
		(provider, model, minute, input, output, cache_creation, cache_read, token_requests)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	imported := 0
	for rawKey, usage := range flat {
		provider, model := splitLegacyKey(rawKey)
		if _, err := stmt.Exec(
			provider,
			model,
			minute,
			usage.Input,
			usage.Output,
			usage.CacheCreation,
			usage.CacheRead,
			usage.Requests,
		); err != nil {
			return imported, err
		}
		imported++
	}
	if err := tx.Commit(); err != nil {
		return imported, err
	}
	return imported, nil
}

func splitLegacyKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// exportPricingFromDB reads all rows from AIS Switch's model_pricing table.
// Uses modernc.org/sqlite (pure Go, no CGO) so it cross-compiles to Linux.
func exportPricingFromDB(dbPath string) ([]ModelPricing, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("db not found: %w", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT model_id, display_name, input_cost_per_million,
		output_cost_per_million, cache_read_cost_per_million, cache_creation_cost_per_million
		FROM model_pricing ORDER BY model_id`)
	if err != nil {
		return nil, fmt.Errorf("query model_pricing: %w", err)
	}
	defer rows.Close()
	var out []ModelPricing
	for rows.Next() {
		var p ModelPricing
		if err := rows.Scan(&p.ModelID, &p.DisplayName, &p.InputCostPerMillion,
			&p.OutputCostPerMillion, &p.CacheReadCostPerMillion, &p.CacheCreationCostPerMillion); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

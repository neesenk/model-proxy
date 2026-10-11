package requestlog

// keyusage.go — per-forward-key usage aggregation over LLM forward records:
// one (key, model, provider) cell per group with request/failure counts,
// token sums and last-use. Two producers answer identically — the JSONL
// directory scan (KeyUsage) and the SQLite index (Indexer.KeyUsage) — the
// same twin contract as ShadowReport. Shadow evaluation records are proxy-
// side experiments, not client traffic, and never count toward a key.

import (
	"sort"
	"time"
)

// KeyUsageEntry is one (api_key, model, provider) aggregation cell. LastUsedMs
// is epoch milliseconds of the newest matching record (0 = none).
type KeyUsageEntry struct {
	APIKeyID   string `json:"api_key_id"`
	Model      string `json:"model"` // called (exposed) model
	Provider   string `json:"provider"`
	Requests   int64  `json:"requests"`
	Failures   int64  `json:"failures"` // status >= 400
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	LastUsedMs int64  `json:"last_used_ms"`
}

// KeyUsage aggregates per-key usage from the JSONL directory scan. keyID ""
// aggregates EVERY key (the key list's summary view); a specific id narrows
// to one key (the key's usage view). The window rides filter.From/To; kind
// and shadow scoping is applied here so callers cannot forget it.
func KeyUsage(dir string, filter Filter, keyID string) ([]KeyUsageEntry, error) {
	filter.Kind = "llm"
	filter.UsageOnly = true
	filter.Limit = 0
	records, err := query(dir, filePrefix, filter, false, nil)
	if err != nil {
		return nil, err
	}
	return aggregateKeyUsage(records, keyID), nil
}

// aggregateKeyUsage folds scan records into entries. Sorting: by requests
// desc, then model asc — the usage table's default order.
func aggregateKeyUsage(records []Record, keyID string) []KeyUsageEntry {
	type cell struct {
		entry KeyUsageEntry
	}
	cells := map[string]*cell{}
	for i := range records {
		rec := &records[i]
		if rec.APIKeyID == "" || rec.Shadow || rec.AttemptOnly {
			continue
		}
		if keyID != "" && rec.APIKeyID != keyID {
			continue
		}
		k := rec.APIKeyID + "\x1f" + rec.CalledModel + "\x1f" + rec.Provider
		c := cells[k]
		if c == nil {
			c = &cell{entry: KeyUsageEntry{APIKeyID: rec.APIKeyID, Model: rec.CalledModel, Provider: rec.Provider}}
			cells[k] = c
		}
		c.entry.Requests++
		if rec.Status >= 400 {
			c.entry.Failures++
		}
		c.entry.Input += int64(rec.ParsedUsage.Input)
		c.entry.Output += int64(rec.ParsedUsage.Output)
		if ts, err := time.Parse(time.RFC3339, rec.Ts); err == nil {
			if ms := ts.UnixMilli(); ms > c.entry.LastUsedMs {
				c.entry.LastUsedMs = ms
			}
		}
	}
	out := make([]KeyUsageEntry, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// KeyUsage aggregates per-key usage from the SQLite index — same semantics as
// the directory scan (KeyUsage): LLM records only, shadow excluded, keyID ""
// = all keys, window from filter.From/To.
func (x *Indexer) KeyUsage(filter Filter, keyID string) ([]KeyUsageEntry, error) {
	if x == nil {
		return nil, nil
	}
	where := ` WHERE kind = '' AND shadow = 0 AND attempt_only = 0 AND api_key != ''`
	args := []any{}
	if keyID != "" {
		where += ` AND api_key = ?`
		args = append(args, keyID)
	}
	if !filter.From.IsZero() {
		where += ` AND ts_ms > 0 AND ts_ms >= ?`
		args = append(args, filter.From.UnixMilli())
	}
	if !filter.To.IsZero() {
		where += ` AND ts_ms <= ?`
		args = append(args, filter.To.UnixMilli())
	}
	rows, err := x.db.Query(`SELECT api_key, called_model, provider,
		COUNT(*), SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),
		SUM(input), SUM(output), MAX(ts_ms)
		FROM records`+where+` GROUP BY api_key, called_model, provider`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyUsageEntry{}
	for rows.Next() {
		var e KeyUsageEntry
		var in, outv any
		if err := rows.Scan(&e.APIKeyID, &e.Model, &e.Provider, &e.Requests, &e.Failures, &in, &outv, &e.LastUsedMs); err != nil {
			return nil, err
		}
		if in != nil {
			e.Input, _ = in.(int64)
		}
		if outv != nil {
			e.Output, _ = outv.(int64)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

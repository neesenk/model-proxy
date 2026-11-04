package stats

import (
	"database/sql"
	"fmt"
	"time"

	"model-proxy/internal/observe/logx"
)

// ledgerCutoffISO is the wall-clock moment the normalized usage ledger went
// live in the deployed daemon (commit 89630e1, 2026-10-13 03:36:31Z). Buckets
// committed before it carry the two historical mis-ledgers — cached shares
// landing in BOTH input and cache_read (double count), and gateway-chunk
// repetition inflating totals (fixed by 2537235) — while the request-log
// index has held the correct ledger all along. RebuildTokenLedger rewrites
// the pre-cutoff token columns from that index.
const ledgerCutoffISO = "2026-10-13T03:36:31Z"

// Physical per-request ceilings for the impossible-ledger sweep: no model
// emits >128k output tokens or reads a >2M-token prompt in one request
// (largest context windows are ≤2M). A committed ledger beyond these bounds
// is scanner-era corruption, not usage — the pre-2537235 additive merge
// inflated one 16-request day (2026-10-11, step-plan/step-5-preview) into
// 116M output and 2.24B input tokens; the rebuild's count guard skipped
// those buckets and the request-log source rows have since been rotated
// away, so repairImpossibleLedgers zeroes their token columns (requests and
// timings stay true) instead of leaving the phantom totals poisoning
// cumulative usage, cost and tok/s.
const (
	maxOutputTokensPerRequest = 128 << 10
	maxInputTokensPerRequest  = 2 << 20
)

// ledgerMinute is one aggregated (provider, model, minute) bucket candidate.
type ledgerMinute struct {
	provider, model string
	minute          int64
	inTok, outTok   int64
	ccTok, crTok    int64
	n               int64 // all non-attempt-only records (≈ the legacy EvRequests count, 4xx storms included)
	n200            int64 // 2xx records only (the console-aligned request count)
}

type ledgerAgentRow struct {
	agent, provider, model string
	minute                 int64
	inTok, outTok          int64
	ccTok, crTok           int64
}

// RebuildTokenLedger rewrites the token columns (input/output/cache_creation/
// cache_read) of minute_buckets AND agent_buckets from the request-log index
// for buckets committed before the ledger cutoff, AND repairs the minute
// requests column across ALL history (2xx-only count: committed terminal 4xx
// responses were counted as requests until the success-only fix — a
// bad-body 400 storm inflated one provider-day 4x). Idempotent: a rebuilt
// bucket equals its aggregate, so reruns rewrite the same values. Defensive:
// a minute bucket whose request-log record count diverges from its committed
// token_requests count by more than 10% is left untouched (a log gap must not
// zero out real counters) and counted as skipped; the requests repair only
// fires when the ALL-record count matches the committed requests within 10%
// (the legacy counter included 4xx). The index is read over its own
// connection; the rewrites run in one transaction on the stats store.
// Returns (rebuiltBuckets, skippedMinuteBuckets).
func (s *Store) RebuildTokenLedger(indexDBPath string) (rebuilt, skipped int64, err error) {
	if s == nil || s.db == nil {
		return 0, 0, nil
	}
	// The impossible-ledger sweep needs no request-log source and runs even
	// when the index is unavailable or empty (its rows may be long gone).
	reset, err := s.repairImpossibleLedgers()
	if err != nil {
		return 0, 0, err
	}
	if indexDBPath == "" {
		return reset, 0, nil
	}
	idx, err := sql.Open("sqlite", indexDBPath)
	if err != nil {
		return reset, 0, fmt.Errorf("open request-log index: %w", err)
	}
	defer idx.Close()

	minutes, agents, err := readLedgerAggregates(idx)
	if err != nil {
		return 0, 0, err
	}
	if len(minutes) == 0 && len(agents) == 0 {
		if reset > 0 {
			logx.Infof("[stats] reset %d impossible ledger bucket(s) (per-request physical bounds; no request-log rows to rebuild from)", reset)
		}
		return reset, 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	cutoffEpoch := int64(0)
	if t, err := time.Parse(time.RFC3339, ledgerCutoffISO); err == nil {
		cutoffEpoch = t.Unix()
	}
	for _, m := range minutes {
		if m.minute >= cutoffEpoch {
			// Post-cutoff buckets already carry the corrected token ledger —
			// only the requests repair applies to them.
			resR, err := tx.Exec(`UPDATE minute_buckets SET requests = ?
				WHERE provider = ? AND model = ? AND minute = ?
				  AND requests != ?
				  AND requests BETWEEN ? * 0.9 AND ? * 1.1`,
				m.n200, m.provider, m.model, m.minute, m.n200, m.n200, m.n)
			if err != nil {
				return 0, 0, fmt.Errorf("repair minute requests: %w", err)
			}
			nr, _ := resR.RowsAffected()
			rebuilt += nr
			continue
		}
		res, err := tx.Exec(`UPDATE minute_buckets SET
			input = ?, output = ?, cache_creation = ?, cache_read = ?, requests = ?
			WHERE provider = ? AND model = ? AND minute = ?
			  AND ? BETWEEN token_requests * 0.9 AND token_requests * 1.1
			  AND requests BETWEEN ? * 0.9 AND ? * 1.1`,
			m.inTok, m.outTok, m.ccTok, m.crTok, m.n200, m.provider, m.model, m.minute, m.n200, m.n200, m.n)
		if err != nil {
			return 0, 0, fmt.Errorf("rebuild minute bucket: %w", err)
		}
		n, _ := res.RowsAffected()
		rebuilt += n
		if n == 0 {
			// A pre-cutoff aggregate that did not rewrite its bucket: the
			// count defense rejected it (or the bucket vanished) — the
			// committed values stand.
			skipped++
		}
	}

	stmtA, err := tx.Prepare(`UPDATE agent_buckets SET
		input = ?, output = ?, cache_creation = ?, cache_read = ?
		WHERE agent = ? AND provider = ? AND model = ? AND minute = ?`)
	if err != nil {
		return 0, 0, err
	}
	defer stmtA.Close()
	for _, a := range agents {
		res, err := stmtA.Exec(a.inTok, a.outTok, a.ccTok, a.crTok, a.agent, a.provider, a.model, a.minute)
		if err != nil {
			return 0, 0, fmt.Errorf("rebuild agent bucket: %w", err)
		}
		n, _ := res.RowsAffected()
		rebuilt += n
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if rebuilt > 0 || skipped > 0 || reset > 0 {
		logx.Infof("[stats] rebuilt %d historical token bucket(s) from the request-log ledger (%d skipped on count mismatch; %d impossible ledger bucket(s) reset; cutoff %s)", rebuilt, skipped, reset, ledgerCutoffISO)
	}
	return rebuilt + reset, skipped, nil
}

// repairImpossibleLedgers zeroes the token columns of buckets whose committed
// ledger is beyond physical per-request bounds (see maxOutputTokensPerRequest
// / maxInputTokensPerRequest) in BOTH dimensions. Idempotent: a zeroed bucket
// no longer exceeds any bound, so reruns are no-ops. Requests, token_requests
// and the timing sums are preserved — only the phantom token ledger goes.
func (s *Store) repairImpossibleLedgers() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 64-bit safe: requests is bounded by real traffic; requests×2M ≪ 2^63.
	resM, err := tx.Exec(`UPDATE minute_buckets SET
			input = 0, output = 0, cache_creation = 0, cache_read = 0
		WHERE requests > 0 AND (
			output > requests * ? OR input > requests * ?)`,
		maxOutputTokensPerRequest, maxInputTokensPerRequest)
	if err != nil {
		return 0, fmt.Errorf("reset impossible minute ledgers: %w", err)
	}
	reset, _ := resM.RowsAffected()

	resA, err := tx.Exec(`UPDATE agent_buckets SET
			input = 0, output = 0, cache_creation = 0, cache_read = 0
		WHERE requests > 0 AND (
			output > requests * ? OR input > requests * ?)`,
		maxOutputTokensPerRequest, maxInputTokensPerRequest)
	if err != nil {
		return 0, fmt.Errorf("reset impossible agent ledgers: %w", err)
	}
	n, _ := resA.RowsAffected()
	reset += n
	return reset, tx.Commit()
}

// readLedgerAggregates pulls the pre-cutoff usage ledger out of the
// request-log index: per-(provider, model, minute) with the record-count
// guard column, and per-(agent, provider, model, minute). attempt_only legs
// are excluded (never committed to the buckets).
func readLedgerAggregates(idx *sql.DB) ([]ledgerMinute, []ledgerAgentRow, error) {
	rows, err := idx.Query(`SELECT provider, upstream_model AS model,
			(CAST(strftime('%s', ts) AS INTEGER)/60)*60 AS minute,
			SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read), COUNT(*),
			SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END)
		FROM records
		WHERE attempt_only = 0
		GROUP BY 1, 2, 3`)
	if err != nil {
		return nil, nil, fmt.Errorf("aggregate request-log ledger: %w", err)
	}
	defer rows.Close()
	var minutes []ledgerMinute
	for rows.Next() {
		var m ledgerMinute
		if err := rows.Scan(&m.provider, &m.model, &m.minute, &m.inTok, &m.outTok, &m.ccTok, &m.crTok, &m.n, &m.n200); err != nil {
			return nil, nil, err
		}
		minutes = append(minutes, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	arows, err := idx.Query(`SELECT agent, provider, upstream_model AS model,
			(CAST(strftime('%s', ts) AS INTEGER)/60)*60 AS minute,
			SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read)
		FROM records
		WHERE attempt_only = 0 AND agent != '' AND ts < ?
		GROUP BY 1, 2, 3, 4`, ledgerCutoffISO)
	if err != nil {
		return nil, nil, fmt.Errorf("aggregate request-log agent ledger: %w", err)
	}
	defer arows.Close()
	var agents []ledgerAgentRow
	for arows.Next() {
		var a ledgerAgentRow
		if err := arows.Scan(&a.agent, &a.provider, &a.model, &a.minute, &a.inTok, &a.outTok, &a.ccTok, &a.crTok); err != nil {
			return nil, nil, err
		}
		agents = append(agents, a)
	}
	if err := arows.Err(); err != nil {
		return nil, nil, err
	}
	return minutes, agents, nil
}

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// forwardLogCtx carries per-forward log context from forward() into tryTarget(),
// where the upstream response (and thus the capture point) lives. requestID
// groups one client request's failover attempts; attempt is the 0-based index
// in the failover chain; exposed is the route name (post claude_mapping).
type forwardLogCtx struct {
	requestID string
	attempt   int
	exposed   string
	origBody  []byte // original client body (pre-rewrite, pre-convert) — for faithful replay
}

// requestLogRecord is one line in the JSONL request log. Written as one JSON
// object per line; request_body/response_body are JSON-escaped so newlines in
// the bodies don't break the one-record-per-line format.
type requestLogRecord struct {
	Ts              string `json:"ts"` // RFC3339, UTC
	Shadow          bool   `json:"shadow,omitempty"`
	RequestID       string `json:"request_id"`
	SessionID       string `json:"session_id"`
	Protocol        string `json:"protocol"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	CalledModel     string `json:"called_model"`
	UpstreamModel   string `json:"upstream_model"`
	Exposed         string `json:"exposed"`
	Provider        string `json:"provider"`
	Attempt         int    `json:"attempt"`
	Status          int    `json:"status"`
	LatencyMs       int64  `json:"latency_ms"`
	RequestSize     int    `json:"request_size"`
	ResponseSize    int64  `json:"response_size"`
	RequestBody     string `json:"request_body"`
	ResponseBody    string `json:"response_body"`
	ResponseHeaders string `json:"response_headers,omitempty"`
}

// fileTimeLayout is the timestamp format embedded in log file names:
//
//	requests-20260713-150405.log                       (active)
//	requests-20260713-150405--20260714-090000-1.log    (rotated: start--end-seq)
const fileTimeLayout = "20060102-150405"

// logFileMode is the mode for request-log files. The bodies contain user code
// and prompts (sensitive), so files are owner-only read/write (0o600), matching
// the sibling credential files under ~/.model-proxy/.
const logFileMode = 0o600

// requestFileWriter owns the current log file and the size/day rotation policy.
// It is NOT goroutine-safe - only the logger's single background goroutine
// touches it. `now` is passed into each method so rotation (notably the
// day-boundary check) is unit-testable without sleeping.
type requestFileWriter struct {
	dir       string
	maxSize   int64
	f         *os.File
	curPath   string
	curStart  time.Time
	curDay    string // "2006-01-02" of curStart; day change triggers rotation
	curSize   int64
	rotateSeq uint64 // monotonic counter disambiguating same-second archives
}

// open starts a new active file (requests-<now>.log). If a same-named file
// already exists (restart within the same second) it appends.
func (w *requestFileWriter) open(now time.Time) {
	w.curStart = now
	w.curDay = now.Format("2006-01-02")
	w.curPath = filepath.Join(w.dir, fmt.Sprintf("requests-%s.log", now.Format(fileTimeLayout)))
	f, err := os.OpenFile(w.curPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		log.Printf("[request_log] open %s: %v", w.curPath, err)
		w.f = nil
		return
	}
	w.f = f
	if fi, err := f.Stat(); err == nil {
		w.curSize = fi.Size()
	} else {
		w.curSize = 0
	}
}

// rotate closes the current file and rolls over to a fresh active file. If the
// current file has content, it is renamed to an archive
// (requests-<start>--<now>-<seq>.log); if it is empty, it is removed (no empty
// archives). The seq suffix disambiguates archives rotated within the same
// wall-clock second (a size rotation immediately followed by a day rotation, or
// a burst), so a same-second rename never overwrites a prior archive.
func (w *requestFileWriter) rotate(now time.Time) {
	if w.f != nil {
		w.f.Close()
		w.f = nil
		if w.curSize > 0 {
			w.rotateSeq++
			archived := filepath.Join(w.dir, fmt.Sprintf("requests-%s--%s-%d.log",
				w.curStart.Format(fileTimeLayout), now.Format(fileTimeLayout), w.rotateSeq))
			if err := os.Rename(w.curPath, archived); err != nil {
				log.Printf("[request_log] rename %s -> %s: %v", w.curPath, archived, err)
			}
		} else {
			// Empty file (e.g. day changed before any record landed): drop it so
			// the dir isn't littered with 0-byte files.
			if err := os.Remove(w.curPath); err != nil && !os.IsNotExist(err) {
				log.Printf("[request_log] remove empty %s: %v", w.curPath, err)
			}
		}
	}
	w.open(now)
}

// write appends one record as a JSON line, rotating first if the line would
// exceed maxSize or the calendar day has changed since the file was opened.
// A record larger than maxSize is still written (to the current file when it's
// empty, else to a fresh file) rather than dropped - better an over-size file
// than a lost record.
func (w *requestFileWriter) write(rec *requestLogRecord, now time.Time) error {
	if w.f == nil {
		w.open(now)
		if w.f == nil {
			return fmt.Errorf("request_log: no open file")
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	// Rotate when the file already has content AND the next line would exceed
	// maxSize, OR the calendar day changed. The curSize>0 guard on the size
	// check avoids archiving an empty file just because a single record is
	// larger than maxSize (the record is written to the empty file instead);
	// rotate() additionally drops empty files on day change, so neither path
	// produces a 0-byte archive.
	dayChanged := now.Format("2006-01-02") != w.curDay
	if (w.curSize > 0 && w.curSize+int64(len(line)) > w.maxSize) || dayChanged {
		w.rotate(now)
		if w.f == nil {
			return fmt.Errorf("request_log: no file after rotate")
		}
	}
	n, err := w.f.Write(line)
	w.curSize += int64(n)
	return err
}

func (w *requestFileWriter) close() {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
}

// --- logger: buffered channel + background file writer ---

// sweepInterval is how often the retention sweep runs in loop(). Bounded so an
// idle logger still reclaims old files without waiting for the next record.
const sweepInterval = time.Hour

// requestLogger owns the in-flight capture pipeline. The hot path only calls
// record() (non-blocking); a background goroutine drains the channel and writes
// JSON lines to a rotating file. A nil *requestLogger = disabled (zero
// overhead: tryTarget does not even wrap resp.Body).
type requestLogger struct {
	dir         string
	maxSize     int64
	maxBody     int
	retention   time.Duration // 0 = keep forever (no sweep)
	ch          chan *requestLogRecord
	done        chan struct{} // closed by shutdown to signal loop to drain + exit
	closed      chan struct{} // closed by loop when it has fully exited
	dropped     uint64        // atomic; records dropped because the channel was full
	writeErrors uint64        // atomic; records lost to a file-write/marshal error
	dead        uint32        // atomic; 1 once the loop exited due to a fatal setup error (mkdir)
	stopOnce    sync.Once
}

func newRequestLogger(dir string, maxSize int64, maxBody int, retention time.Duration) *requestLogger {
	return &requestLogger{
		dir:       dir,
		maxSize:   maxSize,
		maxBody:   maxBody,
		retention: retention,
		ch:        make(chan *requestLogRecord, 2048),
		done:      make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

// record enqueues one record. Non-blocking: if the channel is full the record
// is dropped and `dropped` is incremented so the hot path never blocks on disk.
func (l *requestLogger) record(r *requestLogRecord) {
	if l == nil {
		return
	}
	select {
	case l.ch <- r:
	default:
		n := atomic.AddUint64(&l.dropped, 1)
		if n == 1 || n%1000 == 0 {
			if atomic.LoadUint32(&l.dead) == 1 {
				log.Printf("[request_log] logger is dead (setup failed); dropped %d records total", n)
			} else {
				log.Printf("[request_log] channel full, dropped %d records total", n)
			}
		}
	}
}

// noteWriteError bumps the write-error counter and logs at a throttled cadence
// so a sustained disk failure is visible without flooding the log.
func (l *requestLogger) noteWriteError(err error) {
	n := atomic.AddUint64(&l.writeErrors, 1)
	if n == 1 || n%1000 == 0 {
		log.Printf("[request_log] write failed (lost %d records total): %v", n, err)
	}
}

// loop drains the channel, writing each record to the rotating file. Exits when
// done is closed: it drains anything left, then returns. Nil-safe. On a fatal
// setup error (mkdir) it logs and returns immediately; the hot path's record()
// keeps dropping into the dead channel (visible via `dropped`).
func (l *requestLogger) loop() {
	if l == nil {
		return
	}
	defer close(l.closed)
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		log.Printf("[request_log] mkdir %s: %v - logging disabled", l.dir, err)
		atomic.StoreUint32(&l.dead, 1)
		return
	}
	w := &requestFileWriter{dir: l.dir, maxSize: l.maxSize}
	defer w.close()
	w.open(time.Now())
	sweepTick := time.NewTicker(sweepInterval)
	defer sweepTick.Stop()
	// Sweep once at startup so a long-stopped daemon reclaims old files promptly.
	l.sweep(time.Now(), w.curPath)
	for {
		select {
		case r := <-l.ch:
			if r != nil {
				if err := w.write(r, time.Now()); err != nil {
					l.noteWriteError(err)
				}
			}
		case <-sweepTick.C:
			l.sweep(time.Now(), w.curPath)
		case <-l.done:
		drain:
			for {
				select {
				case r := <-l.ch:
					if r != nil {
						if err := w.write(r, time.Now()); err != nil {
							l.noteWriteError(err)
						}
					}
				default:
					break drain
				}
			}
			// Final sweep on shutdown so a clean exit also reclaims.
			l.sweep(time.Now(), w.curPath)
			return
		}
	}
}

// sweep deletes request-log files older than retention: rotated archives
// (requests-<start>--<end>-<seq>.log) AND orphaned active files
// (requests-<ts>.log, no "--") left behind by a previous daemon run that never
// rotated them. curActive (the writer's current active path) is always kept —
// passing it lets sweep treat the prior run's active file as reclaimable.
// No-op when retention <= 0. Best-effort: errors are logged but don't stop the loop.
func (l *requestLogger) sweep(now time.Time, curActive string) {
	if l.retention <= 0 {
		return
	}
	cutoff := now.Add(-l.retention)
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		log.Printf("[request_log] sweep readdir %s: %v", l.dir, err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Any request-log file (rotated archive OR active-style). The CURRENT
		// active file is exempt so an in-flight run never deletes its own file.
		if !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		full := filepath.Join(l.dir, name)
		if curActive != "" && full == curActive {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			if err := os.Remove(full); err != nil {
				log.Printf("[request_log] sweep remove %s: %v", name, err)
			}
		}
	}
}

// recordFilter narrows a request-log query. Empty/zero fields mean "no filter".
// Model/Provider match as case-insensitive substrings against the relevant
// record fields; Status matches exactly (0 = any); From/To bound the record's
// RFC3339 timestamp (zero = unbounded). Limit caps the result count (0 = no cap).
// Shadow is a tri-state: "only" keeps shadow records, "exclude" drops them,
// "" keeps everything.
type recordFilter struct {
	Model      string
	Provider   string
	Status     int
	ErrorsOnly bool
	RequestID  string
	Shadow     string
	From       time.Time
	To         time.Time
	Limit      int
}

// matches reports whether a record passes the filter.
func (f recordFilter) matches(r requestLogRecord) bool {
	if f.RequestID != "" && r.RequestID != f.RequestID {
		return false
	}
	if f.Model != "" {
		if !ciContains(r.CalledModel, f.Model) && !ciContains(r.UpstreamModel, f.Model) && !ciContains(r.Exposed, f.Model) {
			return false
		}
	}
	if f.Provider != "" && !ciContains(r.Provider, f.Provider) {
		return false
	}
	if f.Status != 0 && r.Status != f.Status {
		return false
	}
	if f.ErrorsOnly && r.Status < 400 {
		return false
	}
	switch f.Shadow {
	case "only":
		if !r.Shadow {
			return false
		}
	case "exclude":
		if r.Shadow {
			return false
		}
	}
	if !f.From.IsZero() || !f.To.IsZero() {
		ts, err := time.Parse(time.RFC3339, r.Ts)
		if err != nil {
			return false // unparseable timestamp → exclude under a time filter
		}
		if !f.From.IsZero() && ts.Before(f.From) {
			return false
		}
		if !f.To.IsZero() && ts.After(f.To) {
			return false
		}
	}
	return true
}

// ciContains reports whether s contains sub case-insensitively.
func ciContains(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// queryRequestRecords reads every requests-*.log file in dir (active + rotated),
// parses each JSONL line, and returns records matching f, newest-first, capped at
// f.Limit. Unparseable/partial lines (e.g. a line mid-write at read time) are
// skipped silently. It reads files directly, independent of the logger goroutine.
func queryRequestRecords(dir string, f recordFilter) ([]requestLogRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, "requests-") && strings.HasSuffix(n, ".log") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []requestLogRecord
	// Read newest file first so a Limit cuts early; names sort oldest→first, so
	// iterate in reverse. STREAM each file one line at a time (not os.ReadFile)
	// so a 1 GiB log file doesn't peak at 1 GiB of heap — memory is bounded to a
	// single line. We use bufio.Reader.ReadBytes rather than bufio.Scanner: a
	// Scanner caps token size (8 MiB) and HALTS at the first oversized line,
	// silently dropping it plus every later record. Valid configs (default 5 MiB
	// per body → ~10 MiB lines) exceed that cap. ReadBytes reads any line length,
	// never aborts on size, and each line is inherently bounded to ~2×max_body
	// (the writer caps both bodies) so memory stays one-line-at-a-time.
	for i := len(names) - 1; i >= 0; i-- {
		file, err := os.Open(filepath.Join(dir, names[i]))
		if err != nil {
			continue
		}
		r := bufio.NewReaderSize(file, 64*1024)
		for {
			line, readErr := r.ReadBytes('\n')
			if len(line) > 0 {
				if l := bytes.TrimRight(line, "\n"); len(l) > 0 {
					var rec requestLogRecord
					if json.Unmarshal(l, &rec) == nil && f.matches(rec) {
						out = append(out, rec)
					}
				}
			}
			if readErr != nil { // io.EOF or read error: stop this file
				break
			}
		}
		file.Close()
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	// Records within a file are chronological; across files newest-file-first is
	// already newest-first, but a single file's records are oldest-first, so sort
	// the result by timestamp desc to be certain.
	sort.Slice(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// requestLogSummary is the metadata-only projection of a requestLogRecord for the
// list API — request/response BODIES are omitted (large + sensitive); fetch a
// single record by id for bodies.
type requestLogSummary struct {
	Ts            string `json:"ts"`
	RequestID     string `json:"request_id"`
	SessionID     string `json:"session_id"`
	Protocol      string `json:"protocol"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Exposed       string `json:"exposed"`
	CalledModel   string `json:"called_model"`
	UpstreamModel string `json:"upstream_model"`
	Provider      string `json:"provider"`
	Attempt       int    `json:"attempt"`
	Status        int    `json:"status"`
	LatencyMs     int64  `json:"latency_ms"`
	RequestSize   int    `json:"request_size"`
	ResponseSize  int64  `json:"response_size"`
	Shadow        bool   `json:"shadow,omitempty"`
}

func summarizeRecord(r requestLogRecord) requestLogSummary {
	return requestLogSummary{
		Ts: r.Ts, RequestID: r.RequestID, SessionID: r.SessionID, Protocol: r.Protocol,
		Method: r.Method, Path: r.Path, Exposed: r.Exposed, CalledModel: r.CalledModel,
		UpstreamModel: r.UpstreamModel, Provider: r.Provider, Attempt: r.Attempt,
		Status: r.Status, LatencyMs: r.LatencyMs, RequestSize: r.RequestSize, ResponseSize: r.ResponseSize,
		Shadow: r.Shadow,
	}
}

// directory returns the log directory ("" when logging is disabled / nil), used
// by the /api/requests query handlers to read JSONL files.
func (l *requestLogger) directory() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// shadowReportEntry is one aggregated row of a shadow-evaluation comparison
// report: for a given (route, primary provider, shadow provider) pair, how do
// the primary and shadow compare over the sampled requests?
type shadowReportEntry struct {
	Route            string  `json:"route"`
	PrimaryProvider  string  `json:"primary_provider"`
	ShadowProvider   string  `json:"shadow_provider"`
	Samples          int     `json:"samples"`
	StatusMatchRate  float64 `json:"status_match_rate"`  // fraction where both 2xx or both non-2xx
	PrimaryLatencyMs int64   `json:"primary_latency_ms"` // average
	ShadowLatencyMs  int64   `json:"shadow_latency_ms"`  // average
	LatencyDiffMs    int64   `json:"latency_diff_ms"`    // shadow - primary
	PrimarySizeAvg   int64   `json:"primary_size_avg"`
	ShadowSizeAvg    int64   `json:"shadow_size_avg"`
}

// shadowReport scans request_log, pairs each primary record with its shadow
// counterpart (shadow-<primary-id>), and aggregates per (route, primary provider,
// shadow provider): sample count, status-match rate, avg latency difference, avg
// response-size ratio. Only paired records (both primary + shadow present) count.
func shadowReport(dir string, f recordFilter) ([]shadowReportEntry, error) {
	all, err := queryRequestRecords(dir, f)
	if err != nil {
		return nil, err
	}
	type pair struct{ primary, shadow *requestLogRecord }
	pairs := map[string]*pair{}
	for i := range all {
		r := &all[i]
		if r.Shadow {
			pid := strings.TrimPrefix(r.RequestID, "shadow-")
			p := pairs[pid]
			if p == nil {
				p = &pair{}
				pairs[pid] = p
			}
			p.shadow = r
		} else {
			p := pairs[r.RequestID]
			if p == nil {
				p = &pair{}
				pairs[r.RequestID] = p
			}
			p.primary = r
		}
	}
	type aggKey struct{ route, primary, shadow string }
	type aggVal struct {
		count, match, primLat, shadLat, primSize, shadSize int64
	}
	aggs := map[aggKey]*aggVal{}
	for _, p := range pairs {
		if p.primary == nil || p.shadow == nil {
			continue
		}
		k := aggKey{route: p.primary.Exposed, primary: p.primary.Provider, shadow: p.shadow.Provider}
		a := aggs[k]
		if a == nil {
			a = &aggVal{}
			aggs[k] = a
		}
		a.count++
		if (p.primary.Status < 300) == (p.shadow.Status < 300) {
			a.match++
		}
		a.primLat += p.primary.LatencyMs
		a.shadLat += p.shadow.LatencyMs
		a.primSize += p.primary.ResponseSize
		a.shadSize += p.shadow.ResponseSize
	}
	var out []shadowReportEntry
	for k, a := range aggs {
		n := int64(a.count)
		matchRate := 0.0
		primAvg, shadAvg, primSizeAvg, shadSizeAvg := int64(0), int64(0), int64(0), int64(0)
		if n > 0 {
			matchRate = float64(a.match) / float64(n)
			primAvg = a.primLat / n
			shadAvg = a.shadLat / n
			primSizeAvg = a.primSize / n
			shadSizeAvg = a.shadSize / n
		}
		out = append(out, shadowReportEntry{
			Route: k.route, PrimaryProvider: k.primary, ShadowProvider: k.shadow,
			Samples: int(a.count), StatusMatchRate: matchRate,
			PrimaryLatencyMs: primAvg, ShadowLatencyMs: shadAvg, LatencyDiffMs: shadAvg - primAvg,
			PrimarySizeAvg: primSizeAvg, ShadowSizeAvg: shadSizeAvg,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].Route < out[j].Route
	})
	return out, nil
}

// shutdown signals loop to drain + close, then waits for it to finish.
func (l *requestLogger) shutdown() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() { close(l.done) })
	<-l.closed
}

// --- captureReader: bounded tee that captures response bytes in-flight ---

// captureReader is a pass-through io.ReadCloser that tees bytes read from src
// into a bounded buffer (up to max bytes; past that, capturing stops but bytes
// keep flowing to the client and truncated is set). `total` always reflects the
// full response size. On the first Close it calls onClose once, then closes src.
type captureReader struct {
	src       io.ReadCloser
	buf       bytes.Buffer
	total     int64
	max       int
	truncated bool
	onClose   func(captured []byte, total int64, truncated bool)
	closeOnce sync.Once
}

func newCaptureReader(src io.ReadCloser, max int, onClose func(captured []byte, total int64, truncated bool)) *captureReader {
	return &captureReader{src: src, max: max, onClose: onClose}
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if n > 0 {
		c.total += int64(n)
		// Tee into the buffer while under cap. Once over cap, stop capturing
		// (bytes still pass through to the client) and mark truncated.
		if c.buf.Len() < c.max {
			room := c.max - c.buf.Len()
			if n <= room {
				c.buf.Write(p[:n])
			} else {
				c.buf.Write(p[:room])
				c.truncated = true
			}
		}
	}
	return n, err
}

func (c *captureReader) Close() error {
	var firstErr error
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose(c.buf.Bytes(), c.total, c.truncated)
		}
		firstErr = c.src.Close()
	})
	return firstErr
}

// newRequestID returns a 32-char hex id from crypto/rand, used to group one
// client request's failover attempts.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b[:])
}

// truncMarker is appended to a captured body that exceeded maxBodyBytes so the
// stored record is self-describing about data loss.
const truncMarker = "\n...[truncated by model-proxy request_log max_body_bytes]"

// recordInputs bundles the values needed to build a requestLogRecord, keeping
// the buildRecord signature stable as fields evolve.
type recordInputs struct {
	flc         forwardLogCtx
	r           *http.Request
	proto       string
	calledModel string
	t           RouteTarget
	resp        *http.Response
	start       time.Time
	requestBody []byte
	captured    []byte
	total       int64
	truncated   bool
}

// buildRecord assembles a requestLogRecord. The request body is capped to
// maxBody (with a truncation marker); the response body is already bounded by
// the captureReader (marker appended if truncated). ResponseSize is the true
// total (accurate even when the body was truncated).
func (l *requestLogger) buildRecord(in recordInputs) *requestLogRecord {
	rec := &requestLogRecord{
		Ts:            time.Now().UTC().Format(time.RFC3339),
		RequestID:     in.flc.requestID,
		SessionID:     in.r.Header.Get("x-claude-code-session-id"),
		Protocol:      in.proto,
		Method:        in.r.Method,
		Path:          in.r.URL.Path,
		CalledModel:   in.calledModel,
		UpstreamModel: in.t.Model,
		Exposed:       in.flc.exposed,
		Provider:      in.t.Provider,
		Attempt:       in.flc.attempt,
		Shadow:        strings.HasPrefix(in.flc.requestID, "shadow-"),
		Status:        in.resp.StatusCode,
		LatencyMs:     time.Since(in.start).Milliseconds(),
		ResponseSize:  in.total,
	}
	// Log the ORIGINAL client body (pre-rewrite, pre-convert) when available — so
	// replay re-routes naturally (alias routes resolve; conversion re-applies).
	// Shadow records have no origBody → fall back to the upstream body (sbody).
	reqBody := in.flc.origBody
	if len(reqBody) == 0 {
		reqBody = in.requestBody
	}
	rec.RequestSize = len(reqBody)
	if len(reqBody) > l.maxBody {
		rec.RequestBody = string(reqBody[:l.maxBody]) + truncMarker
	} else {
		rec.RequestBody = string(reqBody)
	}
	if in.truncated {
		rec.ResponseBody = string(in.captured) + truncMarker
	} else {
		rec.ResponseBody = string(in.captured)
	}
	hdrs := map[string]string{}
	if ct := in.resp.Header.Get("content-type"); ct != "" {
		hdrs["content-type"] = ct
	}
	if rid := in.resp.Header.Get("x-request-id"); rid != "" {
		hdrs["x-request-id"] = rid
	}
	if rr := in.resp.Header.Get("retry-after"); rr != "" {
		hdrs["retry-after"] = rr
	}
	if len(hdrs) > 0 {
		if hb, err := json.Marshal(hdrs); err == nil {
			rec.ResponseHeaders = string(hb)
		}
	}
	return rec
}

// initRequestLog constructs the file-based request logger (does NOT start the
// loop - runProxy does `go p.reqLog.loop()`). Best-effort: on failure leaves
// p.reqLog nil. Called from runProxy only - direct NewProxy callers (tests)
// stay in-memory.
func (p *Proxy) initRequestLog(rlc RequestLogConfig) {
	if !rlc.Enabled {
		return
	}
	p.reqLog = newRequestLogger(rlc.dir(), rlc.maxFileSize(), rlc.maxBodyBytes(), rlc.retention())
	log.Printf("[request_log] enabled -> %s (max_file_size %d bytes, max_body %d bytes, retention %s)",
		rlc.dir(), rlc.maxFileSize(), rlc.maxBodyBytes(), rlc.retention())
}

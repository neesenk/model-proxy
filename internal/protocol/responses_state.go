package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sonic "github.com/bytedance/sonic"
)

const (
	responsesStateTTL       = 30 * time.Minute
	responsesStateMax       = 512
	responsesStateEntryMax  = 2 << 20
	responsesStateTotalMax  = 32 << 20
	responsesStatePersistV1 = 1
)

type responsesStateEntry struct {
	Session   string `json:"session,omitempty"`
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	History   []any  `json:"history"`
	// size caches the entry's serialized byte length. sonic/json only marshal
	// the exported fields above, so the value is stable across put / persist /
	// restore; it is computed once at put time (or lazily on restore) so
	// pruneLocked and persist don't re-marshal every live entry per lookup.
	size int
}

// responsesEntrySize returns the serialized byte length of an entry. It is the
// single place that marshals an entry for accounting.
func responsesEntrySize(e responsesStateEntry) int {
	b, _ := sonic.Marshal(e)
	return len(b)
}

type responsesStateSnapshot struct {
	Version int                   `json:"version"`
	Entries []responsesStateEntry `json:"entries"`
}

// responsesStateStore expands Responses previous_response_id chains when the
// selected backend is stateless Chat/Anthropic. It is deliberately independent
// from quota/runtime health state: entries contain conversation content, have a
// short TTL, strict byte caps, and live in a 0600 file.
type responsesStateStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]responsesStateEntry
	order   []string
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	now     func() time.Time
}

func newResponsesStateStore(path string) *responsesStateStore {
	s := &responsesStateStore{
		path: path, entries: map[string]responsesStateEntry{},
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		now: time.Now,
	}
	s.load()
	if path == "" {
		close(s.done)
		return s
	}
	go s.persistLoop()
	return s
}

func responsesStatePath(quotaPath string) string {
	if quotaPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(quotaPath), "responses_state.json")
}

func responsesStateKey(session, id string) string { return session + "\x00" + id }

func responsesPreviousID(body []byte) string {
	var src map[string]any
	if sonic.Unmarshal(body, &src) != nil {
		return ""
	}
	return strOpt(src["previous_response_id"])
}

func (s *responsesStateStore) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var snap responsesStateSnapshot
	if json.Unmarshal(data, &snap) != nil || snap.Version != responsesStatePersistV1 {
		return
	}
	now := s.now()
	for _, e := range snap.Entries {
		if e.ID == "" || len(e.History) == 0 || now.Sub(time.UnixMilli(e.CreatedAt)) > responsesStateTTL {
			continue
		}
		s.putLocked(e)
	}
	s.pruneLocked(now)
}

func (s *responsesStateStore) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.path == "" {
			return
		}
		close(s.stop)
		<-s.done
	})
}

func (s *responsesStateStore) persistLoop() {
	defer close(s.done)
	var timer *time.Timer
	for {
		var timerC <-chan time.Time
		if timer != nil {
			timerC = timer.C
		}
		select {
		case <-s.wake:
			if timer == nil {
				timer = time.NewTimer(250 * time.Millisecond)
			} else if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
				timer.Reset(250 * time.Millisecond)
			}
		case <-timerC:
			s.persist()
			timer = nil
		case <-s.stop:
			if timer != nil {
				timer.Stop()
			}
			s.persist()
			return
		}
	}
}

func (s *responsesStateStore) schedulePersist() {
	if s.path == "" {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *responsesStateStore) persist() {
	s.mu.Lock()
	s.pruneLocked(s.now())
	snap := responsesStateSnapshot{Version: responsesStatePersistV1}
	total := 0
	for _, key := range s.order {
		e, ok := s.entries[key]
		if !ok {
			continue
		}
		if e.size > responsesStateEntryMax || total+e.size > responsesStateTotalMax {
			continue
		}
		total += e.size
		snap.Entries = append(snap.Entries, e)
	}
	s.mu.Unlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	_ = os.Chmod(filepath.Dir(s.path), 0o700)
	tmpFile, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp.*")
	if err != nil {
		return
	}
	tmp := tmpFile.Name()
	_ = tmpFile.Chmod(0o600)
	if _, err = tmpFile.Write(data); err == nil {
		err = tmpFile.Sync()
	}
	closeErr := tmpFile.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return
	}
	if os.Rename(tmp, s.path) != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Chmod(s.path, 0o600)
}

func (s *responsesStateStore) putLocked(e responsesStateEntry) {
	key := responsesStateKey(e.Session, e.ID)
	if _, exists := s.entries[key]; !exists {
		s.order = append(s.order, key)
	}
	if e.size == 0 {
		// Restored (or hand-built) entries carry no cached size — compute it
		// once here so prune/persist never re-marshal per lookup.
		e.size = responsesEntrySize(e)
	}
	s.entries[key] = e
}

func (s *responsesStateStore) pruneLocked(now time.Time) {
	kept := s.order[:0]
	total := 0
	for _, key := range s.order {
		e, ok := s.entries[key]
		if !ok || now.Sub(time.UnixMilli(e.CreatedAt)) > responsesStateTTL {
			delete(s.entries, key)
			continue
		}
		if e.size > responsesStateEntryMax {
			delete(s.entries, key)
			continue
		}
		total += e.size
		kept = append(kept, key)
	}
	s.order = kept
	for len(s.order) > responsesStateMax || total > responsesStateTotalMax {
		key := s.order[0]
		s.order = s.order[1:]
		if e, ok := s.entries[key]; ok {
			total -= e.size
			delete(s.entries, key)
		}
	}
}

func (s *responsesStateStore) lookup(session, id string) (responsesStateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	if e, ok := s.entries[responsesStateKey(session, id)]; ok {
		return e, true
	}
	// Clients WITHOUT a stable session header still get continuity by opaque
	// response id. Only accept an unambiguous match to avoid cross-session
	// bleed; a caller that DID present a session must never expand another
	// session's history by guessing its response id.
	if session != "" {
		return responsesStateEntry{}, false
	}
	var found responsesStateEntry
	matches := 0
	for _, e := range s.entries {
		if e.ID == id {
			found = e
			matches++
		}
	}
	return found, matches == 1
}

// expand rewrites previous_response_id + delta input into a full stateless
// history. A cache miss is repaired rather than rejected: orphan tool outputs
// become user messages so their result text survives without an invalid pair.
func (s *responsesStateStore) expand(body []byte, session string) ([]byte, []any, bool, error) {
	var src map[string]any
	if err := sonic.Unmarshal(body, &src); err != nil {
		return nil, nil, false, fmt.Errorf("parse responses request for state expansion: %w", err)
	}
	current := responsesInputAny(src["input"])
	prev := strOpt(src["previous_response_id"])
	delete(src, "previous_response_id")
	hit := false
	var merged []any
	if prev != "" {
		if e, ok := s.lookup(session, prev); ok {
			merged = append(merged, cloneAnySlice(e.History)...)
			hit = true
		}
	}
	merged = append(merged, current...)
	// Orphan repair rewrites semantics, so it only runs when the client chained
	// on a previous_response_id we could not expand (contract: explicit
	// full-history requests pass through untouched).
	if prev != "" && !hit {
		merged = repairOrphanedResponsesItems(merged)
	}
	if len(merged) > 0 {
		src["input"] = merged
	}
	out, err := sonic.Marshal(src)
	return out, merged, hit, err
}

func responsesInputAny(v any) []any {
	switch raw := v.(type) {
	case []any:
		return cloneAnySlice(raw)
	case string:
		if raw == "" {
			return nil
		}
		return []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": raw}},
		}}
	case map[string]any:
		return []any{raw}
	default:
		return nil
	}
}

func cloneAnySlice(in []any) []any {
	if len(in) == 0 {
		return nil
	}
	b, _ := sonic.Marshal(in)
	var out []any
	_ = sonic.Unmarshal(b, &out)
	return out
}

func repairOrphanedResponsesItems(input []any) []any {
	fnCalls := map[string]bool{}
	customCalls := map[string]bool{}
	fnOutputs := map[string]bool{}
	customOutputs := map[string]bool{}
	for _, raw := range input {
		item := asMap(raw)
		id := strOpt(item["call_id"])
		switch strOpt(item["type"]) {
		case "function_call", "local_shell_call":
			fnCalls[id] = id != ""
		case "custom_tool_call":
			customCalls[id] = id != ""
		case "function_call_output":
			fnOutputs[id] = id != ""
		case "custom_tool_call_output":
			customOutputs[id] = id != ""
		}
	}
	out := make([]any, 0, len(input))
	for _, raw := range input {
		item := asMap(raw)
		if item == nil {
			out = append(out, raw)
			continue
		}
		typ := strOpt(item["type"])
		// Reasoning items are bound to the lost response's context; on a miss
		// they would replay thinking the backend never saw.
		if typ == "reasoning" {
			continue
		}
		if (typ == "function_call" || typ == "local_shell_call") && !fnOutputs[strOpt(item["call_id"])] {
			convertWarn("dropping dangling responses function call without output")
			continue
		}
		if typ == "custom_tool_call" && !customOutputs[strOpt(item["call_id"])] {
			convertWarn("dropping dangling responses custom tool call without output")
			continue
		}
		if typ == "function_call_output" || typ == "custom_tool_call_output" {
			id := strOpt(item["call_id"])
			paired := fnCalls[id]
			if typ == "custom_tool_call_output" {
				paired = customCalls[id]
			}
			if !paired {
				out = append(out, map[string]any{
					"type": "message", "role": "user",
					"content": []any{map[string]any{
						"type": "input_text",
						"text": "[tool output for " + firstNonEmpty(id, "unknown call") + "]\n" + responsesToolOutputText(item["output"]),
					}},
				})
				continue
			}
		}
		out = append(out, raw)
	}
	return out
}

func responsesToolOutputText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if parts, ok := v.([]any); ok {
		var text []string
		for _, p := range parts {
			pm := asMap(p)
			if s := firstNonEmpty(strOpt(pm["text"]), strOpt(pm["refusal"])); s != "" {
				text = append(text, s)
			}
		}
		return strings.Join(text, "\n")
	}
	b, _ := sonic.MarshalString(v)
	return b
}

func (s *responsesStateStore) recordJSON(session string, requestHistory []any, body []byte) bool {
	var resp map[string]any
	if sonic.Unmarshal(body, &resp) != nil {
		return false
	}
	return s.recordResponse(session, requestHistory, resp)
}

func (s *responsesStateStore) recordSSE(session string, requestHistory []any, body []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	pending := ""
	pendData := ""    // folded data lines of the SSE frame in progress
	pendOpen := false // a data: line opened the current frame (an empty one folds to "")
	var doneItems []any
	// handleFrame processes one assembled frame; it returns true once a
	// terminal response frame has been recorded.
	handleFrame := func(event, payload string) bool {
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) != nil {
			return false
		}
		if event == "" {
			event = strOpt(data["type"])
		}
		if event == "response.output_item.done" {
			if item := asMap(data["item"]); item != nil {
				doneItems = append(doneItems, item)
			}
			return false
		}
		if event == "response.completed" || event == "response.incomplete" {
			resp := asMap(data["response"])
			if resp == nil {
				return false
			}
			if _, ok := resp["output"].([]any); !ok && len(doneItems) > 0 {
				resp["output"] = doneItems
			}
			return s.recordResponse(session, requestHistory, resp)
		}
		return false
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data:") {
			// Fold consecutive data: lines with "\n" per the SSE spec — a
			// spec-folded multi-line frame only parses once assembled.
			pendData = appendSSEData(pendData, pendOpen, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			pendOpen = true
			continue
		}
		// Classify the frame-terminating line first — it may open the NEXT
		// frame's event type; the closing frame keeps its own.
		frameEvent := pending
		if line == "" {
			pending = ""
		} else if strings.HasPrefix(line, "event:") {
			pending = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if line != "" || !pendOpen {
			// Only a blank line dispatches a frame (SSE spec); comment lines
			// belong to the frame in progress.
			continue
		}
		payload := pendData
		pendData, pendOpen = "", false
		if handleFrame(frameEvent, payload) {
			return true
		}
	}
	// A trailing frame without its final blank line still dispatches.
	if pendOpen {
		return handleFrame(pending, pendData)
	}
	return false
}

func (s *responsesStateStore) recordResponse(session string, requestHistory []any, resp map[string]any) bool {
	id := strOpt(resp["id"])
	status := strOpt(resp["status"])
	if id == "" || (status != "completed" && status != "incomplete") {
		return false
	}
	if status == "incomplete" {
		// Only token-limit partials are authoritative continuation history.
		// Content-filtered or otherwise aborted output must not be replayed as
		// if the assistant had committed it.
		switch strOpt(asMap(resp["incomplete_details"])["reason"]) {
		case "max_output_tokens", "max_tokens", "length":
		default:
			return false
		}
	}
	if asMap(resp["error"]) != nil {
		return false
	}
	history := cloneAnySlice(requestHistory)
	if output, ok := resp["output"].([]any); ok {
		history = append(history, cloneAnySlice(output)...)
	}
	if len(history) == 0 {
		return false
	}
	e := responsesStateEntry{Session: session, ID: id, CreatedAt: s.now().UnixMilli(), History: history}
	e.size = responsesEntrySize(e)
	if e.size > responsesStateEntryMax {
		return false
	}
	s.mu.Lock()
	s.putLocked(e)
	s.pruneLocked(s.now())
	s.mu.Unlock()
	s.schedulePersist()
	return true
}

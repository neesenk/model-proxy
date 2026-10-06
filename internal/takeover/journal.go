package takeover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// opJournal is the intent record of an in-flight takeover/restore operation
// (cc-switch's live-state.json pattern): written BEFORE the first mutation,
// removed only on successful completion. A leftover journal on the next run
// means the previous op was interrupted mid-flight (process exit, kill,
// power loss) — backups are idempotent-preserved and rewrites idempotent, so
// re-running the same op converges, but the interruption itself must be
// surfaced (a daemon-side Web takeover that died halfway is otherwise
// invisible).
type opJournal struct {
	Op        string   `json:"op"` // takeover | restore
	Clients   []string `json:"clients"`
	Files     []string `json:"files"`
	StartedAt string   `json:"started_at"` // RFC3339, UTC
}

const opJournalName = "op-journal.json"

func journalPath(bakDir string) string {
	return filepath.Join(bakDir, opJournalName)
}

// journalFiles lists every file an op on these clients may mutate: the main
// config, the separate MCP storage file, and side artifacts written next to
// the config (codex's model catalog).
func journalFiles(clients []ClientSpec) []string {
	var out []string
	for _, c := range clients {
		out = append(out, c.File)
		if aux := mcpAuxFile(c.Template); aux != "" {
			out = append(out, aux)
		}
		if c.Template != nil && c.Template.Models != nil && c.Template.Models.CatalogFile != "" {
			out = append(out, expandHome(c.Template.Models.CatalogFile))
		}
	}
	return out
}

// beginOp records the intent of a new takeover/restore. A pre-existing
// journal is an interrupted previous op: it is returned (non-nil) so the
// caller can warn, then superseded by the new record — the new op IS the
// convergence, no separate replay is needed. A journal that cannot be
// written degrades to a warning (the per-file atomic writes keep the op
// itself safe); a journal that cannot be READ is still treated as
// interrupted (its mere presence is the signal).
func beginOp(bakDir, op string, clients []ClientSpec) (interrupted *opJournal, err error) {
	if data, rerr := os.ReadFile(journalPath(bakDir)); rerr == nil {
		var prev opJournal
		if json.Unmarshal(data, &prev) != nil || prev.Op == "" {
			prev = opJournal{Op: "unknown"}
		}
		interrupted = &prev
	} else if !os.IsNotExist(rerr) {
		return nil, fmt.Errorf("read op journal: %w", rerr)
	}
	names := make([]string, 0, len(clients))
	for _, c := range clients {
		names = append(names, c.Name)
	}
	j := opJournal{
		Op:        op,
		Clients:   names,
		Files:     journalFiles(clients),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(j)
	if err != nil {
		return interrupted, err
	}
	if err := os.MkdirAll(bakDir, 0o700); err != nil {
		return interrupted, fmt.Errorf("create backup dir for op journal: %w", err)
	}
	if err := atomicWriteFile(journalPath(bakDir), data, 0o600); err != nil {
		return interrupted, fmt.Errorf("write op journal: %w", err)
	}
	return interrupted, nil
}

// finishOp clears the journal after a fully successful op. A removal failure
// is returned so the caller can warn — the next op would otherwise report a
// spurious interruption.
func finishOp(bakDir string) error {
	if err := os.Remove(journalPath(bakDir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear op journal: %w", err)
	}
	return nil
}

// interruptedWarning renders the interrupted-op notice shared by the CLI
// stderr warnings and the report Warnings channel.
func interruptedWarning(j *opJournal) string {
	when := j.StartedAt
	if when == "" {
		when = "unknown time"
	}
	what := strings.Join(j.Clients, ", ")
	if what == "" {
		what = "unknown clients"
	}
	return fmt.Sprintf("previous %s (started %s, clients: %s) was interrupted mid-operation; this run converges it (backups are idempotent, rewrites are idempotent)",
		j.Op, when, what)
}

package requestlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeRecordFile(t *testing.T, dir, name string, records []Record) {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for i := range records {
		if err := encoder.Encode(&records[i]); err != nil {
			t.Fatalf("encode record %d: %v", i, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, name), buffer.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func requestLogFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

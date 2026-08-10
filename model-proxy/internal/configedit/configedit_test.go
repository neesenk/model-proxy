package configedit

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadNodeAndMapNode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("a: 1\nb:\n  c: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := LoadNode(path)
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}
	if MapNode(root) == nil {
		t.Fatal("MapNode returned nil for mapping document")
	}
	if MapNode(nil) != nil {
		t.Error("MapNode(nil) must be nil")
	}
	if _, err := LoadNode(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("LoadNode missing file must error")
	}
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("a: [unclosed"), 0o600)
	if _, err := LoadNode(bad); err == nil {
		t.Error("LoadNode invalid YAML must error")
	}
}

func TestScalarHelpers(t *testing.T) {
	scalar := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

	// SetScalar replaces and appends.
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	SetScalar(doc, "k", "v")
	SetScalar(doc, "k", "v2")
	mapping := MapNode(doc)
	if len(mapping.Content) != 2 || mapping.Content[1].Value != "v2" {
		t.Errorf("SetScalar replace: content=%v", mapping.Content)
	}
	SetScalar(nil, "x", "y") // no panic

	// ChildMap finds existing, creates missing.
	sched := ChildMap(doc, "scheduling")
	if sched == nil || sched.Kind != yaml.MappingNode {
		t.Fatalf("ChildMap create: %v", sched)
	}
	if again := ChildMap(doc, "scheduling"); again != sched {
		t.Error("ChildMap must return the existing mapping")
	}
	if ChildMap(scalar("x"), "k") != nil {
		t.Error("ChildMap on scalar root must be nil")
	}

	// SetChildScalar replace/append.
	SetChildScalar(sched, "a", "1")
	SetChildScalar(sched, "a", "9")
	if sched.Content[1].Value != "9" {
		t.Errorf("SetChildScalar replace: %v", sched.Content)
	}
	SetChildScalar(nil, "a", "1")

	// LookupChildMap never creates.
	if LookupChildMap(doc, "absent") != nil {
		t.Error("LookupChildMap must not create mappings")
	}
	if LookupChildMap(doc, "scheduling") != sched {
		t.Error("LookupChildMap must find existing mapping")
	}
	if LookupChildMap(nil, "k") != nil {
		t.Error("LookupChildMap(nil) must be nil")
	}

	// DeleteKey removes, keeps others.
	SetChildScalar(sched, "b", "2")
	DeleteKey(sched, "a")
	if len(sched.Content) != 2 || sched.Content[0].Value != "b" {
		t.Errorf("DeleteKey: content=%v", sched.Content)
	}
	DeleteKey(nil, "a")

	// SetChildNode replace/append.
	parent := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{scalar("a"), scalar("1")}}
	SetChildNode(parent, "a", scalar("9"))
	SetChildNode(parent, "c", scalar("3"))
	if parent.Content[1].Value != "9" || parent.Content[len(parent.Content)-1].Value != "3" {
		t.Errorf("SetChildNode: %v", parent.Content)
	}
	SetChildNode(nil, "k", scalar("v"))
}

func TestMustEncode(t *testing.T) {
	node := MustEncode([]string{"a", "b"})
	if node.Kind != yaml.SequenceNode || len(node.Content) != 2 {
		t.Errorf("MustEncode sequence: %v", node)
	}
	if MustEncode(nil).Tag != "!!null" {
		t.Error("MustEncode(nil) must be null tag")
	}
}

func TestWriteConfigValidated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reject := func(string, []byte) error { return os.ErrInvalid }
	if _, err := WriteConfigValidated(path, "new: true", reject); err == nil {
		t.Error("WriteConfigValidated must reject when validation fails")
	}
	accept := func(string, []byte) error { return nil }
	backup, err := WriteConfigValidated(path, "new: true", accept)
	if err != nil {
		t.Fatalf("WriteConfigValidated: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new: true" {
		t.Errorf("written content = %q", data)
	}
	backupData, err := os.ReadFile(backup)
	if err != nil || string(backupData) != "old: true\n" {
		t.Errorf("backup content = %q, err=%v", backupData, err)
	}

	// AtomicWrite into a fresh nested directory.
	nested := filepath.Join(dir, "sub", "f.txt")
	if err := AtomicWrite(nested, []byte("hello")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	if b, _ := os.ReadFile(nested); string(b) != "hello" {
		t.Errorf("AtomicWrite content = %q", b)
	}

	// BackupConfig is best-effort for a missing source.
	BackupConfig(filepath.Join(dir, "missing"), filepath.Join(dir, "out.bak"))
	if _, err := os.Stat(filepath.Join(dir, "out.bak")); !os.IsNotExist(err) {
		t.Error("BackupConfig must skip missing source")
	}

	if got := BackupPath("/tmp/x/config.yaml"); filepath.Dir(got) != "/tmp/x/back" {
		t.Errorf("BackupPath = %q", got)
	}
}

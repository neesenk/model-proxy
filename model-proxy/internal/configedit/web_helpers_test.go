package configedit

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := AtomicWrite(p, []byte("hello")); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello" {
		t.Errorf("atomicWrite content=%q want hello", b)
	}
}

func TestSetChildNode(t *testing.T) {
	s := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	// nil parent is a no-op.
	SetChildNode(nil, "k", s("v"))
	// Update an existing key.
	parent := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{s("a"), s("1"), s("b"), s("2")}}
	SetChildNode(parent, "a", s("9"))
	if parent.Content[1].Value != "9" {
		t.Errorf("setChildNode update: a=%q want 9", parent.Content[1].Value)
	}
	// Append a missing key.
	SetChildNode(parent, "c", s("3"))
	if parent.Content[len(parent.Content)-1].Value != "3" {
		t.Errorf("setChildNode append: last=%q want 3", parent.Content[len(parent.Content)-1].Value)
	}
}

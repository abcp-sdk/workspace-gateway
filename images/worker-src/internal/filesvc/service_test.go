package filesvc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadWriteList(t *testing.T) {
	s := New(t.TempDir())

	if err := s.Write("a/b.txt", []byte("content")); err != nil {
		t.Fatal(err)
	}
	data, err := s.Read("a/b.txt")
	if err != nil || string(data) != "content" {
		t.Fatalf("read = %q err=%v", data, err)
	}

	// parent dirs auto-created on write
	if _, err := os.Stat(filepath.Join(s.Root(), "a")); err != nil {
		t.Fatal(err)
	}

	isDir, entries, err := s.List("a")
	if err != nil || !isDir || len(entries) != 1 || entries[0].Path != "a/b.txt" {
		t.Fatalf("list dir: %v %v err=%v", isDir, entries, err)
	}

	isDir, entries, err = s.List("a/b.txt")
	if err != nil || isDir || len(entries) != 1 {
		t.Fatalf("list file: %v %v err=%v", isDir, entries, err)
	}
}

func TestNoContainment(t *testing.T) {
	s := New(t.TempDir())

	// Absolute paths outside the workspace are allowed (the worker may touch
	// any path in its own container).
	tmp := filepath.Join(t.TempDir(), "escape.txt")
	if err := s.Write(tmp, []byte("x")); err != nil {
		t.Fatalf("absolute write must be allowed: %v", err)
	}
	if b, err := s.Read(tmp); err != nil || string(b) != "x" {
		t.Fatalf("absolute read: %v %q", err, b)
	}
	// A relative path still resolves under the workspace.
	if err := s.Write("rel.txt", []byte("y")); err != nil {
		t.Fatalf("relative write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "rel.txt")); err != nil {
		t.Fatalf("relative write did not land in workspace: %v", err)
	}
}

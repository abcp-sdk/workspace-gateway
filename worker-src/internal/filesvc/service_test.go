package filesvc

import (
	"os"
	"path/filepath"
	"strings"
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

	isDir, entries, _, err := s.List("a", 0, 0)
	if err != nil || !isDir || len(entries) != 1 || entries[0].Path != "a/b.txt" {
		t.Fatalf("list dir: %v %v err=%v", isDir, entries, err)
	}

	isDir, entries, _, err = s.List("a/b.txt", 0, 0)
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

func TestWritePreservesMode(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Write("run.sh", []byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(s.Root(), "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("run.sh", []byte("#!/bin/sh -e\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(s.Root(), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", st.Mode().Perm())
	}
}

func TestReadWindow(t *testing.T) {
	s := New(t.TempDir())
	// 5 lines, the last without a trailing newline.
	if err := s.Write("log.txt", []byte("l1\nl2\nl3\nl4\nl5")); err != nil {
		t.Fatal(err)
	}

	got, total, st, e, err := s.ReadWindow("log.txt", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || string(got) != "l2\nl3" || st != 1 || e != 3 {
		t.Fatalf("window [1,3): %q total=%d [%d,%d)", got, total, st, e)
	}

	// negative start counts from EOF
	got, total, st, e, err = s.ReadWindow("log.txt", -2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || string(got) != "l4\nl5" || st != 3 || e != 5 {
		t.Fatalf("window [-2,0): %q total=%d [%d,%d)", got, total, st, e)
	}

	// whole file when no window given
	got, total, _, _, err = s.ReadWindow("log.txt", 0, 0)
	if err != nil || total != 5 || string(got) != "l1\nl2\nl3\nl4\nl5" {
		t.Fatalf("whole: %q total=%d err=%v", got, total, err)
	}

	// window past EOF clamps empty
	_, total, st, e, err = s.ReadWindow("log.txt", 9, 20)
	if err != nil || total != 5 || st != 5 || e != 5 {
		t.Fatalf("clamp: total=%d [%d,%d) err=%v", total, st, e, err)
	}
}

func TestDeleteMoveCopy(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Write("src/a.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("src/nested/b.txt", []byte("B")); err != nil {
		t.Fatal(err)
	}

	if err := s.Copy("src", "dup"); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Read("dup/nested/b.txt"); err != nil || string(b) != "B" {
		t.Fatalf("copy tree: %q %v", b, err)
	}

	if err := s.Move("src/a.txt", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("src/a.txt"); err == nil {
		t.Fatal("source must be gone after move")
	}
	if b, err := s.Read("moved.txt"); err != nil || string(b) != "A" {
		t.Fatalf("moved: %q %v", b, err)
	}

	if err := s.Delete("src"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("src/nested/b.txt"); err == nil {
		t.Fatal("delete must remove the tree")
	}
	// the copy is untouched
	if b, err := s.Read("dup/nested/b.txt"); err != nil || string(b) != "B" {
		t.Fatalf("dup after delete: %q %v", b, err)
	}
}

func TestListDepthAndLimit(t *testing.T) {
	s := New(t.TempDir())
	for _, p := range []string{"d/a/one.txt", "d/a/two.txt", "d/b/three.txt", "d/top.txt"} {
		if err := s.Write(p, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}

	// depth=1: only immediate children
	_, entries, _, err := s.List("d", 1, 0)
	if err != nil || len(entries) != 3 {
		t.Fatalf("depth1: %v err=%v", entries, err)
	}

	// depth=3 expands recursively (BFS: level 1 dirs+file, then level 2 files)
	_, entries, _, err = s.List("d", 3, 0)
	if err != nil || len(entries) != 6 {
		t.Fatalf("depth3: %v err=%v", entries, err)
	}

	// limit truncates
	_, entries, trunc, err := s.List("d", 3, 2)
	if err != nil || len(entries) != 2 || !trunc {
		t.Fatalf("limit2: n=%d trunc=%v err=%v", len(entries), trunc, err)
	}
}

func TestListOutsideRootReturnsAbsolute(t *testing.T) {
	s := New(t.TempDir())
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	isDir, entries, _, err := s.List(outside, 1, 0)
	if err != nil || !isDir || len(entries) != 1 {
		t.Fatalf("list outside: %v %v err=%v", isDir, entries, err)
	}
	// The child path must stay ABSOLUTE (not stripped to a workspace-relative
	// name that a client would re-root under the workspace).
	if !strings.HasPrefix(entries[0].Path, "/") {
		t.Fatalf("expected absolute path, got %q", entries[0].Path)
	}
	if !strings.HasSuffix(entries[0].Path, "/f.txt") {
		t.Fatalf("unexpected path %q", entries[0].Path)
	}
}

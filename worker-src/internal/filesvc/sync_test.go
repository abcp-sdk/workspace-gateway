package filesvc

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func tarball(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

func TestSyncFolderUnpacks(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	n, err := s.SyncFolder(tarball(t, map[string]string{"a.txt": "A", "sub/b.txt": "B"}), ".", true)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 2 {
		t.Fatalf("files = %d", n)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "sub", "b.txt")); string(b) != "B" {
		t.Fatalf("b = %q", b)
	}
}

func TestSyncFolderClean(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncFolder(tarball(t, map[string]string{"new.txt": "n"}), ".", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("clean should have removed old.txt")
	}
}

func TestSyncFolderAllowsTraversal(t *testing.T) {
	// No containment: a tarball entry with `..` is unpacked as-is (the worker
	// is trusted with its own filesystem).
	root := t.TempDir()
	s := New(root)
	if _, err := s.SyncFolder(tarball(t, map[string]string{"../escape.txt": "X"}), ".", false); err != nil {
		t.Fatalf("traversal entry must be allowed now: %v", err)
	}
}

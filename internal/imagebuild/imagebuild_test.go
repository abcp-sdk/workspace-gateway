package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// tarGz builds a gzip'd tar with a single top-level dir (Forgejo style).
func tarGz(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		p := top + "/" + name
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractStripsTopDir(t *testing.T) {
	dir, cleanup, err := Extract(tarGz(t, "repo-abc123", map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"sub/main.go": "package main\n",
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(b) != "FROM scratch\n" {
		t.Fatalf("Dockerfile = %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "main.go")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
}

func TestExtractRejectsOversize(t *testing.T) {
	_, cleanup, err := Extract(tarGz(t, "r", map[string]string{"big": "0123456789"}), 5)
	if err == nil {
		cleanup()
		t.Fatal("expected oversize error")
	}
}

func TestFullRef(t *testing.T) {
	b := &Builder{RegistryHost: "git.agent.fenjin.org/"}
	if got := b.FullRef("root/myimg", "v1"); got != "git.agent.fenjin.org/root/myimg:v1" {
		t.Fatalf("FullRef = %q", got)
	}
}

func TestBuildValidatesContext(t *testing.T) {
	b := &Builder{RegistryHost: "reg", Buildctl: "true"}
	dir := t.TempDir()
	// Missing dockerfile.
	if _, err := b.Build(t.Context(), Request{ContextDir: dir, Repo: "org/x", Tag: "1"}); err == nil {
		t.Fatal("expected missing-dockerfile error")
	}
}

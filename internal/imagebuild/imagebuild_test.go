package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
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
	b := &Builder{RegistryHost: "git.agent.svc.cluster.local/"}
	if got := b.FullRef("root/myimg", "v1"); got != "git.agent.svc.cluster.local/root/myimg:v1" {
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

func TestImportValidatesInputs(t *testing.T) {
	b := &Builder{RegistryHost: "reg", Buildctl: "true"}
	cases := []ImportRequest{
		{Repo: "org/x", Tag: "1"},                            // no source
		{Source: "docker.io/library/redis:7", Tag: "7"},      // no repo
		{Source: "docker.io/library/redis:7", Repo: "org/x"}, // no tag
	}
	for _, c := range cases {
		if _, err := b.Import(t.Context(), c); err == nil {
			t.Fatalf("expected validation error for %+v", c)
		}
	}
}

func TestRegistryHostOf(t *testing.T) {
	cases := map[string]string{
		"docker.io/library/redis:7":  "docker.io",
		"library/redis":              "docker.io",
		"redis:7":                    "docker.io",
		"ghcr.io/foo/bar:v1":         "ghcr.io",
		"localhost:5000/foo/bar:tag": "localhost:5000",
		"registry.example.com/x/y:1": "registry.example.com",
	}
	for in, want := range cases {
		if got := registryHostOf(in); got != want {
			t.Fatalf("registryHostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScheme(t *testing.T) {
	if got := (&Builder{}).scheme(); got != "https" {
		t.Fatalf("default scheme = %q", got)
	}
	if got := (&Builder{RegistryScheme: "http"}).scheme(); got != "http" {
		t.Fatalf("explicit scheme = %q", got)
	}
}

func TestDockerConfigAuth(t *testing.T) {
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("root:devpassword"))
	cfg := `{"auths":{"git.agent.svc.cluster.local":{"auth":"` + auth + `"},"ghcr.io":{"auth":"` + auth + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	b := &Builder{}
	u, p, ok := b.dockerConfigAuth("git.agent.svc.cluster.local")
	if !ok || u != "root" || p != "devpassword" {
		t.Fatalf("auth = %q/%q ok=%v", u, p, ok)
	}
	// Unknown host -> no creds.
	if _, _, ok := b.dockerConfigAuth("unknown.example"); ok {
		t.Fatal("unknown host must not resolve creds")
	}
	// No DOCKER_CONFIG -> no creds.
	t.Setenv("DOCKER_CONFIG", "")
	if _, _, ok := b.dockerConfigAuth("git.agent.svc.cluster.local"); ok {
		t.Fatal("no DOCKER_CONFIG must not resolve creds")
	}
}

func TestMergeDockerConfigKeepsDeploymentAuths(t *testing.T) { // Point DOCKER_CONFIG at a temp dir holding the deployment's push creds.
	deploy := t.TempDir()
	if err := os.WriteFile(filepath.Join(deploy, "config.json"),
		[]byte(`{"auths":{"git.agent.svc.cluster.local":{"auth":"ZGVwbG95"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", deploy)
	b := &Builder{}
	dir, err := b.mergeDockerConfig("docker.io/library/redis:7", "user", "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Auths["git.agent.svc.cluster.local"].Auth != "ZGVwbG95" {
		t.Fatal("deployment push creds must be preserved")
	}
	if cfg.Auths["docker.io"].Auth == "" {
		t.Fatal("source registry creds must be added")
	}
}

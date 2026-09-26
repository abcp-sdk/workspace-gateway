// Package imagebuild builds a container image from a repository Dockerfile
// using the cluster buildkitd, and pushes it to the deployment registry.
//
// It shells out to `buildctl` (bundled in the gateway image) — the same path
// the rest of the platform uses — rather than linking the BuildKit Go client.
package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Builder builds images via buildctl against a buildkitd address.
type Builder struct {
	// Addr is the buildkitd address, e.g. tcp://buildkitd.agent.svc.cluster.local:1234.
	Addr string
	// RegistryHost is the registry host, e.g. git.agent.svc.cluster.local.
	RegistryHost string
	// RegistryScheme is the scheme for the registry host ("https" default;
	// "http" for the in-cluster plaintext registry).
	RegistryScheme string
	// Timeout bounds a build (default 30m).
	Timeout time.Duration
	// MaxContextBytes caps the extracted context size (default 512 MiB).
	MaxContextBytes int64
	// Buildctl is the buildctl binary path (default "buildctl").
	Buildctl string
	// RegistryUser/RegistryPass authenticate registry requests.
	RegistryUser string
	RegistryPass string
}

// Request is one build.
type Request struct {
	// ContextDir is the extracted repository root (tar.gz already unpacked).
	ContextDir string
	// Dockerfile is the repo-relative Dockerfile path (default "Dockerfile").
	Dockerfile string
	// Context is the repo-relative build-context subdirectory ("" = root).
	Context string
	// Repo is the destination repo path under the registry host, e.g.
	// "<org>/<image>" (may contain '/').
	Repo string
	// Tag is the image tag.
	Tag string
	// BuildArgs are passed as --opt build-arg:k=v.
	BuildArgs map[string]string
}

// Result is a completed build.
type Result struct {
	ImageRef string
	Log      string
}

// FullRef is the destination reference for a repo path + tag.
func (b *Builder) FullRef(repo, tag string) string {
	return strings.TrimRight(b.RegistryHost, "/") + "/" + strings.Trim(repo, "/") + ":" + tag
}

// scheme is the registry scheme (https unless explicitly set to http).
func (b *Builder) scheme() string {
	if b.RegistryScheme != "" {
		return b.RegistryScheme
	}
	return "https"
}

// Build runs buildctl and pushes the image. It returns the destination ref and
// the captured build log.
func (b *Builder) Build(ctx context.Context, req Request) (Result, error) {
	if req.Repo == "" || req.Tag == "" {
		return Result{}, errors.New("repo and tag required")
	}
	ctxDir := filepath.Join(req.ContextDir, filepath.FromSlash(strings.Trim(req.Context, "/")))
	if fi, err := os.Stat(ctxDir); err != nil || !fi.IsDir() {
		return Result{}, fmt.Errorf("build context %q not found in repository", req.Context)
	}
	dockerfile := req.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	// buildctl's dockerfile frontend wants the dockerfile dir + filename
	// separately; both are relative to --local dockerfile.
	dockerfileDir := filepath.Dir(filepath.FromSlash(strings.Trim(dockerfile, "/")))
	filename := filepath.Base(filepath.FromSlash(dockerfile))
	if _, err := os.Stat(filepath.Join(req.ContextDir, filepath.FromSlash(dockerfile))); err != nil {
		return Result{}, fmt.Errorf("dockerfile %q not found in repository", dockerfile)
	}

	ref := b.FullRef(req.Repo, req.Tag)
	args := []string{
		"--frontend", "dockerfile.v0",
		"--local", "context=" + ctxDir,
		"--local", "dockerfile=" + filepath.Join(req.ContextDir, dockerfileDir),
		"--opt", "filename=" + filename,
	}
	for k, v := range req.BuildArgs {
		args = append(args, "--opt", "build-arg:"+k+"="+v)
	}
	args = append(args, "--output", "type=image,name="+ref+",push=true")
	log, err := b.run(ctx, args)
	if err != nil {
		return Result{}, err
	}
	return Result{ImageRef: ref, Log: log}, nil
}

// ImportRequest mirrors one upstream image into the deployment registry.
type ImportRequest struct {
	// Source is the upstream image ref (e.g. docker.io/library/redis:7.4.2).
	Source string
	// Repo is the destination repo path under the registry host, e.g.
	// "<org>/<image>".
	Repo string
	// Tag is the destination tag.
	Tag string
	// AuthUser/AuthToken authenticate a PRIVATE source registry (optional).
	AuthUser  string
	AuthToken string
}

// Import re-serves an upstream image under the deployment registry with a no-op
// `FROM <Source>` build, preserving the upstream config (entrypoint, env, ...).
// The source may be private: its credentials are merged into a temporary Docker
// config alongside the deployment's own, so the push still authenticates.
func (b *Builder) Import(ctx context.Context, req ImportRequest) (Result, error) {
	if req.Source == "" || req.Repo == "" || req.Tag == "" {
		return Result{}, errors.New("source, repo and tag required")
	}
	ref := b.FullRef(req.Repo, req.Tag)
	dir, err := os.MkdirTemp("", "import-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	cf := "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(cf), 0o644); err != nil {
		return Result{}, err
	}

	args := []string{
		"--frontend", "dockerfile.v0",
		"--local", "context=" + dir,
		"--local", "dockerfile=" + dir,
		"--opt", "filename=Dockerfile",
		"--opt", "build-arg:BASE_IMAGE=" + req.Source,
		"--output", "type=image,name=" + ref + ",push=true",
	}

	// A private source needs credentials the buildkit client can present. Merge
	// the source registry's Basic auth into a copy of the deployment's Docker
	// config (which carries the destination push creds), and run THIS build with
	// DOCKER_CONFIG pointed at the copy.
	env := []string(nil)
	cleanup := func() {}
	if req.AuthUser != "" || req.AuthToken != "" {
		merged, err := b.mergeDockerConfig(req.Source, req.AuthUser, req.AuthToken)
		if err != nil {
			return Result{}, err
		}
		cleanup = func() { _ = os.RemoveAll(merged) }
		env = append(env, "DOCKER_CONFIG="+merged)
	}
	defer cleanup()
	log, err := b.runEnv(ctx, env, args)
	if err != nil {
		return Result{}, err
	}
	return Result{ImageRef: ref, Log: log}, nil
}

// dockerConfigAuth reads Basic-auth for a registry host from the client's
// Docker config (DOCKER_CONFIG/config.json), which the gateway mounts. It lets
// ImageExists authenticate against the in-cluster registry without a separate
// credential env.
func (b *Builder) dockerConfigAuth(host string) (user, pass string, ok bool) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		return "", "", false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return "", "", false
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", "", false
	}
	// The key may be the bare host or include a scheme/path; match either.
	for _, key := range []string{host, "https://" + host, "http://" + host, "https://" + host + "/v1/", "https://" + host + "/v2/", "http://" + host + "/v2/"} {
		entry, found := cfg.Auths[key]
		if !found || entry.Auth == "" {
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return "", "", false
		}
		u, p, found := strings.Cut(string(dec), ":")
		if !found {
			return "", "", false
		}
		return u, p, true
	}
	return "", "", false
}

// registryHostOf derives the registry host from an image ref (docker.io when no
// explicit host is present). The first path segment is a registry host only
// when it contains a '.' or ':' AND a path follows; `redis:7` is a tag, not a
// host.
func registryHostOf(ref string) string {
	slash := strings.Index(ref, "/")
	if slash < 0 {
		return "docker.io"
	}
	first := ref[:slash]
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return "docker.io"
}

// mergeDockerConfig writes a temp DOCKER_CONFIG whose config.json is the
// deployment's own plus a Basic-auth entry for the source registry. It returns
// the temp dir (to be removed by the caller).
func (b *Builder) mergeDockerConfig(srcRef, user, token string) (string, error) {
	cfg := map[string]any{"auths": map[string]any{}}
	if p := os.Getenv("DOCKER_CONFIG"); p != "" {
		if raw, err := os.ReadFile(filepath.Join(p, "config.json")); err == nil {
			_ = json.Unmarshal(raw, &cfg)
		}
	}
	auths, _ := cfg["auths"].(map[string]any)
	if auths == nil {
		auths = map[string]any{}
		cfg["auths"] = auths
	}
	host := registryHostOf(srcRef)
	enc := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	for _, key := range []string{host, "https://" + host, "https://" + host + "/v1/", "https://" + host + "/v2/"} {
		auths[key] = map[string]any{"auth": enc}
	}
	dir, err := os.MkdirTemp("", "dockerconfig-")
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), out, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// run executes buildctl with the given args (after --addr build) and returns
// the captured log tail.
func (b *Builder) run(ctx context.Context, args []string) (string, error) {
	return b.runEnv(ctx, nil, args)
}

// runEnv is run with extra environment variables (e.g. a per-build
// DOCKER_CONFIG for private-source credentials).
func (b *Builder) runEnv(ctx context.Context, env []string, args []string) (string, error) {
	buildctl := b.Buildctl
	if buildctl == "" {
		buildctl = "buildctl"
	}
	addr := b.Addr
	if addr == "" {
		addr = "tcp://buildkitd.agent.svc.cluster.local:1234"
	}
	full := append([]string{"--addr", addr, "build"}, args...)

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var out bytes.Buffer
	cmd := exec.CommandContext(bctx, buildctl, full...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return tail(out.String(), 16*1024), fmt.Errorf("buildctl failed: %w\n%s", err, tail(out.String(), 16*1024))
	}
	return tail(out.String(), 16*1024), nil
}

// isTarMeta reports whether a header is tar bookkeeping that must never be
// treated as a real entry. Go's archive/tar SURFACES a leading
// `TypeXGlobalHeader` (Forgejo archives begin with a `pax_global_header`), and
// a `TypeXHeader` may appear too; both would otherwise be mistaken for the
// archive's top-level directory name.
func isTarMeta(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader
}

// archiveTop decides which single leading path segment to strip. Forgejo wraps
// every archive in one `<repo>/` directory, but a tar stream may begin with PAX
// metadata and an archive may (in principle) have no wrapper at all. Strip the
// leading segment ONLY when every real entry shares the same first segment AND
// that entry is actually nested (its name contains a separator) — otherwise
// nothing is stripped, so a root-level file is never dropped.
func archiveTop(tarball []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seg := ""
	nested := false
	uniform := true
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}
		if isTarMeta(hdr) {
			continue
		}
		name := filepath.ToSlash(hdr.Name)
		if strings.Contains(name, "/") {
			nested = true
		}
		first := strings.SplitN(strings.Trim(name, "/"), "/", 2)[0]
		if first == "" {
			continue
		}
		if seg == "" {
			seg = first
		} else if first != seg {
			uniform = false
		}
	}
	if nested && uniform && seg != "" {
		return seg, nil
	}
	return "", nil
}

// Extract unpacks a tar.gz repository archive into a fresh temp directory,
// stripping the archive's single top-level directory (Forgejo wraps the tree).
// The returned cleanup removes the directory.
func Extract(tarball []byte, maxBytes int64) (string, func(), error) {
	if maxBytes <= 0 {
		maxBytes = 512 << 20
	}
	top, err := archiveTop(tarball)
	if err != nil {
		return "", nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return "", nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	dir, err := os.MkdirTemp("", "sandbox-build-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("tar: %w", err)
		}
		if isTarMeta(hdr) {
			continue
		}
		name := filepath.ToSlash(hdr.Name)
		rel := name
		if top != "" {
			rel = strings.TrimPrefix(name, top)
		}
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		// Reject path escapes.
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, dir+string(os.PathSeparator)) && target != dir {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				cleanup()
				return "", nil, err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxBytes {
				cleanup()
				return "", nil, fmt.Errorf("build context exceeds %d bytes", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				cleanup()
				return "", nil, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				cleanup()
				return "", nil, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				cleanup()
				return "", nil, err
			}
			_ = f.Close()
		}
	}
	return dir, cleanup, nil
}

// tail returns the last n bytes of s (the useful end of a build log).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…\n" + s[len(s)-n:]
}

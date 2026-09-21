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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	// RegistryHost is the registry host, e.g. git.agent.fenjin.org.
	RegistryHost string
	// DeriveRepo is the repo path for derived sandbox images (default
	// "root/sandbox"), pushed as <host>/<DeriveRepo>:<hash>.
	DeriveRepo string
	// Timeout bounds a build (default 30m).
	Timeout time.Duration
	// MaxContextBytes caps the extracted context size (default 512 MiB).
	MaxContextBytes int64
	// Buildctl is the buildctl binary path (default "buildctl").
	Buildctl string
	// WorkerBin is the path to the easyworker linux/amd64 binary to inject
	// into derived sandbox images (default "/usr/local/lib/easyworker/easyworker").
	WorkerBin string
	// RegistryUser/RegistryPass authenticate the ImageExists check.
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

// deriveVersion identifies the derived-sandbox Containerfile layout. Bump it
// whenever the generated Containerfile changes so cached images are not reused.
const deriveVersion = "v2" // v2: use the worker's default ~/workspace

// FullRef is the destination reference for a repo path + tag.
func (b *Builder) FullRef(repo, tag string) string {
	return strings.TrimRight(b.RegistryHost, "/") + "/" + strings.Trim(repo, "/") + ":" + tag
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

// Derive builds a sandbox image by injecting the easyworker binary into an
// arbitrary base image (easylab's model): `FROM <base>` + COPY worker + set
// the worker env/entrypoint. The derived tag is content-addressed over
// (base image, worker binary), so identical requests reuse one image.
//
// It returns the derived ref and whether it was freshly built.
func (b *Builder) Derive(ctx context.Context, baseImage string) (ref string, built bool, err error) {
	if baseImage == "" {
		return "", false, errors.New("base image required")
	}
	bin, err := os.ReadFile(b.workerBin())
	if err != nil {
		return "", false, fmt.Errorf("worker binary: %w", err)
	}
	// deriveVersion is part of the hash: bump it whenever the Containerfile
	// below changes, so a stale image with the same base is never reused.
	sum := sha256.Sum256(append([]byte(baseImage+"|"+deriveVersion+"|"), bin...))
	short := hex.EncodeToString(sum[:])[:16]
	repo := b.DeriveRepo
	if repo == "" {
		repo = "root/sandbox"
	}
	ref = b.FullRef(repo, short)

	if b.ImageExists(ctx, ref) {
		return ref, false, nil
	}

	dir, err := os.MkdirTemp("", "derive-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "easyworker"), bin, 0o755); err != nil {
		return "", false, err
	}
	// No WORKER_WORKSPACE/WORKDIR: the worker uses its own default (~/workspace)
	// and creates it at startup.
	cf := fmt.Sprintf("FROM %s\nCOPY easyworker /usr/local/bin/easyworker\nRUN mkdir -p /data\nENV WORKER_PORT=48080 \\\n    WORKER_DB=/data/jobs.db\nEXPOSE 48080\nENTRYPOINT [\"/usr/local/bin/easyworker\"]\n", baseImage)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(cf), 0o644); err != nil {
		return "", false, err
	}

	args := []string{
		"--frontend", "dockerfile.v0",
		"--local", "context=" + dir,
		"--local", "dockerfile=" + dir,
		"--opt", "filename=Dockerfile",
		"--output", "type=image,name=" + ref + ",push=true",
	}
	if _, err := b.run(ctx, args); err != nil {
		return "", false, err
	}
	return ref, true, nil
}

// run executes buildctl with the given args (after --addr build) and returns
// the captured log tail.
func (b *Builder) run(ctx context.Context, args []string) (string, error) {
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
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return tail(out.String(), 16*1024), fmt.Errorf("buildctl failed: %w\n%s", err, tail(out.String(), 16*1024))
	}
	return tail(out.String(), 16*1024), nil
}

func (b *Builder) workerBin() string {
	if b.WorkerBin != "" {
		return b.WorkerBin
	}
	return "/usr/local/lib/easyworker/easyworker"
}

// ImageExists reports whether a manifest for ref already exists in the registry
// (a HEAD /v2/<repo>/manifests/<tag>). Auth uses RegistryUser/Pass when set.
func (b *Builder) ImageExists(ctx context.Context, ref string) bool {
	repo, tag, ok := splitRef(ref)
	if !ok {
		return false
	}
	host := strings.TrimRight(b.RegistryHost, "/")
	if host == "" {
		host = strings.SplitN(ref, "/", 2)[0]
	}
	u := "https://" + host + "/v2/" + repo + "/manifests/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false
	}
	if b.RegistryUser != "" {
		req.SetBasicAuth(b.RegistryUser, b.RegistryPass)
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// splitRef splits "host/ns/name:tag" into repo ("ns/name") + tag.
func splitRef(ref string) (repo, tag string, ok bool) {
	i := strings.Index(ref, "/")
	if i < 0 {
		return "", "", false
	}
	rest := ref[i+1:]
	j := strings.LastIndex(rest, ":")
	if j < 0 {
		return rest, "latest", true
	}
	return rest[:j], rest[j+1:], true
}

// Extract unpacks a tar.gz repository archive into a fresh temp directory,
// stripping the archive's single top-level directory (Forgejo wraps the tree).
// The returned cleanup removes the directory.
func Extract(tarball []byte, maxBytes int64) (string, func(), error) {
	if maxBytes <= 0 {
		maxBytes = 512 << 20
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
	top := "" // first path component, stripped from every entry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("tar: %w", err)
		}
		name := filepath.ToSlash(hdr.Name)
		if top == "" {
			top = strings.SplitN(strings.Trim(name, "/"), "/", 2)[0]
		}
		rel := strings.TrimPrefix(name, top)
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

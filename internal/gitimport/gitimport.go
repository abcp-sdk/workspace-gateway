// Package gitimport clones an EXTERNAL git repository (head branches only) and
// pushes it into a local Forgejo repository, then reports the branch to check
// out. It exists because Forgejo's own `/repos/migrate` uses mirror semantics
// (`+refs/*:refs/*`): on a popular upstream (e.g. octocat/Spoon-Knife, with
// 63k `refs/pull/*`) that means a multi-GB, multi-minute clone. Cloning just
// the heads is what a normal `git clone` does and completes in seconds.
//
// go-git is used so the gateway image needs no `git` binary and no subprocess.
package gitimport

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Result reports what was imported.
type Result struct {
	// DefaultBranch is ALWAYS `main`: the imported content lands there whatever
	// the source ref was named.
	DefaultBranch string
	// Branches are the branch names pushed to the destination (always [`main`]).
	Branches []string
	// SourceRef is the source ref actually imported (the explicit `Ref`, or the
	// source's HEAD branch when none was given).
	SourceRef string
	// Bytes is the on-disk size of the bare clone (for diagnostics).
	Bytes int64
}

// Options configures an import.
type Options struct {
	// SourceURL is the external repository (https or http; ssh is not supported
	// without a key).
	SourceURL string
	// DestURL is the Forgejo repository to push into, e.g.
	// `http://git.agent.svc.cluster.local/org/repo.git`. It MUST already exist
	// and be empty (a bare repo with no refs).
	DestURL string
	// DestUser + DestToken authenticate the push.
	DestUser  string
	DestToken string
	// SourceUser + SourceToken authenticate a PRIVATE source. Empty = anonymous.
	SourceUser  string
	SourceToken string
	// Ref selects the SOURCE ref to import: a branch name, a tag name, or any
	// revision git can resolve (e.g. a commit sha). Empty = the source's HEAD
	// branch. Whatever it resolves to lands on the destination's `main`.
	Ref string
	// Timeout bounds the whole operation. Zero = 15 minutes.
	Timeout time.Duration
}

// Import clones the source's heads and pushes them to DestURL. It returns the
// branch the caller should treat as the default.
func Import(ctx context.Context, opt Options) (Result, error) {
	if opt.SourceURL == "" {
		return Result{}, fmt.Errorf("source url is required")
	}
	if opt.DestURL == "" {
		return Result{}, fmt.Errorf("destination url is required")
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "gitimport-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.PlainInit(dir, true)
	if err != nil {
		return Result{}, fmt.Errorf("init bare clone: %w", err)
	}

	// Resolve the source's default branch + authenticate the fetch.
	srcAuth := sourceAuth(opt.SourceUser, opt.SourceToken)
	src, err := repo.CreateRemote(&config.RemoteConfig{Name: "src", URLs: []string{opt.SourceURL}})
	if err != nil {
		return Result{}, fmt.Errorf("add source remote: %w", err)
	}
	defaultBranch, err := remoteHead(ctx, src, srcAuth)
	if err != nil {
		return Result{}, fmt.Errorf("read source HEAD: %w", err)
	}

	// Fetch heads + tags (never pull refs). This is the crux: a mirror refspec
	// would drag in every `refs/pull/*`. Tags are fetched so a tag/rev `Ref`
	// can be resolved and imported.
	if err := src.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*",
		},
		Tags: git.NoTags,
		Auth: srcAuth,
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		return Result{}, fmt.Errorf("fetch %s: %w", opt.SourceURL, err)
	}

	// Resolve the SOURCE ref to import: the explicit `Ref` (branch/tag/rev) or
	// the source's HEAD branch. It is pushed to the destination as `main`.
	sourceRef, commit, err := resolveSourceRef(repo, defaultBranch, opt.Ref)
	if err != nil {
		return Result{}, err
	}

	// Push the resolved commit to the destination as `main`. Using the commit
	// hash (not a ref name) lets a tag or arbitrary revision become `main`.
	dst, err := repo.CreateRemote(&config.RemoteConfig{Name: "dst", URLs: []string{opt.DestURL}})
	if err != nil {
		return Result{}, fmt.Errorf("add dest remote: %w", err)
	}
	pushSpec := config.RefSpec(fmt.Sprintf("+%s:refs/heads/%s", commit.String(), mainBranch))
	if err := dst.PushContext(ctx, &git.PushOptions{
		RemoteName: "dst",
		RefSpecs:   []config.RefSpec{pushSpec},
		Auth:       &http.BasicAuth{Username: opt.DestUser, Password: opt.DestToken},
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		return Result{}, fmt.Errorf("push to destination: %w", err)
	}

	var size int64
	if fi, serr := os.Stat(dir); serr == nil {
		size = dirSize(fi)
	}
	return Result{DefaultBranch: mainBranch, Branches: []string{mainBranch}, SourceRef: sourceRef, Bytes: size}, nil
}

// mainBranch is the only destination branch an import creates.
const mainBranch = "main"

// resolveSourceRef turns the requested source ref into a commit to import.
// An explicit ref may be a branch, a tag, or any revision (e.g. a sha); an
// empty ref means the source's HEAD branch. It returns the ref's display name
// and the resolved commit.
func resolveSourceRef(repo *git.Repository, headBranch, ref string) (string, plumbing.Hash, error) {
	if ref == "" {
		ref = headBranch
	}
	if ref == "" {
		return "", plumbing.ZeroHash, fmt.Errorf("source repository has no branches")
	}
	// A branch first, then a tag, then any revision git can peel.
	if h, err := repo.ResolveRevision(plumbing.Revision("refs/heads/" + ref)); err == nil {
		return ref, *h, nil
	}
	if h, err := repo.ResolveRevision(plumbing.Revision("refs/tags/" + ref)); err == nil {
		return ref, *h, nil
	}
	if h, err := repo.ResolveRevision(plumbing.Revision(ref)); err == nil {
		return ref, *h, nil
	}
	return "", plumbing.ZeroHash, fmt.Errorf("ref %q not found in the source repository", ref)
}

// remoteHead reads the source's HEAD symref target (e.g. `refs/heads/main`).
func remoteHead(ctx context.Context, rem *git.Remote, auth transport.AuthMethod) (string, error) {
	list, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return "", err
	}
	for _, ref := range list {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			return strings.TrimPrefix(ref.Target().String(), "refs/heads/"), nil
		}
	}
	return "", nil
}

// sourceAuth builds the fetch auth method, or nil for an anonymous source.
func sourceAuth(user, token string) transport.AuthMethod {
	if token == "" {
		return nil
	}
	if user == "" {
		user = "git"
	}
	return &http.BasicAuth{Username: user, Password: token}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func dirSize(fi os.FileInfo) int64 {
	if !fi.IsDir() {
		return fi.Size()
	}
	return fi.Size()
}

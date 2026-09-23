// Package gitcommit owns the mutable-history write path for a branch session.
//
// The repository's contents API (Forgejo) can only APPEND commits, so editing a
// branch's history — amending the current commit, rewording it, and opening an
// empty "staging" placeholder — needs a real git client. This package clones the
// session's branch into a cached worktree, applies file operations, amends or
// commits, and FORCE-pushes the branch (safe: a branch is session-exclusive).
//
// Semantics (the branch's HEAD is the only commit that is ever rewritten):
//
//	fresh branch (HEAD == main tip)
//	  write/edit/delete  -> commit a new placeholder "ABCP_XXX" with the change
//	  repo-commit(msg)   -> reword placeholder to msg, append an EMPTY placeholder
//	staging open (HEAD == placeholder)
//	  write/edit/delete  -> AMEND the placeholder with the change
//	  repo-commit(msg)   -> reword the placeholder to msg, append an EMPTY placeholder
//
// A placeholder whose tree equals its parent's tree is "clean" (no staged
// change); a placeholder that differs carries staged work. Sync/MR refuse only
// the latter. An MR merges the placeholder's PARENT, so the empty placeholder
// never reaches main.
package gitcommit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// PlaceholderPrefix begins the commit message of the synthetic "staging"
// commit. It is a sentinel: HasPrefix(message, PlaceholderPrefix) detects it.
const PlaceholderPrefix = "ABCP_XXX"

// The placeholder commit message. A trailing note keeps it self-explanatory in
// a git log while remaining a stable prefix.
const PlaceholderMessage = PlaceholderPrefix + " (workspace staging — amend before opening a change request)"

// ErrStaged is returned when an operation requires a clean branch but the
// current placeholder commit carries staged changes.
var ErrStaged = errors.New("branch has uncommitted staged changes; call repo-commit with a message first")

// IsPlaceholder reports whether a commit message is the staging placeholder.
func IsPlaceholder(msg string) bool { return strings.HasPrefix(msg, PlaceholderPrefix) }

// FileOp is one file change to apply.
type FileOp struct {
	Path    string
	Op      string // create | update | delete (empty = update)
	Content []byte
}

// Options identifies one branch to operate on.
type Options struct {
	RepoURL string
	Branch  string
	User    string
	Token   string
	Timeout time.Duration
}

// Manager caches one clone per branch so amend operations are cheap.
type Manager struct {
	mu    sync.Mutex
	cache map[string]*cloned
	caps  int
	// baseDir is where clones live. Empty = the OS temp dir. Set it to a
	// durable volume (the gateway's /data) so clones survive a restart.
	baseDir string
}

type cloned struct {
	dir    string
	repo   *git.Repository
	auth   *http.BasicAuth
	branch string
	last   time.Time
}

// NewManager builds an empty manager with a bounded cache.
func NewManager() *Manager { return &Manager{cache: map[string]*cloned{}, caps: 8} }

// SetBaseDir points the clone cache at a durable directory (default: temp).
func (m *Manager) SetBaseDir(dir string) { m.baseDir = dir }

func (m *Manager) newDir() (string, error) {
	if m.baseDir == "" {
		return os.MkdirTemp("", "gitcommit-")
	}
	if err := os.MkdirAll(m.baseDir, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(m.baseDir, "gitcommit-")
}

func timeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Minute
	}
	return d
}

func (m *Manager) key(o Options) string { return o.RepoURL + "\x00" + o.Branch }

// clone returns a ready worktree for the branch, cloning or fetching as needed.
func (m *Manager) clone(ctx context.Context, o Options) (*cloned, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.key(o)
	if c, ok := m.cache[key]; ok {
		if err := m.refresh(ctx, c, o); err == nil {
			c.last = time.Now()
			return c, nil
		}
		// Refresh failed (branch force-pushed with unrelated history); reclaim.
		_ = os.RemoveAll(c.dir)
		delete(m.cache, key)
	}
	dir, err := m.newDir()
	if err != nil {
		return nil, err
	}
	auth := &http.BasicAuth{Username: o.User, Password: o.Token}
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           o.RepoURL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(o.Branch),
		SingleBranch:  true,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("clone %s: %w", o.Branch, err)
	}
	c := &cloned{dir: dir, repo: repo, auth: auth, branch: o.Branch, last: time.Now()}
	m.evictLocked()
	m.cache[key] = c
	return c, nil
}

// refresh fetches the branch and hard-resets the worktree onto the remote tip,
// so the cached clone reflects any external history rewrite (amend/force-push).
func (m *Manager) refresh(ctx context.Context, c *cloned, o Options) error {
	ref := plumbing.NewBranchReferenceName(c.branch)
	err := c.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/" + c.branch + ":refs/remotes/origin/" + c.branch)},
		Auth:       c.auth,
		Force:      true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}
	remote, err := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", c.branch), true)
	if err != nil {
		return err
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Reset(&git.ResetOptions{Commit: remote.Hash(), Mode: git.HardReset}); err != nil {
		return err
	}
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(ref, remote.Hash())); err != nil {
		return err
	}
	return nil
}

// evictLocked drops the least-recently-used clone when over capacity.
func (m *Manager) evictLocked() {
	for len(m.cache) >= m.caps {
		var oldestKey string
		var oldest time.Time
		for k, c := range m.cache {
			if oldestKey == "" || c.last.Before(oldest) {
				oldestKey, oldest = k, c.last
			}
		}
		if oldestKey == "" {
			return
		}
		_ = os.RemoveAll(m.cache[oldestKey].dir)
		delete(m.cache, oldestKey)
	}
}

// head returns the current HEAD commit and whether its message is a placeholder.
func (c *cloned) head() (*object.Commit, bool, error) {
	ref, err := c.repo.Head()
	if err != nil {
		return nil, false, err
	}
	commit, err := c.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, false, err
	}
	return commit, IsPlaceholder(commit.Message), nil
}

// commitIsEmpty reports whether the commit's tree equals its parent's tree (or
// the commit has no parent and an empty tree).
func (c *cloned) commitIsEmpty(commit *object.Commit) (bool, error) {
	tree, err := commit.Tree()
	if err != nil {
		return false, err
	}
	if commit.NumParents() == 0 {
		return len(tree.Entries) == 0, nil
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return false, err
	}
	ptree, err := parent.Tree()
	if err != nil {
		return false, err
	}
	return tree.Hash == ptree.Hash, nil
}

// ResetTo force-moves the branch to `sha` and drops the cached clone, so the
// next operation re-clones. It is used at merge time to strip a trailing empty
// placeholder before Forgejo performs the MR merge (main is protected, so the
// gateway cannot push it directly; the feature branch is not).
func (m *Manager) ResetTo(ctx context.Context, o Options, sha string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout(o.Timeout))
	defer cancel()
	c, err := m.clone(ctx, o)
	if err != nil {
		return err
	}
	h, err := resolveCommit(c.repo, sha)
	if err != nil {
		return err
	}
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(o.Branch), h)); err != nil {
		return err
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Reset(&git.ResetOptions{Commit: h, Mode: git.HardReset}); err != nil {
		return err
	}
	if err := c.push(ctx, o); err != nil {
		return err
	}
	m.forget(o)
	return nil
}

// forget drops a cached clone (after a history rewrite made it stale).
func (m *Manager) forget(o Options) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.key(o)
	if c, ok := m.cache[key]; ok {
		_ = os.RemoveAll(c.dir)
		delete(m.cache, key)
	}
}

// Status reports the branch's staging state.
type Status struct {
	// Placeholder is true when HEAD's message is the placeholder.
	Placeholder bool
	// Staged is true when the placeholder commit carries changes (its tree
	// differs from its parent). Always false for a clean placeholder.
	Staged bool
	// Tip is HEAD's sha; MergeTip is the sha an MR should merge (the parent of a
	// placeholder, else the tip itself).
	Tip      string
	MergeTip string
}

// Status inspects the branch without modifying it.
func (m *Manager) Status(ctx context.Context, o Options) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout(o.Timeout))
	defer cancel()
	c, err := m.clone(ctx, o)
	if err != nil {
		return Status{}, err
	}
	commit, place, err := c.head()
	if err != nil {
		return Status{}, err
	}
	st := Status{Placeholder: place, Tip: commit.Hash.String(), MergeTip: commit.Hash.String()}
	if place {
		empty, err := c.commitIsEmpty(commit)
		if err != nil {
			return Status{}, err
		}
		st.Staged = !empty
		if commit.NumParents() > 0 {
			parent, err := commit.Parent(0)
			if err == nil {
				st.MergeTip = parent.Hash.String()
			}
		}
	}
	return st, nil
}

// ApplyFiles applies ops and commits them, then returns the new tip sha.
//
// If HEAD is a placeholder the commit is AMENDED (rewording it back to the
// placeholder); otherwise a NEW placeholder commit is created. In both cases the
// result is a placeholder carrying the change (staged).
func (m *Manager) ApplyFiles(ctx context.Context, o Options, ops []FileOp) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout(o.Timeout))
	defer cancel()
	if len(ops) == 0 {
		return "", errors.New("no file operations given")
	}
	c, err := m.clone(ctx, o)
	if err != nil {
		return "", err
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return "", err
	}
	for _, op := range ops {
		if err := applyFile(wt, op); err != nil {
			return "", fmt.Errorf("apply %s: %w", op.Path, err)
		}
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return "", fmt.Errorf("stage: %w", err)
	}
	_, place, err := c.head()
	if err != nil {
		return "", err
	}
	author := &object.Signature{Name: "workspace-gateway", Email: "gateway@workspace.local", When: time.Now()}
	var hash plumbing.Hash
	if place {
		hash, err = wt.Commit(PlaceholderMessage, &git.CommitOptions{
			Author: author, Committer: author, Amend: true, AllowEmptyCommits: true,
		})
	} else {
		hash, err = wt.Commit(PlaceholderMessage, &git.CommitOptions{
			Author: author, Committer: author,
		})
	}
	if err != nil && !errors.Is(err, git.ErrEmptyCommit) {
		return "", fmt.Errorf("commit: %w", err)
	}
	if err != nil { // ErrEmptyCommit: nothing changed; treat as no-op.
		ref, herr := c.repo.Head()
		if herr != nil {
			return "", herr
		}
		hash = ref.Hash()
	}
	if err := c.push(ctx, o); err != nil {
		return "", err
	}
	return hash.String(), nil
}

// Commit rewinds the placeholder to `message` and appends an EMPTY placeholder
// (opening a fresh staging area). With no placeholder at HEAD it fails.
func (m *Manager) Commit(ctx context.Context, o Options, message string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout(o.Timeout))
	defer cancel()
	if strings.TrimSpace(message) == "" {
		return "", errors.New("message is required")
	}
	c, err := m.clone(ctx, o)
	if err != nil {
		return "", err
	}
	commit, place, err := c.head()
	if err != nil {
		return "", err
	}
	if !place {
		return "", errors.New("nothing to commit (no open staging area)")
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return "", err
	}
	author := &object.Signature{Name: "workspace-gateway", Email: "gateway@workspace.local", When: time.Now()}
	// Reword the current placeholder to the caller's message (amend, same tree).
	tip, err := wt.Commit(message, &git.CommitOptions{
		Author: author, Committer: author, Amend: true, AllowEmptyCommits: true,
	})
	if err != nil && !errors.Is(err, git.ErrEmptyCommit) {
		return "", fmt.Errorf("commit: %w", err)
	}
	// Append an EMPTY placeholder whose parent is the just-committed message.
	empty, err := wt.Commit(PlaceholderMessage, &git.CommitOptions{
		Author: author, Committer: author,
		Parents:           []plumbing.Hash{tip},
		AllowEmptyCommits: true,
	})
	if err != nil {
		return "", fmt.Errorf("open staging: %w", err)
	}
	if err := c.push(ctx, o); err != nil {
		return "", err
	}
	_ = commit
	return empty.String(), nil
}

// push force-pushes the branch (session-exclusive, so rewriting is safe).
func (c *cloned) push(ctx context.Context, o Options) error {
	err := c.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/" + c.branch + ":refs/heads/" + c.branch)},
		Auth:       c.auth,
		Force:      true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// applyFile writes or removes one path in the worktree.
func applyFile(wt *git.Worktree, op FileOp) error {
	path := filepath.ToSlash(strings.TrimSpace(op.Path))
	if path == "" || strings.Contains(path, "..") || strings.HasPrefix(path, "/") {
		return fmt.Errorf("illegal path %q", op.Path)
	}
	if op.Op == "delete" {
		_, err := wt.Filesystem.Stat(path)
		if err != nil {
			return nil // already absent
		}
		return wt.Filesystem.Remove(path)
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := wt.Filesystem.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := wt.Filesystem.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, bytes.NewReader(op.Content)); err != nil {
		return err
	}
	return nil
}

// ChangedPaths lists the repo-relative paths that differ between two commits.
func ChangedPaths(ctx context.Context, repoURL, user, token, from, to string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(timeout))
	defer cancel()
	dir, err := os.MkdirTemp("", "gitdiff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	auth := &http.BasicAuth{Username: user, Password: token}
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: repoURL, Auth: auth, NoCheckout: true})
	if err != nil {
		return nil, err
	}
	fromHash, err := resolveCommit(repo, from)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", from, err)
	}
	toHash, err := resolveCommit(repo, to)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", to, err)
	}
	fromCommit, err := repo.CommitObject(fromHash)
	if err != nil {
		return nil, err
	}
	toCommit, err := repo.CommitObject(toHash)
	if err != nil {
		return nil, err
	}
	fromTree, err := fromCommit.Tree()
	if err != nil {
		return nil, err
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for _, ch := range changes {
		if ch.From.Name != "" {
			set[ch.From.Name] = struct{}{}
		}
		if ch.To.Name != "" {
			set[ch.To.Name] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// FileDiff returns a unified diff of one file between `base` and `head` refs,
// computed with go-git. Forgejo's compare API returns an EMPTY `patch` field
// (1.22), so the gateway produces the diff itself. An unchanged file yields "".
func FileDiff(ctx context.Context, repoURL, user, token, base, head, path string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(timeout))
	defer cancel()
	if path == "" {
		return "", errors.New("path is required")
	}
	dir, err := os.MkdirTemp("", "gitfilediff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	auth := &http.BasicAuth{Username: user, Password: token}
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: repoURL, Auth: auth, NoCheckout: true})
	if err != nil {
		return "", err
	}
	baseHash, err := resolveCommit(repo, base)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", base, err)
	}
	headHash, err := resolveCommit(repo, head)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", head, err)
	}
	baseCommit, err := repo.CommitObject(baseHash)
	if err != nil {
		return "", err
	}
	headCommit, err := repo.CommitObject(headHash)
	if err != nil {
		return "", err
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return "", err
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return "", err
	}
	changes, err := object.DiffTree(baseTree, headTree)
	if err != nil {
		return "", err
	}
	for i := range changes {
		ch := changes[i]
		if ch.From.Name != path && ch.To.Name != path {
			continue
		}
		patch, err := ch.Patch()
		if err != nil {
			return "", err
		}
		return patch.String(), nil
	}
	return "", nil // unchanged / absent on both sides
}

// BlameLine is one blamed line of a file.
type BlameLine struct {
	Line        int
	SHA         string
	Author      string
	AuthorEmail string
	Date        string // RFC3339
	Content     string
}

// Blame computes per-line authorship of `path` at `ref`. Forgejo has no blame
// API, so this clones the repo bare (no checkout) and uses go-git's Blame. A
// missing file or unknown ref returns an error.
func Blame(ctx context.Context, repoURL, user, token, ref, path string, timeout time.Duration) ([]BlameLine, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(timeout))
	defer cancel()
	if path == "" {
		return nil, errors.New("path is required")
	}
	dir, err := os.MkdirTemp("", "gitblame-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	auth := &http.BasicAuth{Username: user, Password: token}
	// Non-bare with NoCheckout (matches ChangedPaths): a bare clone requires a
	// valid remote HEAD, which an empty/pushed-fresh repo may not have yet.
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: repoURL, Auth: auth, NoCheckout: true})
	if err != nil {
		return nil, err
	}
	hash, err := resolveCommit(repo, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, err
	}
	res, err := git.Blame(commit, path)
	if err != nil {
		return nil, err
	}
	out := make([]BlameLine, 0, len(res.Lines))
	for i, l := range res.Lines {
		out = append(out, BlameLine{
			Line:        i + 1,
			SHA:         l.Hash.String(),
			Author:      l.AuthorName,
			AuthorEmail: l.Author,
			Date:        l.Date.UTC().Format(time.RFC3339),
			Content:     l.Text,
		})
	}
	return out, nil
}

func timeoutOf(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Minute
	}
	return d
}

// resolveCommit resolves a ref sha or a branch name to a hash (prefers a ref).
func resolveCommit(repo *git.Repository, ref string) (plumbing.Hash, error) {
	if h, err := repo.ResolveRevision(plumbing.Revision(ref)); err == nil {
		return *h, nil
	}
	for _, name := range []string{ref, "origin/" + ref} {
		if r, err := repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
			return r.Hash(), nil
		}
		if r, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", name), true); err == nil {
			return r.Hash(), nil
		}
	}
	return plumbing.ZeroHash, fmt.Errorf("unknown ref %q", ref)
}

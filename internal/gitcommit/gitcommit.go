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
//	fresh branch (HEAD == main tip, or a normal commit)
//	  write/edit/delete  -> commit a new placeholder "ABCP_XXX" with the change
//	  repo-commit(msg)   -> error: nothing is staged
//	staging open (HEAD == placeholder with changes)
//	  write/edit/delete  -> AMEND the placeholder with the change
//	  repo-commit(msg)   -> reword the placeholder to msg, CLOSING the staging
//	                        area (HEAD becomes a normal commit; the next write
//	                        opens a fresh placeholder)
//
// So `repo-commit` never appends an empty commit. A placeholder whose tree
// equals its parent's tree is "clean" (no staged change); a placeholder that
// differs carries staged work. Sync/MR refuse the latter.
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
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
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

// ErrNoChanges is returned by ApplyFiles when applying the given operations
// produced NO change to the commit tree. This happens when the content is
// identical to what is already committed, or — the classic footgun — the target
// path is IGNORED by the repository's `.gitignore`, so git never stages it.
// Reporting this as a SUCCESS (the old behaviour returned the parent commit sha)
// silently swallowed failed writes.
var ErrNoChanges = errors.New("no changes to commit (the content is unchanged, or the target path is ignored by the repository's .gitignore)")

// ErrIgnoredPath is returned by ApplyFiles when a target path is excluded by
// the repository's `.gitignore` (git would never stage it).
var ErrIgnoredPath = errors.New("target path is ignored by the repository's .gitignore")

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
	// Compute the repo's .gitignore matcher ONCE: a write to an ignored path
	// would never be staged (git skips it), so we refuse it up front with a
	// precise message instead of silently producing an empty commit.
	ignored, err := gitignoreMatcher(wt)
	if err != nil {
		return "", err
	}
	for _, op := range ops {
		if err := applyFile(wt, op, ignored); err != nil {
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
	// A placeholder is AMENDED with AllowEmptyCommits, so an unchanged tree would
	// otherwise slip through as a bogus "success" (the old bug: a write to an
	// ignored/identical path reported the parent sha). Check the staged state
	// explicitly: nothing staged relative to HEAD means nothing changed.
	staged, err := hasStagedChanges(wt)
	if err != nil {
		return "", err
	}
	if !staged {
		return "", ErrNoChanges
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
	if errors.Is(err, git.ErrEmptyCommit) {
		// Nothing changed. NEVER report this as success (it used to return the
		// parent sha, so a write to an ignored path looked like it worked).
		return "", ErrNoChanges
	}
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	if err := c.push(ctx, o); err != nil {
		return "", err
	}
	return hash.String(), nil
}

// Commit rewinds the placeholder to `message` (CLOSING the staging area: HEAD
// becomes a normal commit; the next write opens a fresh placeholder). With no
// placeholder at HEAD — or a placeholder that carries no change — it fails, so
// it can never create an empty commit.
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
	empty, err := c.commitIsEmpty(commit)
	if err != nil {
		return "", err
	}
	if empty {
		return "", errors.New("nothing to commit (staging area is empty)")
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return "", err
	}
	author := &object.Signature{Name: "workspace-gateway", Email: "gateway@workspace.local", When: time.Now()}
	// Reword the current placeholder to the caller's message (amend, same tree).
	// This closes the staging area: HEAD is now a normal commit.
	tip, err := wt.Commit(message, &git.CommitOptions{
		Author: author, Committer: author, Amend: true, AllowEmptyCommits: true,
	})
	if err != nil && !errors.Is(err, git.ErrEmptyCommit) {
		return "", fmt.Errorf("commit: %w", err)
	}
	if err := c.push(ctx, o); err != nil {
		return "", err
	}
	return tip.String(), nil
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

// hasStagedChanges reports whether the worktree/index differs from HEAD after
// staging. Used to reject a no-op ApplyFiles BEFORE an AllowEmptyCommits amend,
// which would otherwise commit an unchanged tree and report a bogus success.
func hasStagedChanges(wt *git.Worktree) (bool, error) {
	st, err := wt.Status()
	if err != nil {
		return false, fmt.Errorf("status: %w", err)
	}
	return !st.IsClean(), nil
}

// gitignoreMatcher builds a matcher from the worktree's .gitignore files (and
// the repo's exclude file). Returns nil when there are no patterns, so callers
// can skip the check cheaply.
func gitignoreMatcher(wt *git.Worktree) (gitignore.Matcher, error) {
	patterns, err := gitignore.ReadPatterns(wt.Filesystem, nil)
	if err != nil {
		return nil, fmt.Errorf("read .gitignore: %w", err)
	}
	patterns = append(patterns, wt.Excludes...)
	if len(patterns) == 0 {
		return nil, nil
	}
	return gitignore.NewMatcher(patterns), nil
}

// applyFile writes or removes one path in the worktree. A non-nil `ignored`
// matcher refuses a path the repository's `.gitignore` excludes (git would skip
// it, yielding an empty commit).
func applyFile(wt *git.Worktree, op FileOp, ignored gitignore.Matcher) error {
	path := filepath.ToSlash(strings.TrimSpace(op.Path))
	if path == "" || strings.Contains(path, "..") || strings.HasPrefix(path, "/") {
		return fmt.Errorf("illegal path %q", op.Path)
	}
	if ignored != nil && ignored.Match(strings.Split(path, "/"), false) {
		return fmt.Errorf("%w: %s", ErrIgnoredPath, path)
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

// FileChange is one file that differs between two refs.
type FileChange struct {
	Path      string
	Status    string // added | modified | deleted | renamed
	Additions int
	Deletions int
	Patch     string
}

// Compare returns the per-file changes between `base` and `head` (a `base..head`
// tree diff) with unified patches, computed with go-git. Forgejo's compare API
// returns an EMPTY `patch` field (1.22), so the gateway produces the whole
// comparison itself in ONE clone.
func Compare(ctx context.Context, repoURL, user, token, base, head string, timeout time.Duration) ([]FileChange, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(timeout))
	defer cancel()
	dir, err := os.MkdirTemp("", "gitcompare-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	auth := &http.BasicAuth{Username: user, Password: token}
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: repoURL, Auth: auth, NoCheckout: true})
	if err != nil {
		return nil, err
	}
	baseHash, err := resolveCommit(repo, base)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", base, err)
	}
	headHash, err := resolveCommit(repo, head)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", head, err)
	}
	baseCommit, err := repo.CommitObject(baseHash)
	if err != nil {
		return nil, err
	}
	headCommit, err := repo.CommitObject(headHash)
	if err != nil {
		return nil, err
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, err
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTree(baseTree, headTree)
	if err != nil {
		return nil, err
	}
	out := make([]FileChange, 0, len(changes))
	for i := range changes {
		ch := changes[i]
		patch, perr := ch.Patch()
		if perr != nil {
			return nil, perr
		}
		fc := FileChange{
			Path:   changePath(ch),
			Status: changeStatus(ch),
			Patch:  patch.String(),
		}
		for _, fp := range patch.FilePatches() {
			_, to := fp.Files()
			_ = to
			add, del := patchStats(fp)
			fc.Additions += add
			fc.Deletions += del
		}
		out = append(out, fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// changePath picks the destination path (falling back to the source on delete).
func changePath(ch *object.Change) string {
	if ch.To.Name != "" {
		return ch.To.Name
	}
	return ch.From.Name
}

// changeStatus maps a go-git change onto added/modified/deleted/renamed.
func changeStatus(ch *object.Change) string {
	switch {
	case ch.From.Name == "":
		return "added"
	case ch.To.Name == "":
		return "deleted"
	case ch.From.Name != ch.To.Name:
		return "renamed"
	default:
		return "modified"
	}
}

// patchStats counts added/deleted lines in a file patch.
func patchStats(fp diff.FilePatch) (add, del int) {
	for _, c := range fp.Chunks() {
		switch c.Type() {
		case diff.Add:
			add += strings.Count(c.Content(), "\n")
			if !strings.HasSuffix(c.Content(), "\n") && c.Content() != "" {
				add++
			}
		case diff.Delete:
			del += strings.Count(c.Content(), "\n")
			if !strings.HasSuffix(c.Content(), "\n") && c.Content() != "" {
				del++
			}
		}
	}
	return add, del
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

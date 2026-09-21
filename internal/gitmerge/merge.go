package gitmerge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Options configures a branch sync.
type Options struct {
	// RepoURL is the HTTP(S) clone/push URL of the repository.
	RepoURL string
	// Branch is the feature branch to update (NOT the base).
	Branch string
	// Base is the branch to integrate (normally `main`).
	Base string
	// User + Token authenticate clone and push.
	User  string
	Token string
	// Timeout bounds the whole operation. Zero = 10 minutes.
	Timeout time.Duration
}

// Result reports the outcome.
type Result struct {
	// Clean is true when no conflict markers were written.
	Clean bool
	// Conflicts lists the repo-relative paths carrying markers.
	Conflicts []string
	// Commit is the new branch tip.
	Commit string
}

// Sync integrates `opts.Base` into `opts.Branch` with a two-parent merge
// commit on the branch, writing conflict markers where a three-way merge
// cannot decide. It never touches the base branch.
func Sync(ctx context.Context, opts Options) (Result, error) {
	if opts.RepoURL == "" || opts.Branch == "" || opts.Base == "" {
		return Result{}, errors.New("repo url, branch and base are required")
	}
	if opts.Branch == opts.Base {
		return Result{}, errors.New("cannot sync a branch into itself")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "gitmerge-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	auth := &http.BasicAuth{Username: opts.User, Password: opts.Token}
	branchRef := plumbing.NewBranchReferenceName(opts.Branch)

	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           opts.RepoURL,
		Auth:          auth,
		ReferenceName: branchRef,
		SingleBranch:  true,
	})
	if err != nil {
		return Result{}, fmt.Errorf("clone %s: %w", opts.Branch, err)
	}

	rem, err := repo.Remote("origin")
	if err != nil {
		return Result{}, fmt.Errorf("origin remote: %w", err)
	}
	err = rem.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/" + opts.Base + ":refs/remotes/origin/" + opts.Base)},
		Auth:     auth,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return Result{}, fmt.Errorf("fetch %s: %w", opts.Base, err)
	}

	branchTip, err := repo.Reference(branchRef, true)
	if err != nil {
		return Result{}, fmt.Errorf("resolve branch tip: %w", err)
	}
	baseTip, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", opts.Base), true)
	if err != nil {
		return Result{}, fmt.Errorf("resolve base tip: %w", err)
	}
	if branchTip.Hash() == baseTip.Hash() {
		return Result{Clean: true, Commit: branchTip.Hash().String()}, nil
	}

	branchCommit, err := repo.CommitObject(branchTip.Hash())
	if err != nil {
		return Result{}, err
	}
	baseCommit, err := repo.CommitObject(baseTip.Hash())
	if err != nil {
		return Result{}, err
	}

	// Already up to date: the base tip is an ancestor of the branch tip, so
	// their merge base IS the base tip.
	bases, err := branchCommit.MergeBase(baseCommit)
	if err != nil {
		return Result{}, fmt.Errorf("merge base: %w", err)
	}
	if len(bases) == 0 {
		return Result{}, errors.New("no merge base between branch and base")
	}
	mergeBase := bases[0]
	if mergeBase.Hash == baseTip.Hash() {
		return Result{Clean: true, Commit: branchTip.Hash().String()}, nil
	}

	baseTree, err := mergeBase.Tree()
	if err != nil {
		return Result{}, err
	}
	mainTree, err := baseCommit.Tree()
	if err != nil {
		return Result{}, err
	}
	branchTree, err := branchCommit.Tree()
	if err != nil {
		return Result{}, err
	}

	// Candidate paths: changed on EITHER side since the merge base. Rename
	// detection is OFF so a rename appears as delete+add and BOTH survive
	// (the rename/rename policy: keep both files).
	noRenames := &object.DiffTreeOptions{DetectRenames: false}
	changed := map[string]struct{}{}
	for _, pair := range [][2]*object.Tree{{baseTree, mainTree}, {baseTree, branchTree}} {
		changes, err := object.DiffTreeWithOptions(ctx, pair[0], pair[1], noRenames)
		if err != nil {
			return Result{}, fmt.Errorf("diff tree: %w", err)
		}
		for _, ch := range changes {
			if ch.To.Name != "" {
				changed[ch.To.Name] = struct{}{}
			}
			if ch.From.Name != "" {
				changed[ch.From.Name] = struct{}{}
			}
		}
	}

	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	wt, err := repo.Worktree()
	if err != nil {
		return Result{}, err
	}

	var conflicts []string
	for _, p := range paths {
		baseRaw, err := readPath(repo, baseTree, p)
		if err != nil {
			return Result{}, err
		}
		oursRaw, err := readPath(repo, branchTree, p)
		if err != nil {
			return Result{}, err
		}
		theirsRaw, err := readPath(repo, mainTree, p)
		if err != nil {
			return Result{}, err
		}

		res, err := resolvePath(baseRaw, oursRaw, theirsRaw)
		if err != nil {
			return Result{}, fmt.Errorf("resolve %s: %w", p, err)
		}
		if err := applyResult(wt, p, res); err != nil {
			return Result{}, fmt.Errorf("apply %s: %w", p, err)
		}
		if res.conflict {
			conflicts = append(conflicts, p)
		}
	}

	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return Result{}, fmt.Errorf("stage: %w", err)
	}

	author := &object.Signature{Name: "workspace-gateway", Email: "gateway@workspace.local", When: time.Now()}
	commitHash, err := wt.Commit(
		fmt.Sprintf("Merge branch '%s' into %s (workspace sync)", opts.Base, opts.Branch),
		&git.CommitOptions{
			Author:    author,
			Committer: author,
			// First parent = the feature branch (ours); second = base. The
			// base tip becomes an ancestor of the branch, so the MR is
			// mergeable once the markers are resolved.
			Parents: []plumbing.Hash{branchTip.Hash(), baseTip.Hash()},
		},
	)
	if err != nil && !errors.Is(err, git.ErrEmptyCommit) {
		return Result{}, fmt.Errorf("commit: %w", err)
	}

	if err := repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/" + opts.Branch + ":refs/heads/" + opts.Branch)},
		Auth:       auth,
		Force:      true,
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		return Result{}, fmt.Errorf("push: %w", err)
	}

	return Result{Clean: len(conflicts) == 0, Conflicts: conflicts, Commit: commitHash.String()}, nil
}

// pathContent is one path's raw bytes when present in a tree. present=false
// means the path does not exist in that tree.
type pathContent struct {
	present bool
	data    []byte
	// isDir marks a tree/submodule entry (not mergeable as text).
	isDir bool
}

// readPath reads a path's blob from a tree (absent -> present=false).
// Directory/submodule entries report isDir=true and no data.
func readPath(repo *git.Repository, tree *object.Tree, path string) (pathContent, error) {
	if tree == nil {
		return pathContent{}, nil
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		return pathContent{}, nil
	}
	if !entry.Mode.IsFile() {
		return pathContent{isDir: true}, nil
	}
	blob, err := object.GetBlob(repo.Storer, entry.Hash)
	if err != nil {
		return pathContent{}, err
	}
	rd, err := blob.Reader()
	if err != nil {
		return pathContent{}, err
	}
	defer rd.Close()
	data, err := io.ReadAll(rd)
	if err != nil {
		return pathContent{}, err
	}
	return pathContent{present: true, data: data}, nil
}

// applyResult writes a path's merge result into the worktree (or removes it).
func applyResult(wt *git.Worktree, path string, res mergeResult) error {
	if res.omit {
		if err := wt.Filesystem.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
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
	if _, err := io.Copy(f, bytes.NewReader(res.content)); err != nil {
		return err
	}
	return nil
}

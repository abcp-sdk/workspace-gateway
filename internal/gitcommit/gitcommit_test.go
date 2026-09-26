package gitcommit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// setupBranch builds a bare origin with `main` and a `feature` branch, and
// returns the repo URL.
func setupBranch(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	write(t, wt, "f.txt", "l1\nl2\n")
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature"), Create: true}); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(dir, "origin.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	rem, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rem.Push(&git.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{
		"+refs/heads/master:refs/heads/main",
		"+refs/heads/feature:refs/heads/feature",
	}}); err != nil {
		t.Fatal(err)
	}
	// Point the bare origin's HEAD at `main` (as Forgejo does): a clone needs a
	// resolvable remote HEAD, and the default init branch (`master`) was never
	// pushed.
	bareRepo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	if err := bareRepo.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	return bare
}

func write(t *testing.T, wt *git.Worktree, file, content string) {
	t.Helper()
	p := filepath.Join(wt.Filesystem.Root(), file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(file); err != nil {
		t.Fatal(err)
	}
}

// headMessage returns the tip commit's message on a branch.
func headMessage(t *testing.T, repoURL, branch string) (string, int, string) {
	t.Helper()
	repo, err := git.PlainOpen(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c.Message, c.NumParents(), c.Hash.String()
}

func TestStagingLifecycle(t *testing.T) {
	url := setupBranch(t)
	m := NewManager()
	opts := Options{RepoURL: url, Branch: "feature", Timeout: 60 * time.Second}
	ctx := context.Background()

	// Fresh branch: no placeholder, clean.
	st, err := m.Status(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Placeholder || st.Staged {
		t.Fatalf("fresh branch must be clean: %+v", st)
	}

	// First write: opens a placeholder WITH the change (staged).
	sha1, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "f.txt", Op: "update", Content: []byte("l1\nl2\nl3\n")}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	msg, _, _ := headMessage(t, url, "feature")
	if !IsPlaceholder(msg) {
		t.Fatalf("HEAD must be the placeholder, got %q", msg)
	}
	st, _ = m.Status(ctx, opts)
	if !st.Placeholder || !st.Staged {
		t.Fatalf("after first write must be placeholder+staged: %+v", st)
	}
	if st.MergeTip == st.Tip {
		t.Fatal("merge tip must exclude the placeholder")
	}

	// Second write: AMENDS (still one placeholder, same merge tip parent).
	sha2, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "g.txt", Op: "create", Content: []byte("new\n")}})
	if err != nil {
		t.Fatalf("apply2: %v", err)
	}
	if sha2 == sha1 {
		t.Fatal("amend must produce a new sha")
	}
	st, _ = m.Status(ctx, opts)
	if !st.Placeholder || !st.Staged {
		t.Fatalf("after second write must still be placeholder+staged: %+v", st)
	}

	// Commit: rewinds the placeholder to the message and CLOSES staging. HEAD
	// is now a normal commit (not a placeholder); no empty commit is appended.
	head, err := m.Commit(ctx, opts, "add l3 and g")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	msg, parents, _ := headMessage(t, url, "feature")
	if msg != "add l3 and g" {
		t.Fatalf("HEAD after commit must be the message commit, got %q", msg)
	}
	if IsPlaceholder(msg) {
		t.Fatal("HEAD after commit must NOT be a placeholder")
	}
	if parents != 1 {
		t.Fatalf("message commit must have 1 parent, got %d", parents)
	}
	st, _ = m.Status(ctx, opts)
	if st.Placeholder || st.Staged {
		t.Fatalf("after commit must be clean (no placeholder): %+v", st)
	}
	if st.Tip != head || st.MergeTip != head {
		t.Fatalf("tip %s / mergeTip %s != returned %s", st.Tip, st.MergeTip, head)
	}
	// Committing again with nothing staged must fail (never an empty commit).
	if _, err := m.Commit(ctx, opts, "again"); err == nil {
		t.Fatal("commit with nothing staged must fail")
	}
	// A further write opens a FRESH placeholder on top of the real commit.
	sha3, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "h.txt", Op: "create", Content: []byte("more\n")}})
	if err != nil {
		t.Fatalf("apply3: %v", err)
	}
	msg, parents, _ = headMessage(t, url, "feature")
	if !IsPlaceholder(msg) || parents != 1 {
		t.Fatalf("after a later write HEAD must be a fresh placeholder, got %q (%d parents)", msg, parents)
	}
	st, _ = m.Status(ctx, opts)
	if !st.Placeholder || !st.Staged || st.Tip != sha3 {
		t.Fatalf("after later write must be placeholder+staged: %+v", st)
	}
}

func TestBlame(t *testing.T) {
	repoURL := setupBranch(t)
	lines, err := Blame(context.Background(), repoURL, "", "", "main", "f.txt", 30*time.Second)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	if lines[0].Line != 1 || lines[0].Content != "l1" || lines[0].Author != "t" || lines[0].SHA == "" {
		t.Fatalf("bad first line: %+v", lines[0])
	}
	if lines[1].Line != 2 || lines[1].Content != "l2" {
		t.Fatalf("bad second line: %+v", lines[1])
	}
}

func TestFileDiff(t *testing.T) {
	repoURL := setupBranch(t)
	// `main` and `feature` share the same commit in the fixture; diff main vs
	// main yields nothing, and an unknown path also yields nothing.
	if d, err := FileDiff(context.Background(), repoURL, "", "", "main", "main", "f.txt", 30*time.Second); err != nil || d != "" {
		t.Fatalf("self diff = %q, %v (want empty)", d, err)
	}
	if _, err := FileDiff(context.Background(), repoURL, "", "", "main", "main", "", 30*time.Second); err == nil {
		t.Fatal("empty path must error")
	}
}

func TestBlameMissingFile(t *testing.T) {
	repoURL := setupBranch(t)
	if _, err := Blame(context.Background(), repoURL, "", "", "main", "nope.txt", 30*time.Second); err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if _, err := Blame(context.Background(), repoURL, "", "", "main", "", 30*time.Second); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}

func TestCommitRequiresPlaceholder(t *testing.T) {
	url := setupBranch(t)
	m := NewManager()
	opts := Options{RepoURL: url, Branch: "feature", Timeout: 60 * time.Second}
	if _, err := m.Commit(context.Background(), opts, "x"); err == nil {
		t.Fatal("commit with no placeholder must fail")
	}
	if _, err := m.Commit(context.Background(), opts, "  "); err == nil {
		t.Fatal("empty message must fail")
	}
}

// TestStripTrailingPlaceholder verifies the MR-time backstop: a CLEAN trailing
// TestApplyFilesNoChanges pins that an ApplyFiles which produces NO tree change
// (identical content, or an ignored path) FAILS with ErrNoChanges instead of
// silently reporting success with the parent commit sha.
func TestApplyFilesNoChanges(t *testing.T) {
	url := setupBranch(t)
	m := NewManager()
	opts := Options{RepoURL: url, Branch: "feature", Timeout: 60 * time.Second}
	ctx := context.Background()

	// Commit a real change first.
	if _, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "f.txt", Op: "update", Content: []byte("x\n")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(ctx, opts, "change"); err != nil {
		t.Fatal(err)
	}

	// Applying the IDENTICAL content produces no tree change: it must FAIL with
	// ErrNoChanges, never report success.
	_, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "f.txt", Op: "update", Content: []byte("x\n")}})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("identical write err = %v, want ErrNoChanges", err)
	}

	// Same when a placeholder is ALREADY open (the amend path): stage one real
	// change, then re-apply it unchanged.
	if _, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "f.txt", Op: "update", Content: []byte("y\n")}}); err != nil {
		t.Fatal(err)
	}
	_, err = m.ApplyFiles(ctx, opts, []FileOp{{Path: "f.txt", Op: "update", Content: []byte("y\n")}})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("identical amend err = %v, want ErrNoChanges", err)
	}
}

func TestApplyFilesRejectsIgnoredPath(t *testing.T) {
	url := setupBranch(t)
	m := NewManager()
	opts := Options{RepoURL: url, Branch: "feature", Timeout: 60 * time.Second}
	ctx := context.Background()

	// Land a .gitignore that excludes `/easyvcs` (the easy-vcs footgun).
	if _, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: ".gitignore", Op: "create", Content: []byte("/easyvcs\n")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(ctx, opts, "add gitignore"); err != nil {
		t.Fatal(err)
	}

	// Writing under the ignored directory must be REFUSED (git would skip it and
	// the old code reported an empty-commit success).
	_, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "easyvcs/store/store.go", Op: "create", Content: []byte("package store\n")}})
	if !errors.Is(err, ErrIgnoredPath) {
		t.Fatalf("ignored write err = %v, want ErrIgnoredPath", err)
	}
	// A non-ignored path still works.
	if _, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "store/store.go", Op: "create", Content: []byte("package store\n")}}); err != nil {
		t.Fatalf("non-ignored write: %v", err)
	}
}

func TestStripTrailingPlaceholder(t *testing.T) {
	url := setupBranch(t)
	m := NewManager()
	opts := Options{RepoURL: url, Branch: "feature", Timeout: 60 * time.Second}
	ctx := context.Background()

	// A real commit via the normal write+commit path.
	if _, err := m.ApplyFiles(ctx, opts, []FileOp{{Path: "f.txt", Op: "update", Content: []byte("l1\nl2\nl3\n")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(ctx, opts, "add l3"); err != nil {
		t.Fatal(err)
	}
	_, _, real := headMessage(t, url, "feature")

	// Append a CLEAN placeholder (empty, parent = the real commit) out of band,
	// exactly what a write-then-revert would leave behind.
	work := t.TempDir()
	repo, err := git.PlainClone(work, false, &git.CloneOptions{
		URL:           url,
		ReferenceName: plumbing.NewBranchReferenceName("feature"),
		SingleBranch:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	if _, err := wt.Commit(PlaceholderMessage, &git.CommitOptions{
		Author: sig, Committer: sig, AllowEmptyCommits: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/feature:refs/heads/feature")},
		Force:      true,
	}); err != nil {
		t.Fatal(err)
	}

	st, err := m.Status(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Placeholder || st.Staged {
		t.Fatalf("want clean placeholder, got %+v", st)
	}
	// Strip it: branch returns to the real commit, no placeholder.
	if err := m.ResetTo(ctx, opts, st.MergeTip); err != nil {
		t.Fatalf("reset: %v", err)
	}
	st2, err := m.Status(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Placeholder {
		t.Fatalf("placeholder must be gone, got %+v", st2)
	}
	if st2.Tip != real {
		t.Fatalf("tip %s != real commit %s", st2.Tip, real)
	}
	msg, parents, _ := headMessage(t, url, "feature")
	if msg != "add l3" || parents != 1 {
		t.Fatalf("HEAD after strip = %q (%d parents), want 'add l3' (1)", msg, parents)
	}
}

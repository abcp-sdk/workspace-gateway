package gitcommit

import (
	"context"
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

	// Commit: rewinds placeholder to the message, opens a fresh EMPTY placeholder.
	head, err := m.Commit(ctx, opts, "add l3 and g")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	msg, parents, _ := headMessage(t, url, "feature")
	if !IsPlaceholder(msg) {
		t.Fatalf("HEAD after commit must be a fresh placeholder, got %q", msg)
	}
	if parents != 1 {
		t.Fatalf("fresh placeholder must have 1 parent, got %d", parents)
	}
	st, _ = m.Status(ctx, opts)
	if !st.Placeholder || st.Staged {
		t.Fatalf("after commit must be placeholder but CLEAN: %+v", st)
	}
	if st.Tip != head {
		t.Fatalf("tip %s != returned %s", st.Tip, head)
	}
	// The message commit must carry the caller's message.
	repo, _ := git.PlainOpen(url)
	ref, _ := repo.Reference(plumbing.NewBranchReferenceName("feature"), true)
	tipC, _ := repo.CommitObject(ref.Hash())
	parentC, _ := tipC.Parent(0)
	if parentC.Message != "add l3 and g" {
		t.Fatalf("parent message = %q", parentC.Message)
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

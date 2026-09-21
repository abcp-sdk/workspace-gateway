package gitmerge

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

// writeCommit writes one file and commits it on the current HEAD.
func writeCommit(t *testing.T, repo *git.Repository, file, content, msg string) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
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
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	h, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// setupRepo builds a bare "origin" with `main` and `feature` that conflict on
// f.txt, and returns the file:// URL.
func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	// Base commit on master (go-git default), then rename to main.
	writeCommit(t, repo, "f.txt", "l1\nl2\nl3\nl4\nl5\n", "base")
	// Feature branch: edit l2 and l4 (disjoint-ish) plus add a file.
	wt, _ := repo.Worktree()
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature"), Create: true}); err != nil {
		t.Fatal(err)
	}
	writeCommit(t, repo, "f.txt", "l1\nFEAT2\nl3\nFEAT4\nl5\n", "feature edits")
	writeCommit(t, repo, "only-feature.txt", "mine\n", "feature file")
	// main: edit l2 DIFFERENTLY (conflict) and l4 SAME (false conflict).
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}); err != nil {
		t.Fatal(err)
	}
	writeCommit(t, repo, "f.txt", "l1\nMAIN2\nl3\nFEAT4\nl5\n", "main edits")
	writeCommit(t, repo, "only-main.txt", "theirs\n", "main file")

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
	return bare
}

func TestSyncWritesMarkersAndTwoParentCommit(t *testing.T) {
	bare := setupRepo(t)
	res, err := Sync(context.Background(), Options{
		RepoURL: bare,
		Branch:  "feature",
		Base:    "main",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Clean {
		t.Fatal("expected conflicts (l2 edited differently on both sides)")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "f.txt" {
		t.Fatalf("conflicts = %v, want [f.txt]", res.Conflicts)
	}

	// Inspect the pushed branch: two parents, base is parent[1], and the
	// merged tree carries markers but also the cleanly-merged main file.
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := repo.Reference(plumbing.NewBranchReferenceName("feature"), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CommitObject(tip.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if c.NumParents() != 2 {
		t.Fatalf("parents = %d, want 2", c.NumParents())
	}
	mainTip, _ := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if c.ParentHashes[1] != mainTip.Hash() {
		t.Fatal("second parent must be the main tip")
	}
	// The main tip must now be an ancestor of the synced branch: their merge
	// base IS the main tip.
	mainCommit, _ := repo.CommitObject(mainTip.Hash())
	bases, err := c.MergeBase(mainCommit)
	if err != nil || len(bases) == 0 {
		t.Fatalf("merge base: %v", err)
	}
	if bases[0].Hash != mainTip.Hash() {
		t.Fatal("main tip must be an ancestor of the synced branch")
	}

	blob, err := c.File("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := blob.Contents()
	if !HasMarkers([]byte(content)) {
		t.Fatalf("f.txt must carry markers:\n%s", content)
	}
	// only-main.txt (untouched by the branch) must be present (taken from main).
	if _, err := c.File("only-main.txt"); err != nil {
		t.Fatalf("only-main.txt must be taken from main: %v", err)
	}
	// only-feature.txt must survive.
	if _, err := c.File("only-feature.txt"); err != nil {
		t.Fatalf("only-feature.txt must survive: %v", err)
	}
}

func TestSyncAlreadyCurrentIsNoop(t *testing.T) {
	bare := setupRepo(t)
	// First sync brings feature up to date with main.
	if _, err := Sync(context.Background(), Options{RepoURL: bare, Branch: "feature", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	before, _ := git.PlainOpen(bare)
	ref, _ := before.Reference(plumbing.NewBranchReferenceName("feature"), true)

	// Second sync: base is already an ancestor -> clean no-op.
	res, err := Sync(context.Background(), Options{RepoURL: bare, Branch: "feature", Base: "main"})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if !res.Clean {
		t.Fatal("second sync must be clean/no-op")
	}
	if res.Commit != ref.Hash().String() {
		t.Fatal("already-current sync must not move the branch")
	}
}

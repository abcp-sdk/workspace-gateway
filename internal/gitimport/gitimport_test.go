package gitimport

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

// seedSource builds a local repo with two branches and a tag, plus a
// `refs/pull/1/head` ref (the shape that makes mirror clones pathological).
// It returns the file:// URL of the bare source.
func seedSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	commit, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	head := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commit)
	if err := repo.Storer.SetReference(head); err != nil {
		t.Fatal(err)
	}
	// A second branch off the same commit.
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("dev"), commit)); err != nil {
		t.Fatal(err)
	}
	// A tag + a pull ref that a mirror clone would also carry.
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1"), commit)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/pull/1/head"), commit)); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(dir, "src.git")
	if _, err := git.PlainInit(src, true); err != nil {
		t.Fatal(err)
	}
	// Push every head + the tag into the bare source (a plain clone would only
	// bring the checked-out branch).
	rem, err := repo.CreateRemote(&config.RemoteConfig{Name: "src", URLs: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rem.Push(&git.PushOptions{RemoteName: "src", RefSpecs: []config.RefSpec{
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
	}}); err != nil {
		t.Fatal(err)
	}
	// The bare source's HEAD must point at main, plus a pull ref that a mirror
	// clone would also carry (the test asserts we do NOT fetch it).
	srcRepo, err := git.PlainOpen(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := srcRepo.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	if err := srcRepo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/pull/1/head"), commit)); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestImportDefaultRefLandsOnMain(t *testing.T) {
	src := seedSource(t)
	dst := filepath.Join(t.TempDir(), "dst.git")
	if _, err := git.PlainInit(dst, true); err != nil {
		t.Fatal(err)
	}

	res, err := Import(context.Background(), Options{
		SourceURL: src,
		DestURL:   dst,
		Timeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main", res.DefaultBranch)
	}
	if res.SourceRef != "main" {
		t.Fatalf("SourceRef = %q, want main", res.SourceRef)
	}
	if len(res.Branches) != 1 || res.Branches[0] != "main" {
		t.Fatalf("Branches = %v, want [main]", res.Branches)
	}

	// The destination must have ONLY refs/heads/main — never the source's other
	// heads, the tag, or the pull ref.
	dstRepo, err := git.PlainOpen(dst)
	if err != nil {
		t.Fatal(err)
	}
	iter, err := dstRepo.References()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	_ = iter.ForEach(func(r *plumbing.Reference) error {
		names = append(names, r.Name().String())
		return nil
	})
	if !contains(names, "refs/heads/main") {
		t.Fatalf("destination missing refs/heads/main: %v", names)
	}
	for _, bad := range []string{"refs/heads/dev", "refs/tags/v1", "refs/pull/1/head"} {
		if contains(names, bad) {
			t.Fatalf("destination leaked %s: %v", bad, names)
		}
	}
}

// TestImportBranchAsMain imports a NON-main source branch and asserts it lands
// on the destination's `main`.
func TestImportBranchAsMain(t *testing.T) {
	src := seedSource(t)
	dst := filepath.Join(t.TempDir(), "dst.git")
	if _, err := git.PlainInit(dst, true); err != nil {
		t.Fatal(err)
	}

	res, err := Import(context.Background(), Options{
		SourceURL: src,
		DestURL:   dst,
		Ref:       "dev",
		Timeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.DefaultBranch != "main" || res.SourceRef != "dev" {
		t.Fatalf("got DefaultBranch=%q SourceRef=%q, want main/dev", res.DefaultBranch, res.SourceRef)
	}
	if len(res.Branches) != 1 || res.Branches[0] != "main" {
		t.Fatalf("Branches = %v, want [main]", res.Branches)
	}
	dstRepo, _ := git.PlainOpen(dst)
	if _, err := dstRepo.Reference(plumbing.NewBranchReferenceName("main"), false); err != nil {
		t.Fatalf("destination missing main: %v", err)
	}
	if _, err := dstRepo.Reference(plumbing.NewBranchReferenceName("dev"), false); err == nil {
		t.Fatal("destination must not have the source branch name `dev`")
	}
}

// TestImportTagAsMain imports a TAG and asserts it becomes the destination main.
func TestImportTagAsMain(t *testing.T) {
	src := seedSource(t)
	dst := filepath.Join(t.TempDir(), "dst.git")
	if _, err := git.PlainInit(dst, true); err != nil {
		t.Fatal(err)
	}

	res, err := Import(context.Background(), Options{
		SourceURL: src,
		DestURL:   dst,
		Ref:       "v1",
		Timeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Import tag: %v", err)
	}
	if res.DefaultBranch != "main" || res.SourceRef != "v1" {
		t.Fatalf("got DefaultBranch=%q SourceRef=%q, want main/v1", res.DefaultBranch, res.SourceRef)
	}
	dstRepo, _ := git.PlainOpen(dst)
	if _, err := dstRepo.Reference(plumbing.NewBranchReferenceName("main"), false); err != nil {
		t.Fatalf("destination missing main: %v", err)
	}
}

func TestImportMissingRef(t *testing.T) {
	src := seedSource(t)
	dst := filepath.Join(t.TempDir(), "dst.git")
	if _, err := git.PlainInit(dst, true); err != nil {
		t.Fatal(err)
	}
	_, err := Import(context.Background(), Options{
		SourceURL: src,
		DestURL:   dst,
		Ref:       "nope",
		Timeout:   20 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error for a missing ref")
	}
}

func TestImportRejectsEmptyArgs(t *testing.T) {
	if _, err := Import(context.Background(), Options{DestURL: "x"}); err == nil {
		t.Fatal("empty source url must be refused")
	}
	if _, err := Import(context.Background(), Options{SourceURL: "x"}); err == nil {
		t.Fatal("empty dest url must be refused")
	}
}

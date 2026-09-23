package members

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOrgOwnershipIsGlobal(t *testing.T) {
	s := openTest(t)
	if err := s.AddOrg("alice", "acme"); err != nil {
		t.Fatal(err)
	}
	owner, err := s.OrgOwner("acme")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "alice" {
		t.Fatalf("OrgOwner = %q, want alice", owner)
	}
	// A different tenant does NOT own it.
	if ok, _ := s.OwnsOrg("bob", "acme"); ok {
		t.Fatal("bob must not own acme")
	}
	// An unowned org reports no owner.
	if owner, _ := s.OrgOwner("nobody"); owner != "" {
		t.Fatalf("unowned org owner = %q, want empty", owner)
	}
}

func TestRepoOwnership(t *testing.T) {
	s := openTest(t)
	if err := s.AddRepo("alice", "acme", "web"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.OwnsRepo("alice", "acme", "web"); !ok {
		t.Fatal("alice must own acme/web")
	}
	if ok, _ := s.OwnsRepo("bob", "acme", "web"); ok {
		t.Fatal("bob must not own acme/web")
	}
	repos, err := s.ListRepos("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != [2]string{"acme", "web"} {
		t.Fatalf("ListRepos = %v", repos)
	}
}

func TestRemoveRepo(t *testing.T) {
	s := openTest(t)
	if err := s.AddRepo("alice", "acme", "web"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRepo("alice", "acme", "web"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.OwnsRepo("alice", "acme", "web"); ok {
		t.Fatal("acme/web must be gone after RemoveRepo")
	}
	// Idempotent: removing again is fine.
	if err := s.RemoveRepo("alice", "acme", "web"); err != nil {
		t.Fatal(err)
	}
}

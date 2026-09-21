package roles

import (
	"strings"
	"testing"
)

func has(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

func TestRoleForBranch(t *testing.T) {
	if RoleForBranch("main") != Maintainer {
		t.Fatal("main must be maintainer")
	}
	if RoleForBranch("feat/x") != Developer {
		t.Fatal("non-main must be developer")
	}
}

func TestMaintainerCannotWriteMain(t *testing.T) {
	tools := ToolsFor(Maintainer)
	// Maintainer must NOT have any tool that writes git, and no sandbox-port.
	for _, forbidden := range []string{
		"repo-write", "repo-edit", "repo-delete", "repo-commit", "sandbox-port",
	} {
		if has(tools, forbidden) {
			t.Fatalf("maintainer must not have %q", forbidden)
		}
	}
	// But it DOES need read, review/merge, branch+dispatch, sandbox.
	for _, want := range []string{
		"repo-read", "repo-list", "repo-mr-list", "repo-mr-merge",
		"repo-branch-create", "sandbox-exec", "sandbox-create",
	} {
		if !has(tools, want) {
			t.Fatalf("maintainer must have %q", want)
		}
	}
	// Never empty (empty whitelist = all tools).
	if len(tools) == 0 {
		t.Fatal("maintainer tools must not be empty")
	}
}

func TestDeveloperProposesNotMerges(t *testing.T) {
	tools := ToolsFor(Developer)
	if !has(tools, "repo-write") || !has(tools, "repo-mr-create") || !has(tools, "sandbox-port") {
		t.Fatal("developer must write its branch + propose + port")
	}
	if has(tools, "repo-mr-merge") {
		t.Fatal("developer must NOT merge")
	}
}

func TestPlannerAndExplorer(t *testing.T) {
	p := ToolsFor(Planner)
	if !has(p, "sandbox-exec") || has(p, "sandbox-port") || has(p, "repo-write") {
		t.Fatal("planner: sandbox yes, git write no")
	}
	e := ToolsFor(Explorer)
	if has(e, "sandbox-exec") || has(e, "repo-write") {
		t.Fatal("explorer: no sandbox, no write")
	}
	if !has(e, "repo-read") {
		t.Fatal("explorer must read")
	}
}

func TestAdminCreatesRepoNoSandbox(t *testing.T) {
	a := ToolsFor(Admin)
	if !has(a, "repo-create-org") || !has(a, "repo-create-repo") {
		t.Fatal("admin must create org/repo")
	}
	if has(a, "sandbox-exec") || has(a, "repo-write") {
		t.Fatal("admin: no sandbox, no writes")
	}
}

func TestParseSession(t *testing.T) {
	org, repo, branch, ok := ParseSession("acme:web:main")
	if !ok || org != "acme" || repo != "web" || branch != "main" {
		t.Fatalf("bad parse: %q %q %q %v", org, repo, branch, ok)
	}
	for _, bad := range []string{"a:b", "a:b:c:d", "a::c", "a:b:..", "a:b:.lock"} {
		if _, _, _, ok := ParseSession(bad); ok {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestNoToolsNeverEmpty(t *testing.T) {
	for _, r := range []Role{Admin, Maintainer, Developer, Planner, Explorer} {
		if len(ToolsFor(r)) == 0 {
			t.Fatalf("%s tools must not be empty (empty == all)", r)
		}
	}
}

var _ = strings.TrimSpace

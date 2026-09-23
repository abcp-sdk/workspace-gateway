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
		"repo-file-write", "repo-file-edit", "repo-file-delete", "repo-commit", "sandbox-port",
	} {
		if has(tools, forbidden) {
			t.Fatalf("maintainer must not have %q", forbidden)
		}
	}
	// But it DOES need read, review/merge, branch+dispatch, sandbox.
	for _, want := range []string{
		"repo-file-read", "repo-file-list", "repo-mr-list", "repo-mr-merge",
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
	if !has(tools, "repo-file-write") || !has(tools, "repo-mr-create") || !has(tools, "sandbox-port") {
		t.Fatal("developer must write its branch + propose + port")
	}
	if has(tools, "repo-mr-merge") {
		t.Fatal("developer must NOT merge")
	}
}

func TestExplorer(t *testing.T) {
	e := ToolsFor(Explorer)
	// Explorer MAY run a sandbox for analysis...
	if !has(e, "sandbox-exec") || !has(e, "sandbox-checkout") {
		t.Fatal("explorer: sandbox analysis tools missing")
	}
	// ...but has NO path back into a repository.
	if has(e, "sandbox-port") || has(e, "repo-file-write") || has(e, "repo-file-edit") || has(e, "repo-commit") {
		t.Fatal("explorer: must have no repo write path")
	}
	if !has(e, "repo-file-read") {
		t.Fatal("explorer must read")
	}
}

func TestAdminCreatesRepoNoSandbox(t *testing.T) {
	a := ToolsFor(Admin)
	if !has(a, "repo-create-org") || !has(a, "repo-create-repo") {
		t.Fatal("admin must create org/repo")
	}
	// Admin may run long-lived services but has NO sandbox and no repo writes.
	if !has(a, "service-deploy") || !has(a, "service-list") {
		t.Fatal("admin must deploy/list services")
	}
	if !has(a, "oci-import") || !has(a, "repo-import") {
		t.Fatal("admin must import repos and images")
	}
	if !has(a, "repo-set-push-mirror") || !has(a, "repo-list-push-mirrors") || !has(a, "repo-delete-push-mirror") {
		t.Fatal("admin must manage push mirrors")
	}
	if has(a, "sandbox-exec") || has(a, "repo-file-write") {
		t.Fatal("admin: no sandbox, no writes")
	}
}

func TestPreviewAndLogsAvailability(t *testing.T) {
	// developer: may build a preview image + run a preview service + read logs.
	d := ToolsFor(Developer)
	for _, want := range []string{"repo-build-preview", "service-preview", "service-logs"} {
		if !has(d, want) {
			t.Fatalf("developer must have %q", want)
		}
	}
	// developer must NOT deploy a release service.
	if has(d, "service-deploy") {
		t.Fatal("developer must not deploy a release service")
	}
	// explorer: read-only observability.
	e := ToolsFor(Explorer)
	if !has(e, "service-list") || !has(e, "service-logs") {
		t.Fatal("explorer must list services and read logs")
	}
	if has(e, "service-preview") || has(e, "service-deploy") || has(e, "repo-build-preview") {
		t.Fatal("explorer must not deploy or build")
	}
	// maintainer keeps release deploy + logs.
	m := ToolsFor(Maintainer)
	if !has(m, "service-deploy") || !has(m, "service-logs") {
		t.Fatal("maintainer must deploy release services and read logs")
	}
}

func TestOnlyAdminRemovesRepo(t *testing.T) {
	if !has(ToolsFor(Admin), "repo-remove") {
		t.Fatal("admin must remove repos")
	}
	for _, r := range []Role{Maintainer, Developer, Explorer} {
		if has(ToolsFor(r), "repo-remove") {
			t.Fatalf("%s must not remove repos (admin-only)", r)
		}
	}
}

func TestOnlyAdminManagesPushMirrors(t *testing.T) {
	mirrorTools := []string{"repo-set-push-mirror", "repo-list-push-mirrors", "repo-delete-push-mirror"}
	a := ToolsFor(Admin)
	for _, want := range mirrorTools {
		if !has(a, want) {
			t.Fatalf("admin must have %q", want)
		}
	}
	for _, r := range []Role{Maintainer, Developer, Explorer} {
		for _, forbidden := range mirrorTools {
			if has(ToolsFor(r), forbidden) {
				t.Fatalf("%s must not have %q (admin-only)", r, forbidden)
			}
		}
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
	for _, r := range []Role{Admin, Maintainer, Developer, Explorer} {
		if len(ToolsFor(r)) == 0 {
			t.Fatalf("%s tools must not be empty (empty == all)", r)
		}
	}
}

func TestValidServiceName(t *testing.T) {
	good := []string{"app", "my-app", "a1", "web2-3"}
	for _, s := range good {
		if !ValidServiceName(s) {
			t.Fatalf("%q should be a valid service name", s)
		}
	}
	bad := []string{"", "App", "a_b", "a.b", "a/b", "-a", "a-", "a b", strings.Repeat("a", 64)}
	for _, s := range bad {
		if ValidServiceName(s) {
			t.Fatalf("%q should NOT be a valid service name", s)
		}
	}
}

var _ = strings.TrimSpace

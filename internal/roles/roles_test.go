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
	// Every branch session is the SAME developer role (merge rights come from
	// the MR's base, not the session's branch).
	if RoleForBranch("main") != Developer {
		t.Fatal("main must be developer")
	}
	if RoleForBranch("feat/x") != Developer {
		t.Fatal("non-main must be developer")
	}
}

func TestDeveloperEditsOnlyViaSandbox(t *testing.T) {
	tools := ToolsFor(Developer)
	// No tool writes branch content directly, and no tool creates a branch.
	for _, forbidden := range []string{
		"repo-file-write", "repo-file-edit", "repo-file-delete", "repo-commit",
		"sandbox-port", "repo-branch-create", "repo-branch-sync", "repo-file-restore",
		"repo-mr-create",
	} {
		if has(tools, forbidden) {
			t.Fatalf("developer must not have %q", forbidden)
		}
	}
	// It edits in a sandbox and submits/merges/closes MRs.
	for _, want := range []string{
		"repo-file-read", "repo-file-list", "sandbox-checkout", "sandbox-submit-mr",
		"repo-mr-list", "repo-mr-comment", "repo-mr-merge", "repo-mr-close",
		"repo-tag-create", "service-deploy", "sandbox-exec", "sandbox-create",
	} {
		if !has(tools, want) {
			t.Fatalf("developer must have %q", want)
		}
	}
	if len(tools) == 0 {
		t.Fatal("developer tools must not be empty")
	}
}

func TestExplorer(t *testing.T) {
	e := ToolsFor(Explorer)
	// Explorer MAY run a sandbox for analysis...
	if !has(e, "sandbox-exec") || !has(e, "sandbox-checkout") {
		t.Fatal("explorer: sandbox analysis tools missing")
	}
	// ...but has NO path back into a repository.
	if has(e, "sandbox-submit-mr") || has(e, "repo-mr-merge") || has(e, "repo-file-write") {
		t.Fatal("explorer: must have no repo write path")
	}
	if !has(e, "repo-file-read") {
		t.Fatal("explorer must read")
	}
}

func TestAdminCreatesRepoAndHasSandbox(t *testing.T) {
	a := ToolsFor(Admin)
	if !has(a, "repo-create-org") || !has(a, "repo-create-repo") {
		t.Fatal("admin must create org/repo")
	}
	// Admin may run long-lived services and an ad-hoc sandbox.
	if !has(a, "service-deploy") || !has(a, "service-list") {
		t.Fatal("admin must deploy/list services")
	}
	if !has(a, "oci-import") || !has(a, "repo-import") {
		t.Fatal("admin must import repos and images")
	}
	if !has(a, "repo-set-push-mirror") || !has(a, "repo-list-push-mirrors") || !has(a, "repo-delete-push-mirror") {
		t.Fatal("admin must manage push mirrors")
	}
	// Admin now HAS the sandbox tools (create/exec/files)…
	if !has(a, "sandbox-create") || !has(a, "sandbox-exec") || !has(a, "sandbox-file-read") {
		t.Fatal("admin must have sandbox tools")
	}
	// …but still NO repo writes and NO sandbox-port (admin is not branch-bound).
	if has(a, "sandbox-port") || has(a, "repo-file-write") {
		t.Fatal("admin: no repo writes, no sandbox-port")
	}
}

func TestLogsAndServicesAvailability(t *testing.T) {
	// developer: may build an image, deploy a service, and read logs.
	d := ToolsFor(Developer)
	for _, want := range []string{"repo-build-image", "service-logs", "service-deploy"} {
		if !has(d, want) {
			t.Fatalf("developer must have %q", want)
		}
	}
	// explorer: read-only observability.
	e := ToolsFor(Explorer)
	if !has(e, "service-list") || !has(e, "service-logs") {
		t.Fatal("explorer must list services and read logs")
	}
	if has(e, "service-deploy") || has(e, "repo-build-image") {
		t.Fatal("explorer must not deploy or build")
	}
}

func TestOnlyAdminRemovesRepo(t *testing.T) {
	if !has(ToolsFor(Admin), "repo-remove") {
		t.Fatal("admin must remove repos")
	}
	for _, r := range []Role{Developer, Explorer} {
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
	for _, r := range []Role{Developer, Explorer} {
		for _, forbidden := range mirrorTools {
			if has(ToolsFor(r), forbidden) {
				t.Fatalf("%s must not have %q (admin-only)", r, forbidden)
			}
		}
	}
}

func TestEveryRoleListsPVCsButOnlyAdminMutates(t *testing.T) {
	// pvc-list is read-only observability: every role may see the tenant's
	// volumes. pvc-create/pvc-delete stay admin-only.
	for _, r := range []Role{Admin, Developer, Explorer} {
		if !has(ToolsFor(r), "pvc-list") {
			t.Fatalf("%s must have pvc-list", r)
		}
	}
	for _, r := range []Role{Developer, Explorer} {
		for _, forbidden := range []string{"pvc-create", "pvc-delete"} {
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

func TestEverySandboxRoleHasBrowserTools(t *testing.T) {
	// The playwright extension is deployed for every workspace tenant, so every
	// role that owns a sandbox must whitelist the full browser-* family —
	// otherwise the tools are discovered but filtered out and the model has no
	// browser access at all (the bug this guards).
	want := []string{
		"browser-create-context", "browser-close-context", "browser-navigate",
		"browser-snapshot", "browser-click", "browser-type", "browser-fill-form",
		"browser-take-screenshot", "browser-console-messages",
		"browser-network-requests",
	}
	for _, r := range []Role{Admin, Developer, Explorer} {
		tools := ToolsFor(r)
		for _, name := range want {
			if !has(tools, name) {
				t.Fatalf("%s must have %q", r, name)
			}
		}
	}
	// The browser block is the size of the shipped playwright manifest.
	if len(browserTools) != 28 {
		t.Fatalf("browserTools has %d entries; expected 28 (playwright manifest)", len(browserTools))
	}
}

func TestEverySandboxRoleHasComputerTools(t *testing.T) {
	// The workspace extension ships the sandbox-computer-* GUI tools for every
	// workspace tenant. Every role that owns a sandbox must whitelist them, or
	// the tools are discovered but filtered out (no GUI access for the model).
	want := []string{
		"sandbox-computer-apps", "sandbox-computer-snapshot",
		"sandbox-computer-find", "sandbox-computer-action",
		"sandbox-computer-click", "sandbox-computer-type",
		"sandbox-computer-key", "sandbox-computer-scroll",
		"sandbox-computer-drag", "sandbox-computer-screenshot",
	}
	for _, r := range []Role{Admin, Developer, Explorer} {
		tools := ToolsFor(r)
		for _, name := range want {
			if !has(tools, name) {
				t.Fatalf("%s must have %q", r, name)
			}
		}
	}
	if len(computerTools) != 10 {
		t.Fatalf("computerTools has %d entries; expected 10 (workspace-extension manifest)", len(computerTools))
	}
}

func TestNoToolsNeverEmpty(t *testing.T) {
	for _, r := range []Role{Admin, Developer, Explorer} {
		if len(ToolsFor(r)) == 0 {
			t.Fatalf("%s tools must not be empty (empty == all)", r)
		}
	}
}

func TestWhitelistEntriesAreToolOwnNames(t *testing.T) {
	// A preset whitelist entry is the tool's OWN name (matching the agent's
	// `toolQualifiedName`, which uses the name as-is). Guard against an
	// accidental `extId-` prefix creeping back in — every shipped extension
	// namespaces its own tools (`repo-`, `sandbox-`, `browser-`, ...).
	for _, r := range []Role{Admin, Developer, Explorer} {
		for _, name := range ToolsFor(r) {
			if strings.Contains(name, ".") {
				t.Fatalf("%s whitelist entry %q must not contain '.' (sanitized on the wire)", r, name)
			}
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

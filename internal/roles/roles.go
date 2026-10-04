// Package roles maps a session's shape to its immutable role + preset, and
// owns each role's tool whitelist.
//
// A tenant only ever sees the org/repo it maintains, so "visible" is the whole
// tenant. A session's preset is decided at creation and never changes:
//
//	admin                -> create org/repo, read-only, NO sandbox
//	org:repo:<branch>    -> developer (sandbox-only edits, submit/merge MRs)
//	<free> role=explorer -> explorer   (read all visible repos only)
//
// RULE: branch content changes ONLY by merging an MR. No role has a tool that
// writes a branch directly; a developer edits in a sandbox and submits an MR
// (`sandbox-submit-mr`), which materializes onto an immutable `mr/...` branch.
// A developer may MERGE only an MR whose base is its OWN branch (self-merge);
// branch protection (apply_to_admins) is the enforcement backstop.
package roles

import (
	"fmt"
	"regexp"
	"strings"
)

// Role is a session role.
type Role string

const (
	Admin     Role = "admin"
	Developer Role = "developer"
	Explorer  Role = "explorer"
)

// MainBranch is the mandatory default branch name.
const MainBranch = "main"

// PresetFor returns the immutable preset id bound to a role.
func PresetFor(r Role) string { return string(r) }

// ---- tool whitelists ----

// ---- tool id qualification ----
//
// A preset whitelist / DISABLED_TOOLS entry addresses a tool by its STABLE
// qualified id `<extId>-<name>` (both parts sanitized to [A-Za-z0-9_-]; see
// abc-protocol/agent's `toolQualifiedName`). It is ALWAYS qualified — never the
// bare name — so the key never changes when another extension adds a same-named
// tool. The prefixes below are the extension ids deployed for a workspace
// tenant: the workspace extension, the bundled extension and the playwright
// browser extension.
const (
	extBundled    = "bundled-"
	extWorkspace  = "workspace-"
	extPlaywright = "playwright-"
)

// General tools every role keeps (memory / web / history / vision). These are
// the bundled extension's tools.
var generalTools = []string{
	extBundled + "todo-write",
	extBundled + "time-wait",
	extBundled + "history-search",
	extBundled + "history-range",
	extBundled + "web-fetch",
	extBundled + "brave-search",
	extBundled + "file-info",
	extBundled + "image-read",
}

// Sandbox lifecycle + execution tools (the workspace extension).
var sandboxBase = []string{
	extWorkspace + "sandbox-create", extWorkspace + "sandbox-list",
	extWorkspace + "sandbox-status", extWorkspace + "sandbox-delete",
	extWorkspace + "list-oci-images",
	extWorkspace + "sandbox-info", extWorkspace + "sandbox-exec",
	extWorkspace + "sandbox-job-start", extWorkspace + "sandbox-job-output",
	extWorkspace + "sandbox-job-wait",
	extWorkspace + "sandbox-job-kill", extWorkspace + "sandbox-job-stdin",
	extWorkspace + "sandbox-job-list",
	extWorkspace + "sandbox-file-read", extWorkspace + "sandbox-file-patch",
	extWorkspace + "sandbox-file-ls",
	extWorkspace + "sandbox-file-download", extWorkspace + "sandbox-file-upload",
	extWorkspace + "sandbox-checkout",
}

// Browser-automation tools (the playwright extension, deployed for every
// workspace tenant and pointed at the shared Selenium service via the seeded
// `selenium-url` config). These are the full `browser-*` family: a session
// creates a context, drives it, and observes network/console. They are a
// general capability like the sandbox tools, so every role that owns a
// sandbox gets them; without this whitelist the extension's tools are
// discovered but filtered out, leaving the model with no browser access.
var browserTools = []string{
	extPlaywright + "browser-create-context", extPlaywright + "browser-close-context",
	extPlaywright + "browser-navigate", extPlaywright + "browser-navigate-back",
	extPlaywright + "browser-snapshot", extPlaywright + "browser-find",
	extPlaywright + "browser-wait-for", extPlaywright + "browser-resize",
	extPlaywright + "browser-tabs",
	extPlaywright + "browser-click", extPlaywright + "browser-type",
	extPlaywright + "browser-hover",
	extPlaywright + "browser-select-option", extPlaywright + "browser-press-key",
	extPlaywright + "browser-drag",
	extPlaywright + "browser-fill-form", extPlaywright + "browser-handle-dialog",
	extPlaywright + "browser-evaluate",
	extPlaywright + "browser-run-code-unsafe",
	extPlaywright + "browser-console-messages", extPlaywright + "browser-network-requests",
	extPlaywright + "browser-network-request",
	extPlaywright + "browser-take-screenshot", extPlaywright + "browser-pdf-save",
	extPlaywright + "browser-file-upload", extPlaywright + "browser-drop",
	extPlaywright + "browser-storage-state", extPlaywright + "browser-set-storage-state",
}

// Computer-use (GUI) tools (the workspace extension's `sandbox-computer-*`
// family, gated at call time on the sandbox's accessibility tooling). They
// drive a native GUI through the platform accessibility tree — observe
// (`apps`/`snapshot`/`find`), interact (action/click/type/key/scroll/drag) and
// capture (screenshot). Every role that owns a sandbox gets them; without this
// whitelist the extension's tools are discovered but filtered out, leaving the
// model with no GUI access.
var computerTools = []string{
	extWorkspace + "sandbox-computer-apps", extWorkspace + "sandbox-computer-snapshot",
	extWorkspace + "sandbox-computer-find", extWorkspace + "sandbox-computer-action",
	extWorkspace + "sandbox-computer-click", extWorkspace + "sandbox-computer-type",
	extWorkspace + "sandbox-computer-key", extWorkspace + "sandbox-computer-scroll",
	extWorkspace + "sandbox-computer-drag", extWorkspace + "sandbox-computer-screenshot",
}

// Repo read-only browse tools (the workspace extension).
var repoReadTools = []string{
	extWorkspace + "repo-explore", extWorkspace + "repo-file-read",
	extWorkspace + "repo-file-list", extWorkspace + "repo-log",
	extWorkspace + "repo-show",
	extWorkspace + "repo-diff", extWorkspace + "repo-branches", extWorkspace + "repo-tags",
}

// Repo propose + review tools. Every branch session has the SAME set:
//   - edit ONLY in a sandbox, then submit an MR (`sandbox-submit-mr`);
//   - open/comment on MRs, and merge/close an MR whose base is its OWN branch;
//   - tag releases, build images, deploy/manage services + helm.
//
// There is NO tool that writes branch content directly (no repo-file-write/edit/
// commit, no sandbox-port) and NO tool that creates a branch.
var repoDevTools = []string{
	// MR lifecycle (the only content path is sandbox-submit-mr)
	extWorkspace + "sandbox-submit-mr", extWorkspace + "repo-mr-list",
	extWorkspace + "repo-mr-comment", extWorkspace + "repo-mr-merge",
	extWorkspace + "repo-mr-close",
	// peer messaging between branch sessions
	extWorkspace + "repo-mail-send",
	// releases / images / services
	extWorkspace + "repo-tag-create", extWorkspace + "repo-build-image",
	extWorkspace + "repo-build-status",
	extWorkspace + "service-deploy", extWorkspace + "service-list",
	extWorkspace + "service-delete", extWorkspace + "service-logs",
	extWorkspace + "service-promote", extWorkspace + "service-rollback",
	extWorkspace + "helm-deploy", extWorkspace + "helm-list",
	extWorkspace + "helm-history", extWorkspace + "helm-rollback",
	extWorkspace + "helm-uninstall",
	extWorkspace + "helm-promote", extWorkspace + "helm-rollback-release",
}

// Admin tools: create org/repo, import repos AND images, configure push
// mirrors, deploy services, and browse the image catalog (an admin has no
// sandbox but manages the tenant's shared resources).
var adminTools = []string{
	extWorkspace + "repo-create-org", extWorkspace + "repo-create-repo",
	extWorkspace + "repo-import", extWorkspace + "repo-remove",
	extWorkspace + "repo-set-push-mirror", extWorkspace + "repo-list-push-mirrors",
	extWorkspace + "repo-delete-push-mirror",
	extWorkspace + "oci-import",
	extWorkspace + "service-deploy", extWorkspace + "service-list",
	extWorkspace + "service-delete", extWorkspace + "service-logs",
	extWorkspace + "service-promote", extWorkspace + "service-rollback",
	extWorkspace + "pvc-create", extWorkspace + "pvc-list", extWorkspace + "pvc-delete",
	extWorkspace + "helm-deploy", extWorkspace + "helm-list",
	extWorkspace + "helm-history", extWorkspace + "helm-rollback",
	extWorkspace + "helm-uninstall",
	extWorkspace + "helm-promote", extWorkspace + "helm-rollback-release",
}

// ToolsFor returns a role's preset whitelist. The agent treats an EMPTY
// whitelist as "all tools", so every role returns a non-empty list.
func ToolsFor(r Role) []string {
	switch r {
	case Admin:
		// Create org/repo + read + manage tenant resources + run a sandbox for
		// ad-hoc work. Still NO repo writes and NO sandbox-port (admin sessions
		// are not bound to a branch, so porting would write main).
		return concat(generalTools, repoReadTools, adminTools, sandboxBase, browserTools, computerTools)
	case Developer:
		// The single repo-bound role: sandbox-only edits, submit/merge/close
		// MRs (merge/close only when base == its own branch), releases, images,
		// services. NOT sandbox-port and NOT any direct branch write.
		return concat(generalTools, repoReadTools, repoDevTools, sandboxBase, browserTools, computerTools, []string{extWorkspace + "pvc-list"})
	case Explorer:
		// Read every visible repo; may run a sandbox for analysis, but has NO
		// tool that writes back to a repo (no sandbox-port, no submit-mr). May
		// read services + their logs (observability only).
		return concat(generalTools, repoReadTools, sandboxBase, browserTools, computerTools, []string{extWorkspace + "service-list", extWorkspace + "service-logs", extWorkspace + "pvc-list"})
	}
	return concat(generalTools, repoReadTools)
}

func concat(parts ...[]string) []string {
	out := []string{}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// CanCreateRepo reports whether a role may create org/repo.
func CanCreateRepo(r Role) bool { return r == Admin }

// ---- service naming ----

// dnsLabelRe is a DNS-1123 label (RFC 1123): lowercase alphanumerics and '-',
// starting/ending with an alphanumeric, at most 63 chars. Service names must
// satisfy this because a service is exposed publicly as
// `<name>.<ns>.<domain>` — an invalid label breaks DNS/TLS for that host.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidServiceName reports whether s is a legal service name (a DNS-1123 label).
func ValidServiceName(s string) bool { return dnsLabelRe.MatchString(s) }

// ---- session naming ----

var componentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// ValidComponent reports whether s is a legal org/repo/branch component.
func ValidComponent(s string) bool {
	if !componentRe.MatchString(s) {
		return false
	}
	if strings.Contains(s, ":") || strings.Contains(s, "..") {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.Contains(s, "//") {
		return false
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false
	}
	return true
}

// ParseSession splits `org:repo:branch`; ok=false when not exactly three legal
// components.
func ParseSession(name string) (org, repo, branch string, ok bool) {
	parts := strings.Split(name, ":")
	if len(parts) != 3 {
		return "", "", "", false
	}
	for _, p := range parts {
		if !ValidComponent(p) {
			return "", "", "", false
		}
	}
	return parts[0], parts[1], parts[2], true
}

// SessionName builds `org:repo:branch`.
func SessionName(org, repo, branch string) string {
	return fmt.Sprintf("%s:%s:%s", org, repo, branch)
}

// RoleForBranch derives the role from a branch session. Every branch session
// (main included) is the SAME developer role; merge rights are enforced by the
// MR's base, not by the session's branch.
func RoleForBranch(branch string) Role {
	_ = branch
	return Developer
}

// ValidRole reports whether s is a known role id.
func ValidRole(s string) bool {
	switch Role(s) {
	case Admin, Developer, Explorer:
		return true
	}
	return false
}

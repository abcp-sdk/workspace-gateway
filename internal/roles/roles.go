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
//
// Entries are the tool's OWN name (self-namespaced by its first segment:
// `repo-*`, `sandbox-*`, `browser-*`, ...), matching the agent's AI/LLM tool
// key. The agent derives that key from the tool name via `toolQualifiedName`
// (see abc-protocol/agent): the name is used as-is, with only out-of-charset
// characters sanitized to `-` (the LLM gateway's `tools.N.function.name`
// charset is `[A-Za-z0-9_-]`, so the bundled `model.image` becomes
// `model-image`). An `extId-` prefix is deliberately NOT used.

// General tools every role keeps (memory / web / history / vision).
var generalTools = []string{
	"todo-write",
	"time-wait",
	"history-search",
	"history-range",
	"web-fetch",
	"brave-search",
	"file-info",
	"image-read",
}

// Sandbox lifecycle + execution tools.
var sandboxBase = []string{
	"sandbox-create", "sandbox-list", "sandbox-status", "sandbox-delete",
	"list-oci-images",
	"sandbox-info", "sandbox-exec",
	"sandbox-job-start", "sandbox-job-output", "sandbox-job-wait",
	"sandbox-job-kill", "sandbox-job-stdin", "sandbox-job-list",
	"sandbox-file-read", "sandbox-file-patch", "sandbox-file-ls",
	"sandbox-file-download", "sandbox-file-upload", "sandbox-checkout",
}

// Browser-automation tools (the playwright extension, deployed for every
// workspace tenant and pointed at the shared Selenium service via the seeded
// `selenium-url` config). These are the full `browser-*` family: a session
// creates a context, drives it, and observes network/console. They are a
// general capability like the sandbox tools, so every role that owns a
// sandbox gets them; without this whitelist the extension's tools are
// discovered but filtered out, leaving the model with no browser access.
var browserTools = []string{
	"browser-create-context", "browser-close-context",
	"browser-navigate", "browser-navigate-back",
	"browser-snapshot", "browser-find", "browser-wait-for", "browser-resize",
	"browser-tabs",
	"browser-click", "browser-type", "browser-hover",
	"browser-select-option", "browser-press-key", "browser-drag",
	"browser-fill-form", "browser-handle-dialog", "browser-evaluate",
	"browser-run-code-unsafe",
	"browser-console-messages", "browser-network-requests",
	"browser-network-request",
	"browser-take-screenshot", "browser-pdf-save",
	"browser-file-upload", "browser-drop",
	"browser-storage-state", "browser-set-storage-state",
}

// Computer-use (GUI) tools (the workspace extension's `sandbox-computer-*`
// family, gated at call time on the sandbox's accessibility tooling). They
// drive a native GUI through the platform accessibility tree — observe
// (`apps`/`snapshot`/`find`), interact (action/click/type/key/scroll/drag) and
// capture (screenshot). Every role that owns a sandbox gets them; without this
// whitelist the extension's tools are discovered but filtered out, leaving the
// model with no GUI access.
var computerTools = []string{
	"sandbox-computer-apps", "sandbox-computer-snapshot",
	"sandbox-computer-find", "sandbox-computer-action",
	"sandbox-computer-click", "sandbox-computer-type",
	"sandbox-computer-key", "sandbox-computer-scroll",
	"sandbox-computer-drag", "sandbox-computer-screenshot",
}

// Repo read-only browse tools.
var repoReadTools = []string{
	"repo-explore", "repo-file-read", "repo-file-list", "repo-log", "repo-show",
	"repo-diff", "repo-branches", "repo-tags",
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
	"sandbox-submit-mr", "repo-mr-list", "repo-mr-comment", "repo-mr-merge", "repo-mr-close",
	// peer messaging between branch sessions
	"repo-mail-send",
	// releases / images / services
	"repo-tag-create", "repo-build-image", "repo-build-status",
	"service-deploy", "service-list", "service-delete", "service-logs",
	"service-rollback",
	"helm-deploy", "helm-list", "helm-history", "helm-rollback", "helm-uninstall",
	"helm-objects", "helm-object-logs",
}

// Admin tools: create org/repo, import repos AND images, configure push
// mirrors, deploy services, and browse the image catalog (an admin has no
// sandbox but manages the tenant's shared resources).
var adminTools = []string{
	"repo-create-org", "repo-create-repo", "repo-import", "repo-remove",
	"repo-set-push-mirror", "repo-list-push-mirrors", "repo-delete-push-mirror",
	"oci-import",
	"service-deploy", "service-list", "service-delete", "service-logs",
	"service-rollback",
	"pvc-create", "pvc-list", "pvc-delete",
	"helm-deploy", "helm-list", "helm-history", "helm-rollback", "helm-uninstall",
	"helm-objects", "helm-object-logs",
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
		return concat(generalTools, repoReadTools, repoDevTools, sandboxBase, browserTools, computerTools, []string{"pvc-create", "pvc-list", "pvc-delete"})
	case Explorer:
		// Read every visible repo; may run a sandbox for analysis, but has NO
		// tool that writes back to a repo (no sandbox-port, no submit-mr). May
		// read services + their logs (observability only).
		return concat(generalTools, repoReadTools, sandboxBase, browserTools, computerTools, []string{"service-list", "service-logs", "pvc-create", "pvc-list", "pvc-delete"})
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

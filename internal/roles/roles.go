// Package roles maps a session's shape to its immutable role + preset, and
// owns each role's tool whitelist.
//
// A tenant only ever sees the org/repo it maintains, so "visible ==
// maintainer". A session's preset is decided at creation and never changes:
//
//	admin                -> create org/repo, read-only, NO sandbox
//	org:repo:main        -> maintainer (read, review/merge MRs, branch+dispatch, sandbox)
//	org:repo:<other>     -> developer  (write its branch, propose MRs, sandbox)
//	<free> role=planner  -> planner    (read all visible repos + sandbox)
//	<free> role=explorer -> explorer   (read all visible repos only)
//
// RULE: the `main` branch can ONLY change by merging an MR. A maintainer
// session therefore has NO tool that writes to main (no repo-write/edit/commit,
// no sandbox-port); it reviews and merges change requests and creates branch
// sessions. Branch protection (apply_to_admins) is the enforcement backstop.
package roles

import (
	"fmt"
	"regexp"
	"strings"
)

// Role is a session role.
type Role string

const (
	Admin      Role = "admin"
	Maintainer Role = "maintainer"
	Developer  Role = "developer"
	Planner    Role = "planner"
	Explorer   Role = "explorer"
)

// MainBranch is the mandatory default branch name.
const MainBranch = "main"

// PresetFor returns the immutable preset id bound to a role.
func PresetFor(r Role) string { return string(r) }

// ---- tool whitelists ----

// General tools every role keeps (memory / web / history / vision).
var generalTools = []string{
	"todo-write",
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
	"sandbox-read", "sandbox-write", "sandbox-edit", "sandbox-ls",
	"sandbox-download", "sandbox-upload", "sandbox-checkout",
}

// Repo read-only browse tools.
var repoReadTools = []string{
	"repo-explore", "repo-read", "repo-list", "repo-log", "repo-show",
	"repo-diff", "repo-branches", "repo-tags",
}

// Repo propose tools: write to a NON-main branch and open/comment MRs.
var repoProposeTools = []string{
	"repo-write", "repo-edit", "repo-delete", "repo-commit",
	"repo-branch-create", "repo-mr-create", "repo-mr-list", "repo-mr-comment",
	"repo-mail-send",
}

// Repo review tools: the maintainer reviews and merges MRs, creates the
// branches it dispatches work to, tags releases, builds sandbox images from
// the repo, and deploys long-lived services. It does NOT open MRs.
var repoReviewTools = []string{
	"repo-branch-create", "repo-tag-create", "repo-build-image",
	"repo-mr-list", "repo-mr-comment", "repo-mr-merge",
	"repo-mail-send",
	"service-deploy", "service-list", "service-delete",
}

// Admin tools: create org/repo only.
var adminTools = []string{"repo-create-org", "repo-create-repo"}

// ToolsFor returns a role's preset whitelist. The agent treats an EMPTY
// whitelist as "all tools", so every role returns a non-empty list.
func ToolsFor(r Role) []string {
	switch r {
	case Admin:
		// Create org/repo + read; no sandbox, no writes.
		return concat(generalTools, repoReadTools, adminTools)
	case Maintainer:
		// Read, review/merge MRs, create+dispatch branches, sandbox. NOT
		// sandbox-port (would write main) and NOT repo-write/edit/commit.
		return concat(generalTools, repoReadTools, repoReviewTools, sandboxBase)
	case Developer:
		// Work on its branch (incl. sandbox-port), propose MRs, sandbox.
		return concat(generalTools, repoReadTools, repoProposeTools, sandboxBase, []string{"sandbox-port"})
	case Planner:
		// Read every visible repo + experiment in a sandbox (no git write).
		return concat(generalTools, repoReadTools, sandboxBase)
	case Explorer:
		// Read every visible repo only; no sandbox, no writes.
		return concat(generalTools, repoReadTools)
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

// CanMerge reports whether a role may approve/merge MRs.
func CanMerge(r Role) bool { return r == Maintainer }

// CanCreateRepo reports whether a role may create org/repo.
func CanCreateRepo(r Role) bool { return r == Admin }

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

// RoleForBranch derives the role from the branch name (main = maintainer).
func RoleForBranch(branch string) Role {
	if branch == MainBranch {
		return Maintainer
	}
	return Developer
}

// ValidRole reports whether s is a known role id.
func ValidRole(s string) bool {
	switch Role(s) {
	case Admin, Maintainer, Developer, Planner, Explorer:
		return true
	}
	return false
}

// Package gitmerge integrates a base branch (`main`) into a feature branch by
// committing a two-parent merge on the feature branch. Files changed on BOTH
// sides since the merge base are merged three-way; genuine conflicts are left
// in the tree as marker blocks that a session must resolve.
//
// Why markers: a three-way merge cannot invent intent, so the only correct
// arbiter is the branch's own LLM session. Committing the `base`/`ours`/
// `theirs` versions into the branch gives that session everything it needs,
// and a unique sentinel makes "still unresolved" trivially detectable — the
// CreateMR/MergeMR gates refuse a branch whose changed files still carry it.
//
// go-git provides no three-way merge (Repository.Merge is fast-forward only),
// so the merge itself comes from github.com/epiclabs-io/diff3 and the marker
// rendering is ours (which is what lets us include the base section and a
// custom sentinel).
package gitmerge

import (
	"bytes"
	"strings"
)

// Sentinel marks our conflict blocks. It is deliberately unusual so a
// repo-scan for it is a reliable "unresolved conflict" signal.
const Sentinel = "ABCP-CONFLICT"

// markers for the three sections. They are git-style so editors and diffs
// render them naturally, with the sentinel embedded in the anchor lines.
const (
	oursOpen   = "<<<<<<< " + Sentinel + " (ours)"
	baseOpen   = "||||||| " + Sentinel + " (base)"
	splitLine  = "======="
	theirsOpen = ">>>>>>> " + Sentinel + " (theirs)"
	// deletedSide marks a modify/delete conflict where one side removed the
	// file; the removed side's content is rendered as this placeholder.
	deletedSide = Sentinel + " (deleted)"
)

// HasMarkers reports whether b carries our unresolved-conflict sentinel. It is
// the single gate predicate shared by CreateMR and MergeMR.
func HasMarkers(b []byte) bool {
	return bytes.Contains(b, []byte(Sentinel))
}

// conflictBlock renders one unresolved conflict as a marker block. `ours`,
// `base` and `theirs` already carry trailing newlines as needed; empty strings
// render as the deleted placeholder line.
func conflictBlock(ours, base, theirs string) string {
	var sb strings.Builder
	sb.WriteString(oursOpen)
	sb.WriteByte('\n')
	writeSection(&sb, ours)
	sb.WriteString(baseOpen)
	sb.WriteByte('\n')
	writeSection(&sb, base)
	sb.WriteString(splitLine)
	sb.WriteByte('\n')
	writeSection(&sb, theirs)
	sb.WriteString(theirsOpen)
	sb.WriteByte('\n')
	return sb.String()
}

// deletedConflict renders a modify/delete conflict: the side that DELETED the
// file is rendered as the placeholder (empty section), the surviving side keeps
// its content. `oursDeleted` says which side removed the file.
func deletedConflict(ours, base, theirs string, oursDeleted bool) string {
	if oursDeleted {
		return conflictBlock("", base, theirs)
	}
	return conflictBlock(ours, base, "")
}

func writeSection(sb *strings.Builder, s string) {
	if s == "" {
		sb.WriteString(deletedSide)
		sb.WriteByte('\n')
		return
	}
	sb.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		sb.WriteByte('\n')
	}
}

package gitmerge

import (
	"strings"

	"github.com/epiclabs-io/diff3"
)

// textMerge three-way merges `ours` and `theirs` against `base`, returning the
// merged content and whether any marker block was written. It is a thin
// adapter over diff3 that renders OUR marker format (base section + sentinel).
//
// ExcludeFalseConflicts collapses the "both sides changed the region to the
// same text" case, which needs no decision. Remaining conflicts carry the full
// base/ours/theirs triple, which is exactly what a resolving session needs.
func textMerge(base, ours, theirs string) (string, bool) {
	o := splitLines(base)
	a := splitLines(ours)
	b := splitLines(theirs)

	res := diff3.Diff3Merge(a, o, b, true)

	var sb strings.Builder
	conflicted := false
	for _, item := range res {
		if item.Ok != nil {
			sb.WriteString(strings.Join(item.Ok, ""))
			continue
		}
		c := item.Conflict
		conflicted = true
		sb.WriteString(conflictBlock(strings.Join(c.A, ""), strings.Join(c.O, ""), strings.Join(c.B, "")))
	}
	return sb.String(), conflicted
}

// splitLines splits into lines, each EXCEPT the last carrying its trailing
// newline (diff3 compares line-tokens; keeping terminators makes the rejoin
// lossless).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(s, "\n")
}

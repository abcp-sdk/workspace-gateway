package gitmerge

import "bytes"

// isBinary reports whether content looks binary. We treat any NUL byte in the
// first 8000 bytes as the signal (the same heuristic git uses).
func isBinary(b []byte) bool {
	const sniff = 8000
	n := len(b)
	if n > sniff {
		n = sniff
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// mergeResult is the outcome for one path after applying the conflict policy.
type mergeResult struct {
	content  []byte // the merged/selected bytes
	conflict bool   // true when the caller must resolve markers
	omit     bool   // true when the merged result is a deletion
}

// same reports whether two sides hold identical state (presence + bytes).
func same(a, b pathContent) bool {
	if a.present != b.present {
		return false
	}
	if !a.present {
		return true
	}
	return bytes.Equal(a.data, b.data)
}

// resolvePath applies the path-level conflict policy to a single path. `base`,
// `ours` and `theirs` are the path's state in the merge base, the feature
// branch and the base branch respectively.
//
//	unchanged on one side -> take the other side (no conflict)
//	both modified         -> three-way text merge (binary: take theirs)
//	add/add               -> three-way text merge (base is empty)
//	modify/delete         -> marker block with the deleted side marked
//	both deleted          -> removed
func resolvePath(base, ours, theirs pathContent) (mergeResult, error) {
	// Unchanged on OURS means the branch never touched it: take main verbatim.
	if same(base, ours) {
		return single(theirs), nil
	}
	// Unchanged on THEIRS means main never touched it: take the branch.
	if same(base, theirs) {
		return single(ours), nil
	}

	// Both sides deleted it.
	if !ours.present && !theirs.present {
		return mergeResult{omit: true}, nil
	}

	// Modify/delete (either direction): markers, with the deleted side empty.
	if !ours.present || !theirs.present {
		oursDeleted := !ours.present
		survivor := ours.data
		if oursDeleted {
			survivor = theirs.data
		}
		if isBinary(survivor) || isBinary(base.data) {
			// Cannot render markers into binary: keep the survivor.
			return mergeResult{content: survivor}, nil
		}
		return mergeResult{
			content:  []byte(deletedConflict(string(ours.data), string(base.data), string(theirs.data), oursDeleted)),
			conflict: true,
		}, nil
	}

	// Both sides have content and each changed it. Binary -> take theirs.
	if isBinary(base.data) || isBinary(ours.data) || isBinary(theirs.data) {
		return mergeResult{content: theirs.data}, nil
	}

	merged, conflicted := textMerge(string(base.data), string(ours.data), string(theirs.data))
	return mergeResult{content: []byte(merged), conflict: conflicted}, nil
}

// single converts a one-sided state into a merge result.
func single(s pathContent) mergeResult {
	if !s.present {
		return mergeResult{omit: true}
	}
	return mergeResult{content: s.data}
}

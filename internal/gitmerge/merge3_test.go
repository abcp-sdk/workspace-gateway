package gitmerge

import (
	"strings"
	"testing"
)

func TestHasMarkers(t *testing.T) {
	if HasMarkers([]byte("plain content\n")) {
		t.Fatal("plain content wrongly flagged as conflicted")
	}
	if !HasMarkers([]byte("a\n" + oursOpen + "\nb\n")) {
		t.Fatal("marker block not detected")
	}
}

func TestConflictBlockHasThreeSections(t *testing.T) {
	block := conflictBlock("ours\n", "base\n", "theirs\n")
	for _, want := range []string{oursOpen, baseOpen, splitLine, theirsOpen, "ours", "base", "theirs"} {
		if !strings.Contains(block, want) {
			t.Fatalf("block missing %q:\n%s", want, block)
		}
	}
}

func TestTextMergeClean(t *testing.T) {
	// Two disjoint edits (with a common line between) must merge cleanly.
	// NOTE: adjacent edits DO conflict — git does the same (verified), because
	// the hunks overlap. That is correct, not a bug.
	base := "a\nb\nc\nd\ne\n"
	ours := "a\nB\nc\nd\ne\n"
	theirs := "a\nb\nc\nd\nE\n"
	merged, conflicted := textMerge(base, ours, theirs)
	if conflicted {
		t.Fatalf("unexpected conflict:\n%s", merged)
	}
	if !strings.Contains(merged, "B") || !strings.Contains(merged, "E") {
		t.Fatalf("both edits must survive:\n%s", merged)
	}
	if HasMarkers([]byte(merged)) {
		t.Fatal("clean merge carries markers")
	}
}

func TestTextMergeConflict(t *testing.T) {
	// Overlapping edits to the same line conflict.
	base := "a\nb\nc\n"
	ours := "a\nOURS\nc\n"
	theirs := "a\nTHEIRS\nc\n"
	merged, conflicted := textMerge(base, ours, theirs)
	if !conflicted {
		t.Fatal("overlapping edits must conflict")
	}
	if !HasMarkers([]byte(merged)) {
		t.Fatalf("conflict must carry the sentinel:\n%s", merged)
	}
	if !strings.Contains(merged, "OURS") || !strings.Contains(merged, "THEIRS") || !strings.Contains(merged, "b") {
		t.Fatalf("conflict must expose base/ours/theirs:\n%s", merged)
	}
}

func TestTextMergeSameChangeIsNotConflict(t *testing.T) {
	// Both sides made the SAME edit: no decision needed.
	base := "a\nb\n"
	ours := "a\nX\n"
	theirs := "a\nX\n"
	merged, conflicted := textMerge(base, ours, theirs)
	if conflicted {
		t.Fatalf("identical change must not conflict:\n%s", merged)
	}
	if strings.Count(merged, "X") != 1 {
		t.Fatalf("expected a single X:\n%s", merged)
	}
}

func TestResolvePathOneSideUntouched(t *testing.T) {
	base := pathContent{present: true, data: []byte("v1\n")}
	ours := pathContent{present: true, data: []byte("v1\n")}   // unchanged
	theirs := pathContent{present: true, data: []byte("v2\n")} // changed
	res, err := resolvePath(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if res.conflict || res.omit {
		t.Fatalf("one-sided change must be taken silently: %+v", res)
	}
	if string(res.content) != "v2\n" {
		t.Fatalf("content = %q, want v2", res.content)
	}
}

func TestResolvePathAddAdd(t *testing.T) {
	base := pathContent{} // absent
	ours := pathContent{present: true, data: []byte("a\nb\n")}
	theirs := pathContent{present: true, data: []byte("a\nc\n")}
	res, err := resolvePath(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if !res.conflict {
		t.Fatal("add/add with differing content must conflict")
	}
}

func TestResolvePathModifyDelete(t *testing.T) {
	base := pathContent{present: true, data: []byte("a\nb\n")}
	ours := pathContent{present: true, data: []byte("a\nB\n")} // modified
	theirs := pathContent{}                                    // deleted in main
	res, err := resolvePath(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if !res.conflict {
		t.Fatal("modify/delete must conflict")
	}
	if !HasMarkers(res.content) {
		t.Fatal("modify/delete must carry markers")
	}
	if !strings.Contains(string(res.content), deletedSide) {
		t.Fatalf("deleted side must be marked:\n%s", res.content)
	}
}

func TestResolvePathBinaryTakesTheirs(t *testing.T) {
	base := pathContent{present: true, data: []byte{0x00, 0x01, 0x02}}
	ours := pathContent{present: true, data: []byte{0x00, 0xaa, 0xbb}}
	theirs := pathContent{present: true, data: []byte{0x00, 0xcc, 0xdd}}
	res, err := resolvePath(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if res.conflict {
		t.Fatal("binary conflict must not produce markers")
	}
	if string(res.content) != string(theirs.data) {
		t.Fatal("binary conflict must take theirs")
	}
}

func TestResolvePathBothDeleted(t *testing.T) {
	base := pathContent{present: true, data: []byte("x")}
	res, err := resolvePath(base, pathContent{}, pathContent{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.omit {
		t.Fatal("both-deleted must omit the path")
	}
}

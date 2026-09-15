package reviewunits

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildPartitionsFilesInDiffOrderAndKeepsPrimaryCoverageUnique(t *testing.T) {
	diff := []byte("diff --git a/one.txt b/one.txt\n--- a/one.txt\n+++ b/one.txt\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/two.txt b/two.txt\n--- a/two.txt\n+++ b/two.txt\n@@ -1 +1 @@\n-old-two\n+new-two\n")

	manifest, err := Build(diff, "run-1", strings.Repeat("a", 40), strings.Repeat("b", 40), Policy{MaxUnitBytes: 10_000, ContextLines: 10})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(manifest.Units) != 1 {
		t.Fatalf("Build() units = %d, want one packed unit", len(manifest.Units))
	}
	evidence, err := UnitDiff(diff, manifest.Units[0])
	if err != nil {
		t.Fatalf("UnitDiff() error = %v", err)
	}
	if !bytes.Equal(evidence, diff) {
		t.Fatalf("packed unit evidence = %q, want complete diff %q", evidence, diff)
	}
	if len(manifest.Units[0].PrimaryRanges) != 4 {
		t.Fatalf("primary ranges = %d, want one range per changed line", len(manifest.Units[0].PrimaryRanges))
	}
	if manifest.Units[0].ID != "unit-001" {
		t.Fatalf("unit id = %q, want stable first ordinal", manifest.Units[0].ID)
	}
	if err := VerifyPrimaryCoverage(manifest); err != nil {
		t.Fatalf("VerifyPrimaryCoverage() error = %v", err)
	}
}

// TestBuildSplitsAnOversizedFileAtHunkBoundaries verifies that later hunk
// fragments retain only the file header and their own hunk, never duplicating
// an earlier hunk as review evidence.
func TestBuildSplitsAnOversizedFileAtHunkBoundaries(t *testing.T) {
	diff := []byte("diff --git a/multi.txt b/multi.txt\n--- a/multi.txt\n+++ b/multi.txt\n@@ -1 +1 @@\n-old-one\n+new-one\n@@ -20 +20 @@\n-old-two\n+new-two\n")
	manifest, err := Build(diff, "run-1", strings.Repeat("a", 40), strings.Repeat("b", 40), Policy{MaxUnitBytes: 105, ContextLines: 0})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(manifest.Units) != 2 {
		t.Fatalf("units = %d, want one unit per oversized-file hunk", len(manifest.Units))
	}
	for index, unit := range manifest.Units {
		if unit.ID != unitID(index+1) {
			t.Errorf("unit %d id = %q, want %q", index+1, unit.ID, unitID(index+1))
		}
		evidence, evidenceErr := UnitDiff(diff, unit)
		if evidenceErr != nil {
			t.Fatalf("UnitDiff(%s) error = %v", unit.ID, evidenceErr)
		}
		if index == 1 && bytes.Contains(evidence, []byte("new-one")) {
			t.Errorf("later hunk evidence repeats earlier hunk: %q", evidence)
		}
	}
	if err := VerifyPrimaryCoverage(manifest); err != nil {
		t.Fatalf("VerifyPrimaryCoverage() error = %v", err)
	}
}

func TestBuildSplitsOversizedHunkIntoDeterministicWindows(t *testing.T) {
	diff := []byte("diff --git a/big.txt b/big.txt\n--- a/big.txt\n+++ b/big.txt\n@@ -1,8 +1,8 @@\n one\n-two\n+TWO\n three\n-four\n+FOUR\n five\n-six\n+SIX\n seven\n")
	policy := Policy{MaxUnitBytes: 115, ContextLines: 1}
	first, err := Build(diff, "run-1", strings.Repeat("a", 40), strings.Repeat("b", 40), policy)
	if err != nil {
		t.Fatalf("first Build() error = %v", err)
	}
	second, err := Build(diff, "run-1", strings.Repeat("a", 40), strings.Repeat("b", 40), policy)
	if err != nil {
		t.Fatalf("second Build() error = %v", err)
	}
	if len(first.Units) < 2 {
		t.Fatalf("units = %d, want oversized hunk split", len(first.Units))
	}
	if first.ManifestSHA256 != second.ManifestSHA256 {
		t.Fatalf("manifest hashes differ: %q and %q", first.ManifestSHA256, second.ManifestSHA256)
	}
	for _, unit := range first.Units {
		if unit.WorkloadBytes > policy.MaxUnitBytes {
			t.Errorf("unit %s workload = %d, exceeds %d", unit.ID, unit.WorkloadBytes, policy.MaxUnitBytes)
		}
	}
	if err := VerifyPrimaryCoverage(first); err != nil {
		t.Fatalf("VerifyPrimaryCoverage() error = %v", err)
	}
}

func TestBuildRejectsAnUnpartitionableSmallestFragment(t *testing.T) {
	diff := []byte("diff --git a/binary.bin b/binary.bin\nnew file mode 100644\nindex 0000000..1111111\nBinary files /dev/null and b/binary.bin differ\n")
	if _, err := Build(diff, "run-1", strings.Repeat("a", 40), strings.Repeat("b", 40), Policy{MaxUnitBytes: 8}); err == nil {
		t.Fatal("Build() error = nil, want unpartitionable fragment error")
	}
}

// TestBuildAssignsANonTextChangeToExactlyOneUnit verifies that a renamed file
// whose body is split across several units announces its non-text change once,
// as ADR 0012 requires for renames, mode changes, and binary summaries.
func TestBuildAssignsANonTextChangeToExactlyOneUnit(t *testing.T) {
	diff := []byte("diff --git a/old.txt b/new.txt\nsimilarity index 80%\nrename from old.txt\nrename to new.txt\n--- a/old.txt\n+++ b/new.txt\n@@ -1,8 +1,8 @@\n one\n-two\n+TWO\n three\n-four\n+FOUR\n five\n-six\n+SIX\n seven\n")
	manifest, err := Build(diff, "run-1", strings.Repeat("a", 40), strings.Repeat("b", 40), Policy{MaxUnitBytes: 150, ContextLines: 1})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(manifest.Units) < 2 {
		t.Fatalf("units = %d, want the renamed file split across units", len(manifest.Units))
	}
	owners := make([]string, 0, len(manifest.Units))
	for _, unit := range manifest.Units {
		for _, path := range unit.PrimaryNonTextFiles {
			owners = append(owners, unit.ID+"/"+path)
		}
	}
	if len(owners) != 1 || owners[0] != "unit-001/new.txt" {
		t.Fatalf("non-text owners = %v, want only unit-001/new.txt", owners)
	}
	if err := VerifyPrimaryCoverage(manifest); err != nil {
		t.Fatalf("VerifyPrimaryCoverage() error = %v", err)
	}
}

// TestBuildOwnsABinarySummaryWithoutPrimaryRanges verifies that a change with
// no changed source line is still owned, by path rather than by range.
func TestBuildOwnsABinarySummaryWithoutPrimaryRanges(t *testing.T) {
	diff := []byte("diff --git a/logo.png b/logo.png\nindex 1111111..2222222 100644\nBinary files a/logo.png and b/logo.png differ\n" +
		"diff --git a/plain.txt b/plain.txt\n--- a/plain.txt\n+++ b/plain.txt\n@@ -1 +1 @@\n-old\n+new\n")
	manifest, err := Build(diff, "run-1", strings.Repeat("a", 40), strings.Repeat("b", 40), Policy{MaxUnitBytes: 10_000, ContextLines: 10})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	owned := make([]string, 0)
	for _, unit := range manifest.Units {
		owned = append(owned, unit.PrimaryNonTextFiles...)
	}
	if len(owned) != 1 || owned[0] != "logo.png" {
		t.Fatalf("non-text files = %v, want only the binary summary", owned)
	}
	for _, unit := range manifest.Units {
		for _, primary := range unit.PrimaryRanges {
			if primary.Path == "logo.png" {
				t.Fatalf("binary summary claimed a primary source range: %+v", primary)
			}
		}
	}
	if err := VerifyPrimaryCoverage(manifest); err != nil {
		t.Fatalf("VerifyPrimaryCoverage() error = %v", err)
	}
}

// TestVerifyPrimaryCoverageRejectsADuplicatedNonTextFile guards the invariant
// directly, so a hand-edited or migrated manifest cannot claim one non-text
// change in two units.
func TestVerifyPrimaryCoverageRejectsADuplicatedNonTextFile(t *testing.T) {
	manifest := Manifest{Units: []Unit{
		{ID: unitID(1), Ordinal: 1, PrimaryNonTextFiles: []string{"moved.txt"}},
		{ID: unitID(2), Ordinal: 2, PrimaryNonTextFiles: []string{"moved.txt"}},
	}}
	if err := VerifyPrimaryCoverage(manifest); err == nil {
		t.Fatal("VerifyPrimaryCoverage() error = nil, want a duplicated non-text assignment error")
	}
}

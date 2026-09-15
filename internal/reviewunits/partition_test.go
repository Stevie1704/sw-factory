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

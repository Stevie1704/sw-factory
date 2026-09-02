package worker_test

import (
	"os"
	"strings"
	"testing"
)

// TestImplementationSkillKeepsTDDAndReviewOwnershipSeparated verifies the
// implementation workflow delegates tests conditionally without reclaiming
// either factory review axis.
func TestImplementationSkillKeepsTDDAndReviewOwnershipSeparated(t *testing.T) {
	body := readSkill(t, "skills/implement/SKILL.md")
	for _, marker := range []string{"`tdd` skill", "separate specification", "standards roles own review"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("implementation skill missing %q:\n%s", marker, body)
		}
	}
	for _, marker := range []string{"code-review", "parallel sub-agents", "Commit your work"} {
		if strings.Contains(body, marker) {
			t.Fatalf("implementation skill retained combined review behavior %q:\n%s", marker, body)
		}
	}
}

// TestReviewSkillsContainOnlyTheirAssignedAxis verifies the upstream combined
// review guidance is split before it reaches the two independent review roles.
func TestReviewSkillsContainOnlyTheirAssignedAxis(t *testing.T) {
	specification := readSkill(t, "skills/specification-review/SKILL.md")
	for _, marker := range []string{"missing or only partially implemented", "behavior that was not requested", "implemented incorrectly"} {
		if !strings.Contains(specification, marker) {
			t.Fatalf("specification-review skill missing %q:\n%s", marker, specification)
		}
	}
	if strings.Contains(specification, "Fowler") || strings.Contains(specification, "parallel sub-agents") {
		t.Fatalf("specification-review skill retained standards or orchestration guidance:\n%s", specification)
	}

	standards := readSkill(t, "skills/standards-review/SKILL.md")
	for _, marker := range []string{"Repository-documented standards override", "Fowler code smells", "Speculative Generality"} {
		if !strings.Contains(standards, marker) {
			t.Fatalf("standards-review skill missing %q:\n%s", marker, standards)
		}
	}
	if strings.Contains(standards, "missing or only partially implemented") || strings.Contains(standards, "parallel sub-agents") {
		t.Fatalf("standards-review skill retained specification or orchestration guidance:\n%s", standards)
	}
}

// readSkill returns one checked-in worker skill body for a contract test.
func readSkill(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(body)
}

package worker_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
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

// TestRoleSkillsLeaveCoordinatorProtocolToThePrompt verifies Docker-baked
// craft does not absorb the factory-owned result-file completion contract.
func TestRoleSkillsLeaveCoordinatorProtocolToThePrompt(t *testing.T) {
	for _, path := range []string{
		"skills/implement/SKILL.md",
		"skills/specification-review/SKILL.md",
		"skills/standards-review/SKILL.md",
	} {
		t.Run(path, func(t *testing.T) {
			body := readSkill(t, path)
			for _, protocol := range []string{"/usr/local/bin/factory-report", "The coordinator advances only from that file"} {
				if strings.Contains(body, protocol) {
					t.Fatalf("role skill contains factory-owned protocol %q:\n%s", protocol, body)
				}
			}
		})
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

// harnessSkillRoots maps each harness shipped in the worker image to the role
// home directory that harness reads the worker skill set from.
var harnessSkillRoots = map[string]string{
	"claude": "/home/factory/.claude/skills",
	"codex":  "/home/factory/.codex/skills",
}

// Each harness has its own switch that withholds a shipped skill from the
// model-visible catalog. A role-mandated skill must carry neither.
const (
	// claudeHiddenSwitch hides a skill through its SKILL.md front matter.
	claudeHiddenSwitch = "disable-model-invocation"
	// codexHiddenSwitch hides a skill through its Codex activation policy.
	codexHiddenSwitch = "allow_implicit_invocation"
)

// TestMandatorySkillsAreModelVisibleUnderEveryHarness verifies no shipped
// harness hides a role-mandated skill from the model-visible catalog. Codex
// hides a skill whose openai.yaml denies implicit invocation; Claude hides one
// whose SKILL.md front matter disables model invocation.
func TestMandatorySkillsAreModelVisibleUnderEveryHarness(t *testing.T) {
	for _, skill := range harness.MandatorySkills() {
		t.Run(skill, func(t *testing.T) {
			body := readSkill(t, "skills/"+skill+"/SKILL.md")
			if !strings.Contains(body, "name: "+skill+"\n") {
				t.Fatalf("skill %q does not declare its own name:\n%s", skill, body)
			}
			if strings.Contains(body, claudeHiddenSwitch) {
				t.Fatalf("skill %q is hidden from the Claude model-visible catalog:\n%s", skill, body)
			}
			metadata := readSkill(t, "skills/"+skill+"/agents/openai.yaml")
			if strings.Contains(metadata, codexHiddenSwitch) {
				t.Fatalf("skill %q is hidden from the Codex model-visible catalog:\n%s", skill, metadata)
			}
		})
	}
}

// TestWorkerImageInstallsOneSkillSetPerHarnessRoot verifies the image
// definition seeds exactly one skill set per harness discovery root. A second
// root holding the same skill names would let a harness resolve one name to
// two installations.
func TestWorkerImageInstallsOneSkillSetPerHarnessRoot(t *testing.T) {
	definition := readSkill(t, "Dockerfile")
	for harnessName, root := range harnessSkillRoots {
		if got := strings.Count(definition, " "+root+"\n"); got != 1 {
			t.Fatalf("%s skill root %q installed %d times, want once:\n%s", harnessName, root, got, definition)
		}
	}
	if strings.Contains(definition, ".agents/skills") {
		t.Fatalf("worker image installs a duplicate skill set under an additional discovery root:\n%s", definition)
	}
}

// TestRolePromptsOnlyMandateVisibleWorkerSkills verifies every skill an
// embedded role prompt names belongs to the worker skill set and is advertised
// by both harnesses. It is the artifact-level statement of the role contract:
// a prompt never instructs an agent to use a skill the harness withheld.
func TestRolePromptsOnlyMandateVisibleWorkerSkills(t *testing.T) {
	set := workerSkillSet(t)
	prompts, err := filepath.Glob(filepath.Join("..", "internal", "prompt", "prompts", "*.md"))
	if err != nil {
		t.Fatalf("Glob(prompts) error = %v", err)
	}
	if len(prompts) == 0 {
		t.Fatal("no embedded role prompts were found")
	}
	for _, path := range prompts {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		for _, named := range promptSkillNames(string(body)) {
			if _, ok := set[named]; !ok {
				t.Fatalf("role prompt %q names skill %q, which the worker skill set does not contain", path, named)
			}
			if hiddenFromAHarness(t, named) {
				t.Fatalf("role prompt %q names skill %q, which a shipped harness hides from the model", path, named)
			}
		}
	}
}

// promptSkillNames returns the bare backticked names a prompt uses on a line
// that speaks about skills. Prompt bodies name a skill only in that context,
// so the surrounding line keeps unrelated backticked paths out of the result.
func promptSkillNames(body string) []string {
	pattern := regexp.MustCompile("`([a-z][a-z0-9-]{2,})`")
	var names []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "skill") {
			continue
		}
		for _, match := range pattern.FindAllStringSubmatch(line, -1) {
			names = append(names, match[1])
		}
	}
	return names
}

// workerSkillSet returns the skill names the worker image ships.
func workerSkillSet(t *testing.T) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir("skills")
	if err != nil {
		t.Fatalf("ReadDir(skills) error = %v", err)
	}
	set := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			set[entry.Name()] = struct{}{}
		}
	}
	return set
}

// hiddenFromAHarness reports whether either shipped harness would omit one
// worker skill from its model-visible catalog.
func hiddenFromAHarness(t *testing.T, skill string) bool {
	t.Helper()
	if strings.Contains(readSkill(t, "skills/"+skill+"/SKILL.md"), claudeHiddenSwitch) {
		return true
	}
	metadata, err := os.ReadFile(filepath.Join("skills", skill, "agents", "openai.yaml"))
	if err != nil {
		return false
	}
	return strings.Contains(string(metadata), codexHiddenSwitch)
}

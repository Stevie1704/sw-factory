package harness_test

import (
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
)

// TestRequiredSkillsAddThePRSkillOnlyForAConfiguredWriter verifies startup
// diagnosis demands the `pr` skill only from the harness that runs the
// optional pr_writer role, and from every harness for an unavailable policy.
func TestRequiredSkillsAddThePRSkillOnlyForAConfiguredWriter(t *testing.T) {
	withoutWriter := &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex}}
	if got := harness.RequiredSkills(withoutWriter, harness.NameCodex); !slices.Equal(got, harness.MandatorySkills()) {
		t.Fatalf("RequiredSkills(without pr_writer) = %v, want %v", got, harness.MandatorySkills())
	}
	withWriter := &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"pr_writer": config.HarnessClaude}}
	if got := harness.RequiredSkills(withWriter, harness.NameClaude); !slices.Contains(got, harness.PullRequestWriterSkill) {
		t.Fatalf("RequiredSkills(with pr_writer, claude) = %v, want %q", got, harness.PullRequestWriterSkill)
	}
	if got := harness.RequiredSkills(withWriter, harness.NameCodex); slices.Contains(got, harness.PullRequestWriterSkill) {
		t.Fatalf("RequiredSkills(with pr_writer on claude, codex) = %v, want no %q", got, harness.PullRequestWriterSkill)
	}
	if got := harness.RequiredSkills(nil, harness.NameCodex); !slices.Contains(got, harness.PullRequestWriterSkill) {
		t.Fatalf("RequiredSkills(nil) = %v, want every role skill", got)
	}
}

// TestRequiredSkillsAddTheGrillingSkillOnlyForAConfiguredIdeationRole
// verifies that existing repositories without the optional ideation role
// keep their required skill set, and that the harness that runs ideation
// must prove the `grilling` skill.
func TestRequiredSkillsAddTheGrillingSkillOnlyForAConfiguredIdeationRole(t *testing.T) {
	existing := &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex}}
	if got := harness.RequiredSkills(existing, harness.NameCodex); slices.Contains(got, harness.GrillingSkill) {
		t.Fatalf("RequiredSkills(without ideation) = %v, want no %q", got, harness.GrillingSkill)
	}
	ideation := &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"ideation": config.HarnessClaude}}
	if got := harness.RequiredSkills(ideation, harness.NameClaude); !slices.Contains(got, harness.GrillingSkill) {
		t.Fatalf("RequiredSkills(with ideation, claude) = %v, want %q", got, harness.GrillingSkill)
	}
	if got := harness.RequiredSkills(ideation, harness.NameCodex); slices.Contains(got, harness.GrillingSkill) {
		t.Fatalf("RequiredSkills(with ideation on claude, codex) = %v, want no %q", got, harness.GrillingSkill)
	}
	if got := harness.RequiredSkills(nil, harness.NameCodex); !slices.Contains(got, harness.GrillingSkill) {
		t.Fatalf("RequiredSkills(nil) = %v, want every role skill", got)
	}
}

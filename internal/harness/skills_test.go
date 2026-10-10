package harness_test

import (
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
)

// TestRequiredSkillsAddThePRSkillOnlyForAConfiguredWriter verifies startup
// diagnosis demands the `pr` skill only from a repository that can run the
// optional pr_writer role, and from an unavailable policy.
func TestRequiredSkillsAddThePRSkillOnlyForAConfiguredWriter(t *testing.T) {
	withoutWriter := &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex}}
	if got := harness.RequiredSkills(withoutWriter); !slices.Equal(got, harness.MandatorySkills()) {
		t.Fatalf("RequiredSkills(without pr_writer) = %v, want %v", got, harness.MandatorySkills())
	}
	withWriter := &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"pr_writer": config.HarnessClaude}}
	if got := harness.RequiredSkills(withWriter); !slices.Contains(got, harness.PullRequestWriterSkill) {
		t.Fatalf("RequiredSkills(with pr_writer) = %v, want %q", got, harness.PullRequestWriterSkill)
	}
	if got := harness.RequiredSkills(nil); !slices.Contains(got, harness.PullRequestWriterSkill) {
		t.Fatalf("RequiredSkills(nil) = %v, want every role skill", got)
	}
}

package config_test

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// TestValidateRepositoryAcceptsTheOptionalPullRequestWriter verifies the
// pr_writer role is accepted in every per-role policy map.
func TestValidateRepositoryAcceptsTheOptionalPullRequestWriter(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["pr_writer"] = config.HarnessClaude
	policy.ModelOptions["pr_writer"] = []string{"claude-sonnet-5-5"}
	if policy.ReasoningEffortOptions == nil {
		policy.ReasoningEffortOptions = map[string][]string{}
	}
	policy.ReasoningEffortOptions["pr_writer"] = []string{"medium"}
	if err := config.ValidateRepository(policy); err != nil {
		t.Fatalf("ValidateRepository() error = %v, want pr_writer accepted", err)
	}
}

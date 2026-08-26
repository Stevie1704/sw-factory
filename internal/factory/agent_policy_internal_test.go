package factory

import (
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// policyRepository returns a frozen repository policy that selects a different
// harness, model, and reasoning effort for each role.
func policyRepository() config.RepositoryConfig {
	return config.RepositoryConfig{
		RoleHarnessDefaults: map[string]config.Harness{
			"test":           config.HarnessCodex,
			"implementation": config.HarnessClaude,
		},
		ModelOptions: map[string][]string{
			"test":           {"gpt-5.6-luna"},
			"implementation": {"claude-opus-5", "claude-sonnet-5"},
		},
		ReasoningEffortOptions: map[string][]string{
			"test": {"medium", "high"},
		},
	}
}

// TestResolveAgentPolicySelectsEachRoleIndependently verifies harness, model,
// and reasoning effort come from the frozen per-role policy.
func TestResolveAgentPolicySelectsEachRoleIndependently(t *testing.T) {
	repository := policyRepository()
	harnessName, model, effort, err := resolveAgentPolicy(repository, AgentRequest{Role: "test"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy(test) error = %v", err)
	}
	if harnessName != config.HarnessCodex || model != "gpt-5.6-luna" || effort != "medium" {
		t.Fatalf("test role policy = %q/%q/%q, want the declared Codex defaults", harnessName, model, effort)
	}
	harnessName, model, effort, err = resolveAgentPolicy(repository, AgentRequest{Role: "implementation"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy(implementation) error = %v", err)
	}
	if harnessName != config.HarnessClaude || model != "claude-opus-5" || effort != "" {
		t.Fatalf("implementation role policy = %q/%q/%q, want the declared Claude defaults", harnessName, model, effort)
	}
}

// TestResolveAgentPolicyAcceptsADeclaredSelection verifies a request may choose
// any repository-declared option without an override permission.
func TestResolveAgentPolicyAcceptsADeclaredSelection(t *testing.T) {
	_, model, _, err := resolveAgentPolicy(policyRepository(), AgentRequest{Role: "implementation", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy() error = %v", err)
	}
	if model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want the declared alternative", model)
	}
	_, _, effort, err := resolveAgentPolicy(policyRepository(), AgentRequest{Role: "test", ReasoningEffort: "high"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy() error = %v", err)
	}
	if effort != "high" {
		t.Fatalf("reasoning effort = %q, want the declared alternative", effort)
	}
}

// TestResolveAgentPolicyRefusesOverridesOutsidePolicy verifies every
// out-of-policy selection is refused as a typed policy rejection rather than
// an untyped error.
func TestResolveAgentPolicyRefusesOverridesOutsidePolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		request AgentRequest
		code    PolicyRejectionCode
	}{
		{
			name:    "harness",
			request: AgentRequest{Role: "test", Harness: config.HarnessClaude},
			code:    PolicyRejectionHarnessOverride,
		},
		{
			name:    "model",
			request: AgentRequest{Role: "test", Model: "gpt-9"},
			code:    PolicyRejectionModelOverride,
		},
		{
			name:    "reasoning effort",
			request: AgentRequest{Role: "test", ReasoningEffort: "extreme"},
			code:    PolicyRejectionReasoningEffortOverride,
		},
		{
			name:    "undeclared reasoning effort",
			request: AgentRequest{Role: "implementation", ReasoningEffort: "high"},
			code:    PolicyRejectionReasoningEffortOverride,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := resolveAgentPolicy(policyRepository(), test.request)
			var rejection *PolicyRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("resolveAgentPolicy() error = %v, want a typed policy rejection", err)
			}
			if rejection.Code != test.code {
				t.Fatalf("rejection code = %q, want %q", rejection.Code, test.code)
			}
		})
	}
}

// TestResolveAgentPolicyHonoursDeclaredOverridePermissions verifies a
// repository can widen each setting explicitly.
func TestResolveAgentPolicyHonoursDeclaredOverridePermissions(t *testing.T) {
	repository := policyRepository()
	repository.AllowedOverrides = []config.OverrideName{config.OverrideHarness, config.OverrideModel, config.OverrideReasoningEffort}
	harnessName, model, effort, err := resolveAgentPolicy(repository, AgentRequest{
		Role:            "test",
		Harness:         config.HarnessClaude,
		Model:           "gpt-9",
		ReasoningEffort: "extreme",
	})
	if err != nil {
		t.Fatalf("resolveAgentPolicy() error = %v", err)
	}
	if harnessName != config.HarnessClaude || model != "gpt-9" || effort != "extreme" {
		t.Fatalf("policy = %q/%q/%q, want the explicitly permitted overrides", harnessName, model, effort)
	}
}

// TestResolveAgentPolicyRefusesAnUnsupportedHarness verifies an unknown
// harness name never reaches an adapter.
func TestResolveAgentPolicyRefusesAnUnsupportedHarness(t *testing.T) {
	repository := policyRepository()
	repository.RoleHarnessDefaults["test"] = config.Harness("gemini")
	_, _, _, err := resolveAgentPolicy(repository, AgentRequest{Role: "test"})
	var rejection *PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != PolicyRejectionHarnessUnavailable {
		t.Fatalf("resolveAgentPolicy() error = %v, want a typed harness-unavailable rejection", err)
	}
}

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
	policy, err := resolveAgentPolicy(repository, AgentRequest{Role: "test"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy(test) error = %v", err)
	}
	if policy != (AgentPolicy{Harness: config.HarnessCodex, Model: "gpt-5.6-luna", ReasoningEffort: "medium"}) {
		t.Fatalf("test role policy = %#v, want the declared Codex defaults", policy)
	}
	policy, err = resolveAgentPolicy(repository, AgentRequest{Role: "implementation"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy(implementation) error = %v", err)
	}
	if policy != (AgentPolicy{Harness: config.HarnessClaude, Model: "claude-opus-5"}) {
		t.Fatalf("implementation role policy = %#v, want the declared Claude defaults", policy)
	}
}

// TestResolveAgentPolicyAcceptsADeclaredSelection verifies a request may choose
// any repository-declared option without an override permission.
func TestResolveAgentPolicyAcceptsADeclaredSelection(t *testing.T) {
	policy, err := resolveAgentPolicy(policyRepository(), AgentRequest{Role: "implementation", Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy() error = %v", err)
	}
	if policy.Model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want the declared alternative", policy.Model)
	}
	policy, err = resolveAgentPolicy(policyRepository(), AgentRequest{Role: "test", ReasoningEffort: "high"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy() error = %v", err)
	}
	if policy.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q, want the declared alternative", policy.ReasoningEffort)
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
			_, err := resolveAgentPolicy(policyRepository(), test.request)
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
	policy, err := resolveAgentPolicy(repository, AgentRequest{
		Role:            "test",
		Harness:         config.HarnessClaude,
		Model:           "gpt-9",
		ReasoningEffort: "max",
	})
	if err != nil {
		t.Fatalf("resolveAgentPolicy() error = %v", err)
	}
	if policy != (AgentPolicy{Harness: config.HarnessClaude, Model: "gpt-9", ReasoningEffort: "max"}) {
		t.Fatalf("policy = %#v, want the explicitly permitted overrides", policy)
	}
}

// TestResolveAgentPolicyRefusesAnUnsupportedHarness verifies an unknown
// harness name never reaches an adapter.
func TestResolveAgentPolicyRefusesAnUnsupportedHarness(t *testing.T) {
	repository := policyRepository()
	repository.RoleHarnessDefaults["test"] = config.Harness("gemini")
	_, err := resolveAgentPolicy(repository, AgentRequest{Role: "test"})
	var rejection *PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != PolicyRejectionHarnessUnavailable {
		t.Fatalf("resolveAgentPolicy() error = %v, want a typed harness-unavailable rejection", err)
	}
}

// TestResolveAgentPolicySelectsCopilotForAMixedRepository verifies Copilot is
// an ordinary per-role selection next to Codex and Claude.
func TestResolveAgentPolicySelectsCopilotForAMixedRepository(t *testing.T) {
	repository := policyRepository()
	repository.RoleHarnessDefaults["review"] = config.HarnessCopilot
	repository.ModelOptions["review"] = []string{"gpt-5.2"}
	repository.ReasoningEffortOptions["review"] = []string{"minimal"}
	policy, err := resolveAgentPolicy(repository, AgentRequest{Role: "review"})
	if err != nil {
		t.Fatalf("resolveAgentPolicy() error = %v", err)
	}
	if policy != (AgentPolicy{Harness: config.HarnessCopilot, Model: "gpt-5.2", ReasoningEffort: "minimal"}) {
		t.Fatalf("policy = %#v, want the declared Copilot defaults", policy)
	}
}

// TestResolveAgentPolicyKeepsALegacyHarnessOverrideAwayFromCopilot verifies an
// existing allowed_overrides: [harness] does not widen to Copilot until the
// repository lists it in override_harnesses.
func TestResolveAgentPolicyKeepsALegacyHarnessOverrideAwayFromCopilot(t *testing.T) {
	legacy := policyRepository()
	legacy.AllowedOverrides = []config.OverrideName{config.OverrideHarness}
	_, err := resolveAgentPolicy(legacy, AgentRequest{Role: "test", Harness: config.HarnessCopilot})
	var rejection *PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != PolicyRejectionHarnessOverride {
		t.Fatalf("legacy override to Copilot error = %v, want harness_override rejection", err)
	}
	if _, err := resolveAgentPolicy(legacy, AgentRequest{Role: "test", Harness: config.HarnessClaude, ReasoningEffort: "high"}); err != nil {
		t.Fatalf("legacy override to Claude error = %v, want the original authorization kept", err)
	}

	optedIn := legacy
	optedIn.OverrideHarnesses = []config.Harness{config.HarnessCopilot}
	policy, err := resolveAgentPolicy(optedIn, AgentRequest{Role: "test", Harness: config.HarnessCopilot})
	if err != nil || policy.Harness != config.HarnessCopilot {
		t.Fatalf("opted-in override = %#v, %v, want Copilot", policy, err)
	}
}

// TestResolveAgentPolicyRefusesSelectionsTheHarnessCannotHonour verifies a
// model or effort that the final harness would refuse or replace is refused
// before launch, even when an override permits the value.
func TestResolveAgentPolicyRefusesSelectionsTheHarnessCannotHonour(t *testing.T) {
	repository := policyRepository()
	repository.AllowedOverrides = []config.OverrideName{config.OverrideHarness, config.OverrideModel, config.OverrideReasoningEffort}
	repository.OverrideHarnesses = []config.Harness{config.HarnessClaude, config.HarnessCopilot}
	for _, test := range []struct {
		name    string
		request AgentRequest
	}{
		{name: "copilot automatic model", request: AgentRequest{Role: "test", Harness: config.HarnessCopilot, Model: "auto"}},
		{name: "copilot unknown effort", request: AgentRequest{Role: "test", Harness: config.HarnessCopilot, ReasoningEffort: "extreme"}},
		{name: "claude minimal effort", request: AgentRequest{Role: "test", Harness: config.HarnessClaude, ReasoningEffort: "minimal"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolveAgentPolicy(repository, test.request)
			var rejection *PolicyRejection
			if !errors.As(err, &rejection) || rejection.Code != PolicyRejectionSelectionUnsupported {
				t.Fatalf("resolveAgentPolicy() error = %v, want %q", err, PolicyRejectionSelectionUnsupported)
			}
		})
	}
}

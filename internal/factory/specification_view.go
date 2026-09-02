package factory

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/prompt"
)

// promptRunParameters are the frozen decisions a role acts on directly. The
// remaining repository configuration - caches, worker build, harness and model
// policy, budgets - belongs to the coordinator and the worker runtime, so it
// stays out of the prompt.
type promptRunParameters struct {
	TargetBranch            string            `json:"target_branch,omitempty"`
	Route                   string            `json:"route,omitempty"`
	TestPolicyMode          string            `json:"test_policy_mode,omitempty"`
	Gates                   []promptRunGate   `json:"gates,omitempty"`
	RepositoryGuidancePaths []string          `json:"repository_guidance_paths,omitempty"`
	TestPolicyPaths         *promptTestPolicy `json:"test_policy_paths,omitempty"`
	SetupCommand            string            `json:"setup_command,omitempty"`
}

// promptRunGate is one deterministic gate the role's checkpoint must satisfy.
type promptRunGate struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// promptTestPolicy names the frozen repository prefixes that decide test
// ownership, and is present only when the repository declares them.
type promptTestPolicy struct {
	TestPaths           []string `json:"test_paths,omitempty"`
	InfrastructurePaths []string `json:"infrastructure_paths,omitempty"`
}

// promptSpecification renders the frozen packet as the product intent one role
// must satisfy. The complete packet stays mounted read-only beside the
// invocation, so the prompt carries the issue, the accepted clarifications, and
// the frozen run parameters rather than a second copy of the fenced repository
// guidance and of host-only configuration.
func promptSpecification(packet SpecificationPacket) (string, error) {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("## Issue %d: %s\n\n", packet.Issue.Number, strings.TrimSpace(packet.Issue.Title)))
	if body := strings.TrimSpace(packet.Issue.Body); body != "" {
		builder.WriteString(body)
		builder.WriteString("\n\n")
	}
	if len(packet.Clarifications) > 0 {
		builder.WriteString("Accepted clarifications (coordinator-owned):\n")
		for _, clarification := range packet.Clarifications {
			builder.WriteString(fmt.Sprintf("- %s\n  %s\n", strings.TrimSpace(clarification.Question), strings.TrimSpace(clarification.Answer)))
		}
		builder.WriteString("\n")
	}
	parameters, err := marshalPromptJSON(promptRunParametersFor(packet))
	if err != nil {
		return "", fmt.Errorf("encode frozen run parameters: %w", err)
	}
	builder.WriteString("Frozen run parameters (coordinator-owned):\n")
	builder.WriteString(parameters)
	builder.WriteString("\n\nThe complete frozen packet is mounted read-only at ")
	builder.WriteString(prompt.WorkerSpecificationPath)
	builder.WriteString(".")
	return builder.String(), nil
}

// promptRunParametersFor projects the frozen repository configuration onto the
// decisions a role acts on.
func promptRunParametersFor(packet SpecificationPacket) promptRunParameters {
	repositoryConfig := packet.RepositoryConfig
	parameters := promptRunParameters{
		TargetBranch:            repositoryConfig.TargetBranch,
		Route:                   string(packet.Route),
		TestPolicyMode:          string(repositoryConfig.TestPolicy.Mode),
		SetupCommand:            repositoryConfig.Setup,
		RepositoryGuidancePaths: packet.RepositoryGuidancePaths,
	}
	for _, gate := range repositoryConfig.Gates {
		parameters.Gates = append(parameters.Gates, promptRunGate{Name: gate.Name, Command: gate.Command})
	}
	if paths := promptTestPolicyFor(repositoryConfig.TestPolicy); paths != nil {
		parameters.TestPolicyPaths = paths
	}
	return parameters
}

// promptTestPolicyFor returns the declared test-ownership prefixes, or nil when
// the repository declares none.
func promptTestPolicyFor(policy config.TestPolicy) *promptTestPolicy {
	if len(policy.TestPaths) == 0 && len(policy.InfrastructurePaths) == 0 {
		return nil
	}
	return &promptTestPolicy{TestPaths: policy.TestPaths, InfrastructurePaths: policy.InfrastructurePaths}
}

// marshalPromptJSON encodes prompt data readably: indented, and without Go's
// default HTML escaping so a command or a path reaches the role as the
// characters it was written with.
func marshalPromptJSON(value any) (string, error) {
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimRight(builder.String(), "\n"), nil
}

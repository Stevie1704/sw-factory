package factory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// normalizeAgentRequest fills the path default shared by automatic role
// selection without choosing a role before the active run is loaded.
func normalizeAgentRequest(request AgentRequest) AgentRequest {
	return request
}

// validateAgentRequest rejects roles and stages outside the factory-owned
// registry before external side effects occur.
func validateAgentRequest(request AgentRequest) error {
	registry := workflow.DefaultRegistry()
	if request.Role != "" {
		if _, exists := registry.Role(request.Role); !exists {
			return &PolicyRejection{Code: PolicyRejectionRoleUnavailable, Problem: fmt.Sprintf("agent role %q is not declared by the factory-owned workflow registry", request.Role)}
		}
	}
	if request.Stage != "" {
		if _, exists := registry.RoleForInvocationStage(request.Stage); !exists {
			return &PolicyRejection{Code: PolicyRejectionStageUnavailable, Problem: fmt.Sprintf("agent stage %q is not declared by the factory-owned workflow registry", request.Stage)}
		}
	}
	if request.Role != "" && request.Stage != "" {
		definition, _ := registry.Role(request.Role)
		if definition.Stage != request.Stage {
			return &PolicyRejection{Code: PolicyRejectionRoleStageMismatch, Problem: fmt.Sprintf("agent role %q belongs to stage %q, not %q", request.Role, definition.Stage, request.Stage)}
		}
	}
	if request.Model != "" && strings.ContainsAny(request.Model, "\x00\r\n ") {
		return errors.New("agent model contains unsafe characters")
	}
	if request.ReasoningEffort != "" && strings.ContainsAny(request.ReasoningEffort, "\x00\r\n ") {
		return errors.New("agent reasoning effort contains unsafe characters")
	}
	for harnessName, path := range map[string]string{"Codex": request.CodexAuthPath, "Claude": request.ClaudeAuthPath} {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("agent %s auth path must be absolute and free of control characters", harnessName)
		}
	}
	return nil
}

// AgentPolicy is the frozen repository selection resolved for one role. The
// three settings are chosen independently, so they travel together rather than
// as interchangeable bare strings.
type AgentPolicy struct {
	// Harness is the adapter that will run the invocation.
	Harness config.Harness
	// Model is the validated model selection.
	Model string
	// ReasoningEffort is the validated reasoning-effort selection. It is empty
	// when the role declares none.
	ReasoningEffort string
}

// resolveAgentPolicy applies the frozen repository harness, model, and
// reasoning-effort policy for one role. Each setting defaults to the role's
// declared option, and only a repository-declared override permission widens
// it. Every refusal is a typed policy rejection so the coordinator can report
// it in stable vocabulary.
func resolveAgentPolicy(repository config.RepositoryConfig, request AgentRequest) (AgentPolicy, error) {
	harnessName, err := resolveHarnessPolicy(repository, request)
	if err != nil {
		return AgentPolicy{}, err
	}
	model, err := resolveModelPolicy(repository, request)
	if err != nil {
		return AgentPolicy{}, err
	}
	effort, err := resolveReasoningEffortPolicy(repository, request)
	if err != nil {
		return AgentPolicy{}, err
	}
	return AgentPolicy{Harness: harnessName, Model: model, ReasoningEffort: effort}, nil
}

// resolveHarnessPolicy selects the role's harness and refuses an override the
// repository has not explicitly permitted.
func resolveHarnessPolicy(repository config.RepositoryConfig, request AgentRequest) (config.Harness, error) {
	declared := repository.RoleHarnessDefaults[request.Role]
	selected := request.Harness
	if selected == "" {
		selected = declared
	}
	if selected != config.HarnessCodex && selected != config.HarnessClaude {
		return "", &PolicyRejection{
			Code:    PolicyRejectionHarnessUnavailable,
			Problem: fmt.Sprintf("no supported harness policy for role %q", request.Role),
		}
	}
	if request.Harness != "" && request.Harness != declared && !hasOverride(repository.AllowedOverrides, config.OverrideHarness) {
		return "", &PolicyRejection{
			Code:    PolicyRejectionHarnessOverride,
			Problem: fmt.Sprintf("repository configuration does not allow harness override from %q to %q for role %q", declared, request.Harness, request.Role),
		}
	}
	return selected, nil
}

// resolveModelPolicy selects the role's model and refuses a value outside its
// declared options unless the repository permits model overrides.
func resolveModelPolicy(repository config.RepositoryConfig, request AgentRequest) (string, error) {
	options := repository.ModelOptions[request.Role]
	selected := request.Model
	if selected == "" && len(options) > 0 {
		selected = options[0]
	}
	if selected == "" {
		return "", &PolicyRejection{
			Code:    PolicyRejectionModelUnavailable,
			Problem: fmt.Sprintf("no model policy for role %q", request.Role),
		}
	}
	if !contains(options, selected) && !hasOverride(repository.AllowedOverrides, config.OverrideModel) {
		return "", &PolicyRejection{
			Code:    PolicyRejectionModelOverride,
			Problem: fmt.Sprintf("model %q is not a declared option for role %q", selected, request.Role),
		}
	}
	return selected, nil
}

// resolveReasoningEffortPolicy selects the role's reasoning effort. A role that
// declares no options accepts no selection unless the repository permits
// reasoning-effort overrides.
func resolveReasoningEffortPolicy(repository config.RepositoryConfig, request AgentRequest) (string, error) {
	options := repository.ReasoningEffortOptions[request.Role]
	selected := request.ReasoningEffort
	if selected == "" && len(options) > 0 {
		selected = options[0]
	}
	if selected != "" && !contains(options, selected) && !hasOverride(repository.AllowedOverrides, config.OverrideReasoningEffort) {
		return "", &PolicyRejection{
			Code:    PolicyRejectionReasoningEffortOverride,
			Problem: fmt.Sprintf("reasoning effort %q is not a declared option for role %q", selected, request.Role),
		}
	}
	return selected, nil
}

// hasOverride reports whether a repository policy explicitly permits a setting.
func hasOverride(values []config.OverrideName, wanted config.OverrideName) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// contains reports whether a string appears in a policy list.
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// sameStrings compares policy lists in their coordinator-preserved order.
func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// validateAgentRunState enforces the visible role's legal predecessor stages
// before it starts or resumes a harness session.
func validateAgentRunState(run store.Run) error {
	if run.Status != store.StatusActive {
		return fmt.Errorf("cannot start visible agent from run status %q", run.Status)
	}
	registry := workflow.DefaultRegistry()
	if _, exists := registry.RoleForRunStage(run.Stage); !exists {
		if _, invocationStage := registry.RoleForInvocationStage(run.Stage); invocationStage {
			return nil
		}
		return fmt.Errorf("cannot start visible agent from run stage %q", run.Stage)
	}
	return nil
}

// selectAgentRole resolves an automatic request against the frozen run stage.
func selectAgentRole(run store.Run, request AgentRequest) (AgentRequest, error) {
	registry := workflow.DefaultRegistry()
	if request.Role == "" && request.Stage == "" {
		definition, exists := registry.RoleForRunStage(run.Stage)
		if !exists {
			return AgentRequest{}, fmt.Errorf("no factory-owned agent role is declared for run stage %q", run.Stage)
		}
		request.Role = definition.Name
		request.Stage = definition.Stage
	}
	if request.Role == "" {
		definition, exists := registry.RoleForInvocationStage(request.Stage)
		if !exists {
			return AgentRequest{}, fmt.Errorf("no factory-owned role owns invocation stage %q", request.Stage)
		}
		request.Role = definition.Name
	}
	if request.Stage == "" {
		definition, exists := registry.Role(request.Role)
		if !exists {
			return AgentRequest{}, &PolicyRejection{Code: PolicyRejectionRoleUnavailable, Problem: fmt.Sprintf("agent role %q is not declared by the factory-owned workflow registry", request.Role)}
		}
		request.Stage = definition.Stage
	}
	definition, exists := registry.Role(request.Role)
	if !exists {
		return AgentRequest{}, &PolicyRejection{Code: PolicyRejectionRoleUnavailable, Problem: fmt.Sprintf("agent role %q is not declared by the factory-owned workflow registry", request.Role)}
	}
	if definition.Stage != request.Stage {
		return AgentRequest{}, &PolicyRejection{Code: PolicyRejectionRoleStageMismatch, Problem: fmt.Sprintf("agent role %q belongs to stage %q, not %q", request.Role, definition.Stage, request.Stage)}
	}
	if !registry.CanStartFrom(request.Role, run.Stage) {
		return AgentRequest{}, fmt.Errorf("agent role %q cannot start from active run stage %q", request.Role, run.Stage)
	}
	return request, nil
}

// writeInvocationPacket atomically writes the worker-readable packet and leaves
// credentials outside its contents. The packet carries the frozen claim
// unchanged, so a repository-declared host path such as a cache location stays
// in it; the coordinator resolves those paths and the role prompt renders a
// projection that omits them.
func writeInvocationPacket(directory string, packet InvocationPacket) error {
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return fmt.Errorf("encode invocation packet: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".packet-*.tmp")
	if err != nil {
		return fmt.Errorf("create invocation packet: %w", err)
	}
	path := temporary.Name()
	defer func() { _ = os.Remove(path) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect invocation packet: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write invocation packet: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync invocation packet: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close invocation packet: %w", err)
	}
	if err := os.Rename(path, filepath.Join(directory, invocationPacketFileName)); err != nil {
		return fmt.Errorf("publish invocation packet: %w", err)
	}
	return nil
}

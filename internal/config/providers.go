package config

import (
	"regexp"
	"strings"
	"unicode"
)

// Provider selects a factory-owned tracker or code-host adapter.
type Provider string

const (
	// ProviderGitHub serves GitHub Issues or GitHub pull requests.
	ProviderGitHub Provider = "github"
	// ProviderAzureDevOps serves Boards work items or Azure Repos pull requests.
	ProviderAzureDevOps Provider = "azure_devops"
	// ProviderLocal serves accepted Markdown issues, never pull requests.
	ProviderLocal Provider = "local"
)

// LocalOperatorConfig is host-only authority provenance. Neither issue files
// nor imported publication metadata may populate or impersonate it.
type LocalOperatorConfig struct {
	// InstallationID is an explicitly registered opaque installation identity.
	InstallationID string `yaml:"installation_id"`
	// Name identifies the operator of this protected loopback surface.
	Name string `yaml:"name"`
}

// EffectiveTracker preserves omitted legacy combined-provider selections.
func (r RepositoryRegistration) EffectiveTracker() Provider {
	if r.Tracker != "" {
		return r.Tracker
	}
	if !r.AzureDevOps.IsZero() {
		return ProviderAzureDevOps
	}
	return ProviderGitHub
}

// EffectiveCodeHost selects PR/status access independently of the tracker.
func (r RepositoryRegistration) EffectiveCodeHost() Provider {
	if r.CodeHost != "" {
		return r.CodeHost
	}
	if !r.AzureDevOps.IsZero() {
		return ProviderAzureDevOps
	}
	return ProviderGitHub
}

var installationIdentityPattern = regexp.MustCompile(`^install-[0-9a-f]{32}$`)

// validateProvider keeps old metadata and stored payload identities unchanged,
// while accepting a local tracker with either supported code-host adapter.
func validateProvider(prefix string, repository RepositoryRegistration) error {
	if err := validateLegacyProvider(prefix, repository); err != nil {
		return err
	}
	host := repository.EffectiveCodeHost()
	if host != ProviderGitHub && host != ProviderAzureDevOps {
		return validation(prefix+".code_host", "must be github or azure_devops")
	}
	if host == ProviderGitHub && !repository.AzureDevOps.IsZero() || host == ProviderAzureDevOps && repository.AzureDevOps.IsZero() {
		return validation(prefix+".code_host", "must match the configured code-host identity")
	}
	tracker := repository.EffectiveTracker()
	if tracker != ProviderGitHub && tracker != ProviderAzureDevOps && tracker != ProviderLocal {
		return validation(prefix+".tracker", "must be github, azure_devops, or local")
	}
	if tracker != ProviderLocal && tracker != host {
		return validation(prefix+".tracker", "remote tracker must match its configured provider identity")
	}
	local := repository.LocalOperator
	if tracker == ProviderLocal {
		if !installationIdentityPattern.MatchString(local.InstallationID) {
			return validation(prefix+".local_operator.installation_id", "must be install- followed by 32 lowercase hexadecimal characters")
		}
		if strings.TrimSpace(local.Name) != local.Name || local.Name == "" || len(local.Name) > 128 || strings.IndexFunc(local.Name, unicode.IsControl) >= 0 {
			return validation(prefix+".local_operator.name", "must be an explicit nonempty local operator name without control characters")
		}
	} else if local != (LocalOperatorConfig{}) {
		return validation(prefix+".local_operator", "is only valid with the local tracker")
	}
	return nil
}

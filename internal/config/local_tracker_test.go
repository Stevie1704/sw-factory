package config_test

import (
	"github.com/Stevie1704/sw-factory/internal/config"
	"testing"
)

// TestLocalTrackerSelectsCodeHostIndependently preserves legacy identities while requiring local provenance.
func TestLocalTrackerSelectsCodeHostIndependently(t *testing.T) {
	registration := validRegistration()
	registration.Tracker = config.ProviderLocal
	registration.CodeHost = config.ProviderGitHub
	registration.LocalOperator = config.LocalOperatorConfig{InstallationID: "install-00112233445566778899aabbccddeeff", Name: "operator"}
	if err := config.ValidateHost(config.HostConfig{SchemaVersion: 2, Repositories: []config.RepositoryRegistration{registration}}); err != nil {
		t.Fatal(err)
	}
	if registration.EffectiveTracker() != config.ProviderLocal || registration.EffectiveCodeHost() != config.ProviderGitHub {
		t.Fatal("providers not independent")
	}
	registration.GitHub = config.GitHubConfig{}
	registration.AzureDevOps = config.AzureDevOpsConfig{Organization: "company", Project: "project", Repository: "service"}
	registration.CodeHost = config.ProviderAzureDevOps
	if err := config.ValidateHost(config.HostConfig{SchemaVersion: 2, Repositories: []config.RepositoryRegistration{registration}}); err != nil {
		t.Fatal(err)
	}
	registration.LocalOperator.Name = ""
	if err := config.ValidateHost(config.HostConfig{SchemaVersion: 2, Repositories: []config.RepositoryRegistration{registration}}); err == nil {
		t.Fatal("missing local operator accepted")
	}
}

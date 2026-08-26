package config_test

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// TestValidateHostRequiresAnAbsoluteCodexAuthSource verifies host configuration
// stores only an explicit absolute credential-file path.
func TestValidateHostRequiresAnAbsoluteCodexAuthSource(t *testing.T) {
	value := config.HostConfig{SchemaVersion: config.CurrentSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path:                 "/Users/example/project",
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Authentication:       config.AuthenticationConfig{CodexAuthPath: "auth.json"},
		OperationalDataPath:  "/Users/example/data/factory.db",
		RepositoryConfigPath: "/Users/example/project/factory.yaml",
	}}}
	if err := config.ValidateHost(value); err == nil || !strings.Contains(err.Error(), "authentication.codex_auth_path") {
		t.Fatalf("ValidateHost() error = %v, want absolute auth path error", err)
	}
}

// TestValidateHostRejectsControlCharactersInCodexAuthSource verifies a host
// registration cannot persist a path that can alter later command arguments.
func TestValidateHostRejectsControlCharactersInCodexAuthSource(t *testing.T) {
	for _, test := range []struct {
		name    string
		control string
	}{
		{name: "nul", control: "\x00"},
		{name: "carriage-return", control: "\r"},
		{name: "newline", control: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := config.HostConfig{SchemaVersion: config.CurrentSchemaVersion, Repositories: []config.RepositoryRegistration{{
				Path:                 "/Users/example/project",
				GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
				AuthorizedUsers:      []string{"alice"},
				Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
				Authentication:       config.AuthenticationConfig{CodexAuthPath: "/Users/example/auth.json" + test.control},
				OperationalDataPath:  "/Users/example/data/factory.db",
				RepositoryConfigPath: "/Users/example/project/factory.yaml",
			}}}
			if err := config.ValidateHost(value); err == nil || !strings.Contains(err.Error(), "control characters") {
				t.Fatalf("ValidateHost() error = %v, want control-character rejection", err)
			}
		})
	}
}

// TestValidateHostRequiresAnAbsoluteClaudeAuthSource verifies the Claude
// credential source is stored on the same narrow terms as the Codex source.
func TestValidateHostRequiresAnAbsoluteClaudeAuthSource(t *testing.T) {
	value := config.HostConfig{SchemaVersion: config.CurrentSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path:                 "/Users/example/project",
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Authentication:       config.AuthenticationConfig{ClaudeAuthPath: ".credentials.json"},
		OperationalDataPath:  "/Users/example/data/factory.db",
		RepositoryConfigPath: "/Users/example/project/factory.yaml",
	}}}
	if err := config.ValidateHost(value); err == nil || !strings.Contains(err.Error(), "authentication.claude_auth_path") {
		t.Fatalf("ValidateHost() error = %v, want absolute auth path error", err)
	}
}

// TestValidateHostRejectsControlCharactersInClaudeAuthSource verifies a host
// registration cannot persist a path that can alter later command arguments.
func TestValidateHostRejectsControlCharactersInClaudeAuthSource(t *testing.T) {
	for _, test := range []struct {
		name    string
		control string
	}{
		{name: "nul", control: "\x00"},
		{name: "carriage-return", control: "\r"},
		{name: "newline", control: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := config.HostConfig{SchemaVersion: config.CurrentSchemaVersion, Repositories: []config.RepositoryRegistration{{
				Path:                 "/Users/example/project",
				GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
				AuthorizedUsers:      []string{"alice"},
				Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
				Authentication:       config.AuthenticationConfig{ClaudeAuthPath: "/Users/example/.credentials.json" + test.control},
				OperationalDataPath:  "/Users/example/data/factory.db",
				RepositoryConfigPath: "/Users/example/project/factory.yaml",
			}}}
			if err := config.ValidateHost(value); err == nil || !strings.Contains(err.Error(), "control characters") {
				t.Fatalf("ValidateHost() error = %v, want control-character rejection", err)
			}
		})
	}
}

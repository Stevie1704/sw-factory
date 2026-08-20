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

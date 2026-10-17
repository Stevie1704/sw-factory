package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// copilotRepositoryConfig returns a valid repository whose implementation role
// runs on Copilot while the test role stays on Codex.
func copilotRepositoryConfig() config.RepositoryConfig {
	value := validRepositoryConfig()
	value.RoleHarnessDefaults["implementation"] = config.HarnessCopilot
	value.ModelOptions["implementation"] = []string{"gpt-5.2"}
	return value
}

// TestValidateRepositoryAcceptsCopilotRoleSelection verifies a repository can
// select Copilot for a role with the effort values the pinned CLI accepts.
func TestValidateRepositoryAcceptsCopilotRoleSelection(t *testing.T) {
	value := copilotRepositoryConfig()
	value.ReasoningEffortOptions = map[string][]string{"implementation": {"none", "minimal", "low", "medium", "high", "xhigh", "max"}}
	if err := config.ValidateRepository(value); err != nil {
		t.Fatalf("ValidateRepository() error = %v, want Copilot role selection accepted", err)
	}
}

// TestValidateRepositoryRejectsUnsupportedCopilotSelections verifies a Copilot
// role cannot declare an effort the CLI rejects or the model value that lets
// Copilot choose a different model silently.
func TestValidateRepositoryRejectsUnsupportedCopilotSelections(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*config.RepositoryConfig)
		field  string
	}{
		{
			name: "unsupported effort",
			mutate: func(c *config.RepositoryConfig) {
				c.ReasoningEffortOptions = map[string][]string{"implementation": {"bogus"}}
			},
			field: "reasoning_effort_options.implementation[0]",
		},
		{
			name:   "automatic model",
			mutate: func(c *config.RepositoryConfig) { c.ModelOptions["implementation"] = []string{"gpt-5.2", "Auto"} },
			field:  "model_options.implementation[1]",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := copilotRepositoryConfig()
			test.mutate(&value)
			var validationErr *config.ValidationError
			if err := config.ValidateRepository(value); !errors.As(err, &validationErr) || validationErr.Field != test.field {
				t.Fatalf("ValidateRepository() error = %v, want ValidationError on %s", err, test.field)
			}
		})
	}
}

// TestValidateHarnessSelectionChecksEachHarnessOwnValues verifies the shared
// selection check applies the value set of the selected harness only.
func TestValidateHarnessSelectionChecksEachHarnessOwnValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		harness config.Harness
		model   string
		effort  string
		wantErr bool
	}{
		{name: "codex accepts its own effort vocabulary", harness: config.HarnessCodex, model: "gpt-5", effort: "codex-effort"},
		{name: "claude refuses minimal", harness: config.HarnessClaude, model: "opus", effort: "minimal", wantErr: true},
		{name: "copilot accepts minimal", harness: config.HarnessCopilot, model: "gpt-5.2", effort: "minimal"},
		{name: "copilot refuses bogus effort", harness: config.HarnessCopilot, model: "gpt-5.2", effort: "bogus", wantErr: true},
		{name: "copilot refuses automatic model", harness: config.HarnessCopilot, model: "auto", wantErr: true},
		{name: "codex accepts auto as an ordinary model name", harness: config.HarnessCodex, model: "auto"},
		{name: "unknown harness", harness: "gemini", model: "x", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := config.ValidateHarnessSelection(test.harness, test.model, test.effort)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateHarnessSelection() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

// TestHarnessOverrideTargetsKeepOldAuthorizationsNarrow verifies an existing
// allowed_overrides: [harness] keeps its original Codex/Claude meaning, and
// only an explicit override_harnesses list makes Copilot reachable by override.
func TestHarnessOverrideTargetsKeepOldAuthorizationsNarrow(t *testing.T) {
	legacy := validRepositoryConfig()
	legacy.AllowedOverrides = []config.OverrideName{config.OverrideHarness}
	if got, want := legacy.HarnessOverrideTargets(), []config.Harness{config.HarnessCodex, config.HarnessClaude}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy HarnessOverrideTargets() = %v, want %v", got, want)
	}
	if legacy.AllowsHarnessOverrideTo(config.HarnessCopilot) {
		t.Fatal("legacy harness override reaches Copilot without an explicit opt-in")
	}
	if !legacy.AllowsHarnessOverrideTo(config.HarnessClaude) {
		t.Fatal("legacy harness override no longer reaches Claude")
	}

	optedIn := legacy
	optedIn.OverrideHarnesses = []config.Harness{config.HarnessCodex, config.HarnessCopilot}
	if err := config.ValidateRepository(optedIn); err != nil {
		t.Fatalf("ValidateRepository() error = %v", err)
	}
	if !optedIn.AllowsHarnessOverrideTo(config.HarnessCopilot) || optedIn.AllowsHarnessOverrideTo(config.HarnessClaude) {
		t.Fatalf("opted-in targets = %v, want exactly the declared list", optedIn.HarnessOverrideTargets())
	}

	noOverride := validRepositoryConfig()
	if noOverride.AllowsHarnessOverrideTo(config.HarnessClaude) {
		t.Fatal("a repository without allowed_overrides: [harness] permits a harness override")
	}
}

// TestValidateRepositoryRejectsInvalidOverrideHarnesses verifies the opt-in
// list is only meaningful together with a harness override permission.
func TestValidateRepositoryRejectsInvalidOverrideHarnesses(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides []config.OverrideName
		targets   []config.Harness
		field     string
	}{
		{name: "without harness override", targets: []config.Harness{config.HarnessCopilot}, field: "override_harnesses"},
		{name: "empty list", overrides: []config.OverrideName{config.OverrideHarness}, targets: []config.Harness{}, field: "override_harnesses"},
		{name: "unknown harness", overrides: []config.OverrideName{config.OverrideHarness}, targets: []config.Harness{"gemini"}, field: "override_harnesses[0]"},
		{name: "duplicate", overrides: []config.OverrideName{config.OverrideHarness}, targets: []config.Harness{config.HarnessCopilot, config.HarnessCopilot}, field: "override_harnesses[1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := validRepositoryConfig()
			value.AllowedOverrides = test.overrides
			value.OverrideHarnesses = test.targets
			var validationErr *config.ValidationError
			if err := config.ValidateRepository(value); !errors.As(err, &validationErr) || validationErr.Field != test.field {
				t.Fatalf("ValidateRepository() error = %v, want ValidationError on %s", err, test.field)
			}
		})
	}
}

// TestLoadRepositoryKeepsAnOldHarnessOverrideConfigurationValid verifies a
// checked-in configuration written before Copilot support still loads and
// does not gain Copilot as an override target.
func TestLoadRepositoryKeepsAnOldHarnessOverrideConfigurationValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "allowed_overrides: []") {
		t.Fatal("project factory.yaml no longer declares allowed_overrides: []; update this fixture")
	}
	text = strings.Replace(text, "allowed_overrides: []", "allowed_overrides: [harness]", 1)
	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadRepository(path)
	if err != nil {
		t.Fatalf("LoadRepository() error = %v", err)
	}
	if loaded.AllowsHarnessOverrideTo(config.HarnessCopilot) {
		t.Fatal("an old configuration can override to Copilot")
	}
}

// TestValidateHostRequiresAnAbsoluteCopilotAuthSource verifies the Copilot
// token source is stored on the same narrow terms as the other sources.
func TestValidateHostRequiresAnAbsoluteCopilotAuthSource(t *testing.T) {
	value := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path:                 "/Users/example/project",
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Authentication:       config.AuthenticationConfig{CopilotAuthPath: "copilot-token"},
		OperationalDataPath:  "/Users/example/data/factory.db",
		RepositoryConfigPath: "/Users/example/project/factory.yaml",
	}}}
	if err := config.ValidateHost(value); err == nil || !strings.Contains(err.Error(), "authentication.copilot_auth_path") {
		t.Fatalf("ValidateHost() error = %v, want absolute auth path error", err)
	}
}

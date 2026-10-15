// Package acceptance holds the agent-journey acceptance scenarios that ship
// inside the factory binary and builds the release evidence report from the
// recorded outcome of a run. The scenarios measure whether a fresh operator
// agent can use a release through supported interfaces. The report keeps
// deterministic and live evidence apart, so a fake-adapter pass never reads as
// live agent usability.
package acceptance

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// manifestSchema is the version of the scenario manifest.
const manifestSchema = 1

//go:embed scenarios.yaml
var manifestSource []byte

// testReference matches "<package directory>:<test name>".
var testReference = regexp.MustCompile(`^(\.|internal/[a-z0-9/]+):Test[A-Za-z0-9_]+$`)

// Manifest is the versioned list of acceptance scenarios.
type Manifest struct {
	SchemaVersion int        `json:"schema_version" yaml:"schema_version"`
	Scenarios     []Scenario `json:"scenarios" yaml:"scenarios"`
}

// Scenario describes one agent journey: where it starts, what the agent must
// achieve under which authority and interfaces, the condition injected into
// it, how success and failure are observed, and what it cleans up.
type Scenario struct {
	ID                 string   `json:"id" yaml:"id"`
	Title              string   `json:"title" yaml:"title"`
	Fixture            string   `json:"fixture" yaml:"fixture"`
	Objective          string   `json:"objective" yaml:"objective"`
	Authority          []string `json:"authority" yaml:"authority"`
	Interfaces         []string `json:"interfaces" yaml:"interfaces"`
	InjectedConditions []string `json:"injected_conditions" yaml:"injected_conditions"`
	Success            []string `json:"success" yaml:"success"`
	Failure            []string `json:"failure" yaml:"failure"`
	Cleanup            []string `json:"cleanup" yaml:"cleanup"`
	DeterministicTests []string `json:"deterministic_tests" yaml:"deterministic_tests"`
	Gaps               []Gap    `json:"gaps" yaml:"gaps"`
}

// Gap is a capability that a scenario needs and the current release does not
// ship, with the issue that tracks it.
type Gap struct {
	Issue   int    `json:"issue" yaml:"issue"`
	Summary string `json:"summary" yaml:"summary"`
}

// LoadManifest decodes and validates the embedded scenario manifest.
func LoadManifest() (Manifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(manifestSource))
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode acceptance scenarios: %w", err)
	}
	if err := manifest.validate(); err != nil {
		return Manifest{}, fmt.Errorf("acceptance scenarios: %w", err)
	}
	for index := range manifest.Scenarios {
		// JSON readers get an empty list, not null, for a scenario without gaps.
		if manifest.Scenarios[index].Gaps == nil {
			manifest.Scenarios[index].Gaps = []Gap{}
		}
	}
	return manifest, nil
}

// Scenario returns the scenario with the given identity.
func (m Manifest) Scenario(id string) (Scenario, bool) {
	for _, scenario := range m.Scenarios {
		if scenario.ID == id {
			return scenario, true
		}
	}
	return Scenario{}, false
}

// validate refuses a manifest whose scenarios are incomplete, duplicated, or
// whose test references cannot name a test.
func (m Manifest) validate() error {
	if m.SchemaVersion != manifestSchema {
		return fmt.Errorf("schema_version %d, want %d", m.SchemaVersion, manifestSchema)
	}
	if len(m.Scenarios) == 0 {
		return errors.New("no scenarios")
	}
	seen := map[string]bool{}
	for _, scenario := range m.Scenarios {
		if seen[scenario.ID] {
			return fmt.Errorf("duplicate scenario %q", scenario.ID)
		}
		seen[scenario.ID] = true
		if err := scenario.validate(); err != nil {
			return fmt.Errorf("scenario %q: %w", scenario.ID, err)
		}
	}
	return nil
}

// validate refuses a scenario that lacks an observable field, or that has
// neither deterministic coverage nor a recorded gap.
func (s Scenario) validate() error {
	if s.ID == "" || s.Title == "" || s.Fixture == "" || s.Objective == "" {
		return errors.New("needs id, title, fixture, and objective")
	}
	if len(s.Authority) == 0 || len(s.Interfaces) == 0 || len(s.Success) == 0 || len(s.Failure) == 0 || len(s.Cleanup) == 0 {
		return errors.New("needs authority, interfaces, success, failure, and cleanup")
	}
	if len(s.DeterministicTests) == 0 && len(s.Gaps) == 0 {
		return errors.New("needs deterministic tests or a recorded gap")
	}
	for _, reference := range s.DeterministicTests {
		if !testReference.MatchString(reference) {
			return fmt.Errorf("test reference %q is not <package directory>:<test name>", reference)
		}
	}
	for _, gap := range s.Gaps {
		if gap.Issue <= 0 || gap.Summary == "" {
			return errors.New("a gap needs an issue number and a summary")
		}
	}
	return nil
}

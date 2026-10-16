package onboarding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// fixtureDigest is the worker digest of the fixture factory.yaml.
const fixtureDigest = "sha256:44de7b30d96aa23a2db2e38a75e1431e32ede7b6446694d9d782681aeeddce54"

// fakeImages answers every image check with err and records the reference.
type fakeImages struct {
	err     error
	checked []worker.ImageReference
}

// CheckImage records the reference and returns the configured error.
func (f *fakeImages) CheckImage(_ context.Context, image worker.ImageReference) error {
	f.checked = append(f.checked, image)
	return f.err
}

// fixtureRepository creates a Git checkout with the Factory's own
// factory.yaml, a worker build context, and a smoke record of the pinned
// digest that proves only the implement skill for codex.
func fixtureRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	policy, err := os.ReadFile(filepath.Join("..", "..", "factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repository, "factory.yaml", string(policy))
	writeFile(t, repository, "worker/Dockerfile", "FROM scratch\n")
	writeFile(t, repository, "worker/skills/implement/SKILL.md", "implement\n")
	writeFile(t, repository, "worker/skill-smoke.json", `{"schema_version":1,"records":[{"image_digest":"`+fixtureDigest+`","harness":"codex","harness_version":"codex-cli 0.148.0","skills":["implement"],"verified_at":"2026-10-09T08:19:58Z"}]}`)
	writeFile(t, repository, "README.md", "fixture\n")
	return repository
}

// writeFile writes one repository-relative file and its parents.
func writeFile(t *testing.T, repository, relative, body string) {
	t.Helper()
	path := filepath.Join(repository, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// register writes a host configuration that registers repository.
func register(t *testing.T, repository string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	host := config.NewHostConfig()
	host.Repositories = []config.RepositoryRegistration{{
		Path:                 repository,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"operator"},
		Polling:              config.PollingConfig{Interval: "1m", Backoff: "5m"},
		OperationalDataPath:  filepath.Join(filepath.Dir(path), "data", "factory.db"),
		RepositoryConfigPath: filepath.Join(repository, "factory.yaml"),
	}}
	if err := config.SaveHost(path, host); err != nil {
		t.Fatal(err)
	}
	return path
}

// observe observes one plan with an image check that succeeds.
func observe(t *testing.T, plan Plan) Observation {
	t.Helper()
	observer := Observer{
		Images:          &fakeImages{},
		Release:         Release{Version: "v1.0.0", Revision: "abc", ResourceIdentity: "sha256:resources"},
		VerifyResources: func(string) (string, error) { return "sha256:resources", nil },
	}
	return observer.Observe(context.Background(), plan)
}

// TestObserveReadsTheRepositoryInputs verifies that a valid checkout yields
// its policy, pinned image, registration, and the partial smoke coverage of
// each harness the policy selects.
func TestObserveReadsTheRepositoryInputs(t *testing.T) {
	repository := fixtureRepository(t)
	plan := NewPlan(repository, register(t, repository), "/resources")
	observation := observe(t, plan)

	for _, check := range []Check{observation.Resources, observation.Configuration, observation.Image, observation.Registration} {
		if !check.Done {
			t.Fatalf("check = %+v, want done", check)
		}
	}
	if got := observation.Inputs[InputWorkerImage]; got != "ghcr.io/stevie1704/sw-factory-worker@"+fixtureDigest {
		t.Fatalf("worker image = %q", got)
	}
	for _, input := range []Input{InputRelease, InputPolicy, InputWorkerDefinition, InputGateInputs, InputRegistration, InputSkillEvidence} {
		if !strings.HasPrefix(observation.Inputs[input], "sha256:") {
			t.Fatalf("input %s = %q, want a digest", input, observation.Inputs[input])
		}
	}
	if len(observation.Smoke) != 1 || observation.Smoke[0].Harness != "codex" {
		t.Fatalf("smoke = %+v, want only codex, which the policy selects", observation.Smoke)
	}
	smoke := observation.Smoke[0]
	if !slices.Equal(smoke.Verified, []string{"implement"}) || !slices.Contains(smoke.Missing, "standards-review") || slices.Contains(smoke.Missing, "implement") {
		t.Fatalf("codex smoke = %+v, want implement verified and the others missing", smoke)
	}
	if smoke.HarnessVersion != "codex-cli 0.148.0" {
		t.Fatalf("harness version = %q", smoke.HarnessVersion)
	}
}

// TestObserveSeparatesTheInputsOfEachStep verifies that a source change
// changes only the gate inputs, that a new smoke record changes only the
// skill evidence, and that a vendored skill change changes the worker
// definition.
func TestObserveSeparatesTheInputsOfEachStep(t *testing.T) {
	repository := fixtureRepository(t)
	plan := NewPlan(repository, register(t, repository), "")
	before := observe(t, plan)

	writeFile(t, repository, "README.md", "changed\n")
	source := observe(t, plan)
	if source.Inputs[InputGateInputs] == before.Inputs[InputGateInputs] || source.Inputs[InputWorkerDefinition] != before.Inputs[InputWorkerDefinition] || source.Inputs[InputPolicy] != before.Inputs[InputPolicy] {
		t.Fatal("a source change must change only the gate inputs")
	}

	writeFile(t, repository, "worker/skill-smoke.json", `{"schema_version":1,"records":[{"image_digest":"`+fixtureDigest+`","harness":"codex","harness_version":"codex-cli 0.148.0","skills":["implement","specification-review"],"verified_at":"2026-10-10T08:19:58Z"}]}`)
	smoke := observe(t, plan)
	if smoke.Inputs[InputSkillEvidence] == source.Inputs[InputSkillEvidence] || smoke.Inputs[InputGateInputs] != source.Inputs[InputGateInputs] || smoke.Inputs[InputWorkerDefinition] != source.Inputs[InputWorkerDefinition] {
		t.Fatal("a smoke record must change only the skill evidence")
	}

	writeFile(t, repository, "worker/skills/implement/SKILL.md", "changed\n")
	skills := observe(t, plan)
	if skills.Inputs[InputWorkerDefinition] == smoke.Inputs[InputWorkerDefinition] {
		t.Fatal("a vendored skill change must change the worker definition")
	}
}

// TestObserveReportsWhatIsMissing verifies the problems of a checkout
// without a policy, an unprepared or foreign resource directory, a host
// configuration of another repository, and an unavailable image.
func TestObserveReportsWhatIsMissing(t *testing.T) {
	repository := fixtureRepository(t)
	other := t.TempDir()
	plan := NewPlan(repository, register(t, other), "/resources")
	images := &fakeImages{err: errors.New("worker image inspection failed")}
	observer := Observer{
		Images:          images,
		Release:         Release{ResourceIdentity: "sha256:resources"},
		VerifyResources: func(string) (string, error) { return "sha256:other", nil },
	}
	observation := observer.Observe(context.Background(), plan)
	if observation.Resources.Done || !strings.Contains(observation.Resources.Problem, "another release") {
		t.Fatalf("resources = %+v", observation.Resources)
	}
	if observation.Registration.Done || !strings.Contains(observation.Registration.Problem, "another repository") {
		t.Fatalf("registration = %+v", observation.Registration)
	}
	if observation.Image.Done || !strings.Contains(observation.Image.Problem, "inspection failed") {
		t.Fatalf("image = %+v", observation.Image)
	}

	if err := os.Remove(filepath.Join(repository, "factory.yaml")); err != nil {
		t.Fatal(err)
	}
	observation = observer.Observe(context.Background(), NewPlan(repository, filepath.Join(t.TempDir(), "missing.yaml"), ""))
	if observation.Configuration.Done || observation.Inputs[InputPolicy] != "" || observation.Inputs[InputWorkerImage] != "" {
		t.Fatalf("configuration = %+v, inputs %v", observation.Configuration, observation.Inputs)
	}
	if observation.Registration.Done || !strings.Contains(observation.Registration.Problem, "no host configuration") {
		t.Fatalf("registration = %+v", observation.Registration)
	}
}

// TestRefreshRecordsTheObservedImageOnce verifies that the first observation
// of a pinned image records its build context, and that a later edit of the
// context without a rebuild keeps the original record, so the image becomes
// stale.
func TestRefreshRecordsTheObservedImageOnce(t *testing.T) {
	repository := fixtureRepository(t)
	plan := NewPlan(repository, register(t, repository), "/resources")
	plan.Refresh(observe(t, plan))
	recorded := plan.Evidence[StepImage]
	if recorded.Outcome != OutcomePassed || recorded.Inputs[InputWorkerDefinition] == "" {
		t.Fatalf("image evidence = %+v", recorded)
	}

	writeFile(t, repository, "worker/Dockerfile", "FROM scratch\nUSER factory\n")
	observation := observe(t, plan)
	plan.Refresh(observation)
	if plan.Evidence[StepImage].Inputs[InputWorkerDefinition] != recorded.Inputs[InputWorkerDefinition] {
		t.Fatal("refresh replaced the image evidence of the same digest")
	}
	if step := stepOf(t, Evaluate(plan, observation), StepImage); step.State != StateStale {
		t.Fatalf("image = %+v, want stale", step)
	}
}

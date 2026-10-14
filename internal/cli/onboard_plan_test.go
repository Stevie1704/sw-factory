package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	swfactory "github.com/Stevie1704/sw-factory"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// planDigest is the worker digest of the Factory's own factory.yaml.
const planDigest = "sha256:20bad8e4d840d27599c64f047eebba1084fa22ca04112a8dde908a7420bda7b6"

// presentImages reports every pinned image as present.
type presentImages struct{}

// CheckImage accepts every image.
func (presentImages) CheckImage(context.Context, worker.ImageReference) error { return nil }

// planFakes are the adapters an onboarding plan test controls.
type planFakes struct {
	runtime      *verifyRuntime
	doctorCalls  int
	doctorResult factory.DoctorResult
	proof        factory.OnboardingProofObservation
	proofErr     error
	proofCalls   int
}

// usePlanFakes replaces the Docker, doctor, and proof observation adapters
// for one test.
func usePlanFakes(t *testing.T) *planFakes {
	t.Helper()
	fakes := &planFakes{runtime: &verifyRuntime{}}
	useVerifyRuntime(t, fakes.runtime)
	images, doctorRun, observe := onboardingImagesForCLI, onboardingDoctorForCLI, onboardingProofForCLI
	t.Cleanup(func() {
		onboardingImagesForCLI, onboardingDoctorForCLI, onboardingProofForCLI = images, doctorRun, observe
	})
	onboardingImagesForCLI = func() onboarding.ImageChecker { return presentImages{} }
	onboardingDoctorForCLI = func(context.Context, string) (factory.DoctorResult, error) {
		fakes.doctorCalls++
		return fakes.doctorResult, nil
	}
	onboardingProofForCLI = func(context.Context, string, int, string) (factory.OnboardingProofObservation, error) {
		fakes.proofCalls++
		return fakes.proof, fakes.proofErr
	}
	return fakes
}

// planFixture is an onboarding candidate after the image build, with a
// partial skill smoke and a host registration.
type planFixture struct {
	repository string
	hostConfig string
	resources  string
	plan       string
}

// newPlanFixture creates the candidate, its registration, and a prepared
// resource directory.
func newPlanFixture(t *testing.T) planFixture {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", "factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	repository := verifyCandidate(t, contents)
	writePlanFile(t, repository, "worker/Dockerfile", "FROM scratch\n")
	writeSmoke(t, repository, "implement")
	resources := filepath.Join(t.TempDir(), "resources")
	if _, err := swfactory.PrepareResources(resources); err != nil {
		t.Fatal(err)
	}
	hostConfig := filepath.Join(t.TempDir(), "config.yaml")
	host := config.NewHostConfig()
	host.Repositories = []config.RepositoryRegistration{{
		Path:                 repository,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"operator"},
		Polling:              config.PollingConfig{Interval: "1m", Backoff: "5m"},
		OperationalDataPath:  filepath.Join(filepath.Dir(hostConfig), "data", "factory.db"),
		RepositoryConfigPath: filepath.Join(repository, "factory.yaml"),
	}}
	if err := config.SaveHost(hostConfig, host); err != nil {
		t.Fatal(err)
	}
	return planFixture{repository: repository, hostConfig: hostConfig, resources: resources, plan: filepath.Join(t.TempDir(), "plans", "onboarding.json")}
}

// writePlanFile writes one repository-relative file.
func writePlanFile(t *testing.T, repository, relative, body string) {
	t.Helper()
	path := filepath.Join(repository, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSmoke records codex smoke evidence of the pinned digest for skills.
func writeSmoke(t *testing.T, repository string, skills ...string) {
	t.Helper()
	encoded, err := json.Marshal(skills)
	if err != nil {
		t.Fatal(err)
	}
	writePlanFile(t, repository, "worker/skill-smoke.json", `{"schema_version":1,"records":[{"image_digest":"`+planDigest+`","harness":"codex","harness_version":"codex-cli 0.148.0","skills":`+string(encoded)+`,"verified_at":"2026-10-09T08:19:58Z"}]}`)
}

// planDocument is the part of the status document the tests read.
type planDocument struct {
	SchemaVersion int    `json:"schema_version"`
	Result        string `json:"result"`
	Steps         []struct {
		Name     string `json:"name"`
		State    string `json:"state"`
		Reason   string `json:"reason"`
		Action   string `json:"action"`
		Evidence *struct {
			Outcome  string   `json:"outcome"`
			Findings []string `json:"findings"`
		} `json:"evidence"`
	} `json:"steps"`
	Artifacts struct {
		Resources       string `json:"resources"`
		ProofWorkspaces string `json:"proof_workspaces"`
	} `json:"artifacts"`
}

// state returns the state, reason, and action of one step.
func (d planDocument) state(name string) (string, string, string) {
	for _, step := range d.Steps {
		if step.Name == name {
			return step.State, step.Reason, step.Action
		}
	}
	return "", "", ""
}

// recordDocument is the part of a record document the tests read.
type recordDocument struct {
	SchemaVersion int          `json:"schema_version"`
	Step          string       `json:"step"`
	Reused        bool         `json:"reused"`
	Marker        string       `json:"marker"`
	Status        planDocument `json:"status"`
}

// runPlanCommand runs one CLI command and returns its exit status, standard
// output, and standard error.
func runPlanCommand(args ...string) (int, string, string) {
	var output, errorsOutput bytes.Buffer
	status := Run(context.Background(), args, &output, &errorsOutput)
	return status, output.String(), errorsOutput.String()
}

// decode decodes one JSON document or fails the test.
func decode[T any](t *testing.T, output, errorsOutput string) T {
	t.Helper()
	var document T
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatalf("output is not JSON: %v: %q (stderr %q)", err, output, errorsOutput)
	}
	return document
}

// TestOnboardPlanContinuesAfterAnInterruption verifies that a fresh agent can
// continue after the image build, a partial smoke, and the host
// registration, without repeating completed work, and reach each final
// result.
func TestOnboardPlanContinuesAfterAnInterruption(t *testing.T) {
	fakes := usePlanFakes(t)
	fixture := newPlanFixture(t)

	status, output, errorsOutput := runPlanCommand("onboard", "plan", "--repository", fixture.repository, "--output", fixture.plan, "--config", fixture.hostConfig, "--resources", fixture.resources, "--json")
	document := decode[planDocument](t, output, errorsOutput)
	if status != 0 || document.SchemaVersion != 1 || document.Result != "incomplete" {
		t.Fatalf("plan status = %d, document = %+v, stderr %q", status, document, errorsOutput)
	}
	for _, name := range []string{"prepare", "configuration", "image", "registration"} {
		if state, reason, _ := document.state(name); state != "verified" {
			t.Fatalf("%s = %s (%s), want verified", name, state, reason)
		}
	}
	if state, _, action := document.state("skill_smoke"); state != "pending" || !strings.Contains(action, "HARNESSES=codex") || strings.Contains(action, "'implement") {
		t.Fatalf("skill smoke = %s, action %q; want a retry without implement", state, action)
	}
	if document.Artifacts.Resources != fixture.resources || document.Artifacts.ProofWorkspaces == "" {
		t.Fatalf("artifacts = %+v", document.Artifacts)
	}

	status, output, errorsOutput = runPlanCommand("onboard", "record", "--plan", fixture.plan, "gate-proof", "--json")
	recorded := decode[recordDocument](t, output, errorsOutput)
	if state, reason, _ := recorded.Status.state("gate_proof"); status != 0 || recorded.Reused || state != "verified" {
		t.Fatalf("gate proof status = %d, state %s (%s), stderr %q", status, state, reason, errorsOutput)
	}
	cleaned := fakes.runtime.cleaned
	status, output, errorsOutput = runPlanCommand("onboard", "record", "--plan", fixture.plan, "gate-proof", "--json")
	if recorded = decode[recordDocument](t, output, errorsOutput); status != 0 || !recorded.Reused || fakes.runtime.cleaned != cleaned {
		t.Fatalf("a repeated gate proof with unchanged inputs ran again: status %d, reused %v", status, recorded.Reused)
	}

	writeSmoke(t, fixture.repository, "implement", "specification-review", "standards-review")
	fakes.doctorResult = factory.DoctorResult{Report: doctor.Report{Results: []doctor.Result{{Name: "configuration", Status: doctor.StatusPassed}}}}
	status, output, errorsOutput = runPlanCommand("onboard", "record", "--plan", fixture.plan, "doctor", "--json")
	recorded = decode[recordDocument](t, output, errorsOutput)
	if status != 0 || recorded.Status.Result != "doctor_ready" {
		t.Fatalf("doctor status = %d, result %s, stderr %q", status, recorded.Status.Result, errorsOutput)
	}

	status, output, errorsOutput = runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--start", "--json")
	recorded = decode[recordDocument](t, output, errorsOutput)
	if status != 0 || !strings.HasPrefix(recorded.Marker, "factory-onboarding-proof:") {
		t.Fatalf("start status = %d, marker %q, stderr %q", status, recorded.Marker, errorsOutput)
	}
	if status, _, errorsOutput = runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--start", "--json"); status != 1 || !strings.Contains(errorsOutput, recorded.Marker) {
		t.Fatalf("a second start was not refused: status %d, stderr %q", status, errorsOutput)
	}

	fakes.proof = factory.OnboardingProofObservation{CarriesMarker: true, Run: &store.Run{IssueNumber: 7, Stage: store.StageDraftPR, Status: store.StatusActive}}
	status, output, errorsOutput = runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--issue", "7", "--json")
	recorded = decode[recordDocument](t, output, errorsOutput)
	if status != 0 || recorded.Status.Result != "live_proof_passed" {
		t.Fatalf("issue status = %d, result %s, stderr %q", status, recorded.Status.Result, errorsOutput)
	}

	status, output, errorsOutput = runPlanCommand("onboard", "status", "--plan", fixture.plan, "--json")
	if document = decode[planDocument](t, output, errorsOutput); status != 0 || document.Result != "live_proof_passed" {
		t.Fatalf("status = %d, result %s", status, document.Result)
	}
}

// TestOnboardStatusIsReadOnlyAndShowsStaleEvidence verifies that status
// writes nothing, and that a changed candidate file makes only the gate
// proof and the steps after it stale or blocked with a visible reason.
func TestOnboardStatusIsReadOnlyAndShowsStaleEvidence(t *testing.T) {
	usePlanFakes(t)
	fixture := newPlanFixture(t)
	if status, _, errorsOutput := runPlanCommand("onboard", "plan", "--repository", fixture.repository, "--output", fixture.plan, "--config", fixture.hostConfig, "--resources", fixture.resources, "--json"); status != 0 {
		t.Fatalf("plan: %s", errorsOutput)
	}
	if status, _, errorsOutput := runPlanCommand("onboard", "record", "--plan", fixture.plan, "gate-proof", "--json"); status != 0 {
		t.Fatalf("gate proof: %s", errorsOutput)
	}
	before, err := os.ReadFile(fixture.plan)
	if err != nil {
		t.Fatal(err)
	}

	writePlanFile(t, fixture.repository, "README.md", "changed\n")
	status, output, errorsOutput := runPlanCommand("onboard", "status", "--plan", fixture.plan, "--json")
	document := decode[planDocument](t, output, errorsOutput)
	if state, reason, _ := document.state("gate_proof"); status != 0 || state != "stale" || !strings.Contains(reason, "gate_inputs changed") {
		t.Fatalf("gate proof = %s (%s), want stale because the gate inputs changed", state, reason)
	}
	if state, _, _ := document.state("registration"); state != "verified" {
		t.Fatalf("registration = %s, want verified", state)
	}
	after, err := os.ReadFile(fixture.plan)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("status changed the plan")
	}
}

// TestOnboardRecordDoctorKeepsBlockingFindings verifies that a blocked
// doctor is recorded with its finding codes and exits 1.
func TestOnboardRecordDoctorKeepsBlockingFindings(t *testing.T) {
	fakes := usePlanFakes(t)
	fixture := newPlanFixture(t)
	if status, _, errorsOutput := runPlanCommand("onboard", "plan", "--repository", fixture.repository, "--output", fixture.plan, "--config", fixture.hostConfig, "--json"); status != 0 {
		t.Fatalf("plan: %s", errorsOutput)
	}
	fakes.doctorResult = factory.DoctorResult{Report: doctor.Report{Results: []doctor.Result{
		{Condition: doctor.Condition{Code: "github.labels.missing"}, Name: "labels", Status: doctor.StatusFailed},
		{Condition: doctor.Condition{Code: "harness.authentication.source_missing"}, Name: "auth", Status: doctor.StatusWarning},
	}}}

	status, output, errorsOutput := runPlanCommand("onboard", "record", "--plan", fixture.plan, "doctor", "--json")
	recorded := decode[recordDocument](t, output, errorsOutput)
	for _, step := range recorded.Status.Steps {
		if step.Name == "doctor" && (step.Evidence == nil || step.Evidence.Outcome != "failed" || strings.Join(step.Evidence.Findings, ",") != "github.labels.missing") {
			t.Fatalf("doctor evidence = %+v", step.Evidence)
		}
	}
	if status != 1 || fakes.doctorCalls != 1 {
		t.Fatalf("status = %d, doctor calls %d", status, fakes.doctorCalls)
	}
}

// TestOnboardLiveProofNeverRecordsAnUnconfirmedIssue verifies that an issue
// without the marker is refused, that an unknown outcome blocks a new start
// until the operator abandons it, and that a declined proof is final.
func TestOnboardLiveProofNeverRecordsAnUnconfirmedIssue(t *testing.T) {
	fakes := usePlanFakes(t)
	fixture := newPlanFixture(t)
	writeSmoke(t, fixture.repository, "implement", "specification-review", "standards-review")
	fakes.doctorResult = factory.DoctorResult{Report: doctor.Report{}}
	for _, args := range [][]string{
		{"onboard", "plan", "--repository", fixture.repository, "--output", fixture.plan, "--config", fixture.hostConfig, "--resources", fixture.resources, "--json"},
		{"onboard", "record", "--plan", fixture.plan, "gate-proof", "--json"},
		{"onboard", "record", "--plan", fixture.plan, "doctor", "--json"},
		{"onboard", "record", "--plan", fixture.plan, "live-proof", "--start", "--json"},
	} {
		if status, _, errorsOutput := runPlanCommand(args...); status != 0 {
			t.Fatalf("%v: status %d: %s", args, status, errorsOutput)
		}
	}

	fakes.proof = factory.OnboardingProofObservation{CarriesMarker: false}
	if status, _, errorsOutput := runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--issue", "9", "--json"); status != 1 || !strings.Contains(errorsOutput, "marker") {
		t.Fatalf("an issue without the marker was not refused: status %d, stderr %q", status, errorsOutput)
	}
	fakes.proofErr = errors.New("network unavailable")
	if status, _, _ := runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--issue", "9", "--json"); status != 1 {
		t.Fatalf("an unobserved issue was accepted: status %d", status)
	}
	status, output, errorsOutput := runPlanCommand("onboard", "status", "--plan", fixture.plan, "--json")
	document := decode[planDocument](t, output, errorsOutput)
	if state, reason, _ := document.state("live_proof"); status != 0 || state != "blocked" || !strings.Contains(reason, "unknown") {
		t.Fatalf("live proof = %s (%s), want blocked by an unknown outcome", state, reason)
	}
	if status, _, _ := runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--decline", "--json"); status != 1 {
		t.Fatal("a decline was accepted while the outcome is unknown")
	}

	if status, _, errorsOutput := runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--abandon", "--json"); status != 0 {
		t.Fatalf("abandon: status %d, stderr %q", status, errorsOutput)
	}
	status, output, errorsOutput = runPlanCommand("onboard", "record", "--plan", fixture.plan, "live-proof", "--decline", "--json")
	recorded := decode[recordDocument](t, output, errorsOutput)
	if status != 0 || recorded.Status.Result != "live_proof_declined" {
		t.Fatalf("decline status = %d, result %s, stderr %q", status, recorded.Status.Result, errorsOutput)
	}
}

// TestOnboardPlanRefusesUnsafeTargets verifies the usage and target checks:
// a plan inside the checkout, a plan of another repository, and missing
// arguments.
func TestOnboardPlanRefusesUnsafeTargets(t *testing.T) {
	usePlanFakes(t)
	fixture := newPlanFixture(t)
	if status, _, errorsOutput := runPlanCommand("onboard", "plan", "--repository", fixture.repository, "--output", filepath.Join(fixture.repository, "plan.json"), "--json"); status != 1 || !strings.Contains(errorsOutput, "outside") {
		t.Fatalf("a plan inside the checkout was accepted: status %d, stderr %q", status, errorsOutput)
	}
	if status, _, errorsOutput := runPlanCommand("onboard", "plan", "--repository", fixture.repository, "--output", fixture.plan, "--config", fixture.hostConfig, "--json"); status != 0 {
		t.Fatalf("plan: %s", errorsOutput)
	}
	other := newPlanFixture(t)
	if status, _, errorsOutput := runPlanCommand("onboard", "plan", "--repository", other.repository, "--output", fixture.plan, "--json"); status != 1 || !strings.Contains(errorsOutput, "another repository") {
		t.Fatalf("a plan of another repository was overwritten: status %d, stderr %q", status, errorsOutput)
	}
	for name, args := range map[string][]string{
		"plan without output":     {"onboard", "plan", "--repository", ".", "--json"},
		"status without json":     {"onboard", "status", "--plan", fixture.plan},
		"record without step":     {"onboard", "record", "--plan", fixture.plan, "--json"},
		"record unknown step":     {"onboard", "record", "--plan", fixture.plan, "image", "--json"},
		"live proof two actions":  {"onboard", "record", "--plan", fixture.plan, "live-proof", "--start", "--decline", "--json"},
		"live proof no action":    {"onboard", "record", "--plan", fixture.plan, "live-proof", "--json"},
		"status extra positional": {"onboard", "status", "--plan", fixture.plan, "--json", "extra"},
	} {
		if status, _, _ := runPlanCommand(args...); status != 2 {
			t.Errorf("%s: status = %d, want 2", name, status)
		}
	}
}

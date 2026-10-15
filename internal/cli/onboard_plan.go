package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	swfactory "github.com/Stevie1704/sw-factory"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/gateproof"
	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/version"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// onboardingStatusSchema is the version of the onboard status and record
// documents. Increment it when a field changes meaning or is removed.
const onboardingStatusSchema = 1

// onboardingProofMarkerPrefix starts the marker a proof issue body carries.
const onboardingProofMarkerPrefix = "factory-onboarding-proof:"

// onboardingImagesForCLI is the image inspection seam of the onboarding plan,
// so the CLI contract can be tested without Docker.
var onboardingImagesForCLI = func() onboarding.ImageChecker { return worker.NewDockerRuntime() }

// onboardingDoctorForCLI is the doctor seam of onboard record doctor.
var onboardingDoctorForCLI = func(ctx context.Context, configPath string) (factory.DoctorResult, error) {
	return newService(configPath).Doctor(ctx)
}

// onboardingProofForCLI is the live proof observation seam of onboard record
// live-proof.
var onboardingProofForCLI = func(ctx context.Context, configPath string, issue int, marker string) (factory.OnboardingProofObservation, error) {
	return newService(configPath).ObserveOnboardingProof(ctx, issue, marker)
}

// onboardingStatusDocument is the JSON result of onboard plan and onboard
// status: the plan identity, the current inputs, and the evaluated steps.
type onboardingStatusDocument struct {
	SchemaVersion int                         `json:"schema_version"`
	Plan          string                      `json:"plan"`
	Revision      int                         `json:"revision"`
	Repository    string                      `json:"repository"`
	HostConfig    string                      `json:"host_config"`
	Release       onboarding.Release          `json:"release"`
	Inputs        map[onboarding.Input]string `json:"inputs"`
	Artifacts     onboardingArtifacts         `json:"artifacts"`
	onboarding.Status
}

// onboardingArtifacts names what the onboarding leaves on the host, so an
// agent can find and remove it after an interruption.
type onboardingArtifacts struct {
	// Resources is the prepared resource directory the operator owns.
	Resources string `json:"resources,omitempty"`
	// ProofWorkspaces holds the workspaces of interrupted gate proofs, which
	// onboard verify --cleanup removes.
	ProofWorkspaces string `json:"proof_workspaces"`
	// LiveProofMarker is the marker of the started disposable-issue proof.
	LiveProofMarker string `json:"live_proof_marker,omitempty"`
}

// onboardingRecordDocument is the JSON result of onboard record.
type onboardingRecordDocument struct {
	SchemaVersion int             `json:"schema_version"`
	Step          onboarding.Step `json:"step"`
	// Reused reports that valid evidence already existed, so nothing ran.
	Reused bool `json:"reused,omitempty"`
	// Marker is the text the proof issue body must carry.
	Marker string `json:"marker,omitempty"`
	// Report is the gate proof report or the doctor document that produced
	// the evidence.
	Report any                      `json:"report,omitempty"`
	Status onboardingStatusDocument `json:"status"`
}

// runOnboardPlan creates or refreshes the plan at --output and writes its
// status. It writes only that file and its lock.
func runOnboardPlan(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("onboard plan", errorsOutput)
	repository := flags.String("repository", "", "Git checkout of the repository to onboard")
	destination := flags.String("output", "", "plan file outside the checkout, for example below $HOME/.cache/sw-factory")
	hostConfig := flags.String("config", "", "host configuration that registers the repository; defaults to the plan's earlier value, then to the default path")
	resources := flags.String("resources", "", "directory that onboard prepare filled; defaults to the plan's earlier value")
	asJSON := flags.Bool("json", false, "write the status as one schema-versioned JSON document; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return 2
	}
	if *repository == "" || *destination == "" || !*asJSON {
		writeError(errorsOutput, errors.New("onboard plan needs --repository <path>, --output <file>, and --json"))
		return 2
	}
	request, err := resolvePlanRequest(*repository, *destination, *hostConfig, *resources)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	observer, err := newOnboardingObserver()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	var observation onboarding.Observation
	plan, err := onboarding.Update(request.plan, func(plan *onboarding.Plan, exists bool) error {
		if exists && !samePlanRepository(plan.Repository, request.repository) {
			return fmt.Errorf("the plan %s belongs to another repository, %s: choose another --output", request.plan, plan.Repository)
		}
		if !exists {
			*plan = onboarding.NewPlan(request.repository, "", "")
		}
		plan.HostConfig = firstNonEmpty(request.hostConfig, plan.HostConfig, request.defaultHostConfig)
		plan.Resources = firstNonEmpty(request.resources, plan.Resources)
		observation = observer.Observe(ctx, *plan)
		plan.Refresh(observation)
		return nil
	})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeJSONDocument(output, errorsOutput, newOnboardingStatusDocument(request.plan, plan, observation)) {
		return 1
	}
	return 0
}

// planRequest holds the resolved paths of onboard plan.
type planRequest struct {
	repository, plan, hostConfig, defaultHostConfig, resources string
}

// resolvePlanRequest makes the paths absolute and refuses a plan inside the
// checkout, where it could be committed with the repository policy.
func resolvePlanRequest(repository, destination, hostConfig, resources string) (planRequest, error) {
	var request planRequest
	var err error
	for _, path := range []struct {
		value  string
		target *string
	}{{repository, &request.repository}, {destination, &request.plan}, {hostConfig, &request.hostConfig}, {resources, &request.resources}} {
		if path.value == "" {
			continue
		}
		if *path.target, err = filepath.Abs(path.value); err != nil {
			return planRequest{}, fmt.Errorf("resolve %s: %w", path.value, err)
		}
	}
	if info, err := os.Stat(request.repository); err != nil || !info.IsDir() {
		return planRequest{}, fmt.Errorf("the repository %s is not a directory", request.repository)
	}
	if inside(request.repository, request.plan) {
		return planRequest{}, errors.New("store the plan outside the repository checkout, for example below $HOME/.cache/sw-factory, so it is never committed")
	}
	if request.defaultHostConfig, err = config.DefaultHostConfigPath(); err != nil {
		return planRequest{}, err
	}
	return request, nil
}

// inside reports whether path lies in directory after symbolic links of
// their existing parents are resolved.
func inside(directory, path string) bool {
	resolvedDirectory := resolveExisting(directory)
	resolvedPath := resolveExisting(path)
	relative, err := filepath.Rel(resolvedDirectory, resolvedPath)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// resolveExisting resolves the symbolic links of the longest existing
// prefix of path and keeps the missing rest.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolveExisting(parent), filepath.Base(path))
}

// samePlanRepository reports whether two repository paths name the same
// checkout.
func samePlanRepository(first, second string) bool {
	return resolveExisting(first) == resolveExisting(second)
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// runOnboardStatus evaluates a plan against the current inputs. It writes
// nothing.
func runOnboardStatus(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("onboard status", errorsOutput)
	planPath := flags.String("plan", "", "plan file that onboard plan wrote")
	asJSON := flags.Bool("json", false, "write the status as one schema-versioned JSON document; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return 2
	}
	if *planPath == "" || !*asJSON {
		writeError(errorsOutput, errors.New("onboard status needs --plan <file> and --json"))
		return 2
	}
	path, plan, observation, err := observePlan(ctx, *planPath)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeJSONDocument(output, errorsOutput, newOnboardingStatusDocument(path, plan, observation)) {
		return 1
	}
	return 0
}

// observePlan loads a plan and observes its current inputs.
func observePlan(ctx context.Context, planPath string) (string, onboarding.Plan, onboarding.Observation, error) {
	path, err := filepath.Abs(planPath)
	if err != nil {
		return "", onboarding.Plan{}, onboarding.Observation{}, err
	}
	plan, err := onboarding.Load(path)
	if err != nil {
		return "", onboarding.Plan{}, onboarding.Observation{}, err
	}
	observer, err := newOnboardingObserver()
	if err != nil {
		return "", onboarding.Plan{}, onboarding.Observation{}, err
	}
	return path, plan, observer.Observe(ctx, plan), nil
}

// parseOnboardFlags parses flags and refuses positional arguments.
func parseOnboardFlags(flags *flag.FlagSet, args []string, errorsOutput io.Writer) bool {
	if err := flags.Parse(args); err != nil {
		return false
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, fmt.Errorf("%s does not accept positional arguments", flags.Name()))
		return false
	}
	return true
}

// newOnboardingObserver returns the observer of the running build.
func newOnboardingObserver() (onboarding.Observer, error) {
	identity, err := swfactory.ResourceIdentity()
	if err != nil {
		return onboarding.Observer{}, err
	}
	build := version.Current()
	return onboarding.Observer{
		Images:          onboardingImagesForCLI(),
		Release:         onboarding.Release{Version: build.Version, Revision: build.Revision, ResourceIdentity: identity},
		VerifyResources: swfactory.VerifyResources,
	}, nil
}

// newOnboardingStatusDocument evaluates a plan for output. The actions name
// the running factory executable and the plan path.
func newOnboardingStatusDocument(path string, plan onboarding.Plan, observation onboarding.Observation) onboardingStatusDocument {
	observation.PlanPath = path
	if command, err := factoryCommand(); err == nil {
		observation.FactoryCommand = command
	}
	proofs, _ := proofRoot()
	artifacts := onboardingArtifacts{Resources: plan.Resources, ProofWorkspaces: proofs}
	if plan.LiveProof != nil {
		artifacts.LiveProofMarker = plan.LiveProof.Marker
	}
	return onboardingStatusDocument{
		SchemaVersion: onboardingStatusSchema,
		Plan:          path,
		Revision:      plan.Revision,
		Repository:    plan.Repository,
		HostConfig:    plan.HostConfig,
		Release:       observation.Release,
		Inputs:        observation.Inputs,
		Artifacts:     artifacts,
		Status:        onboarding.Evaluate(plan, observation),
	}
}

// runOnboardRecord runs the supported observation of one step and records
// its evidence in the plan. Agent-authored text never becomes evidence.
func runOnboardRecord(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("onboard record", errorsOutput)
	planPath := flags.String("plan", "", "plan file that onboard plan wrote")
	asJSON := flags.Bool("json", false, "write the result as one schema-versioned JSON document; required here or after the step")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *planPath == "" || flags.NArg() == 0 {
		writeError(errorsOutput, errors.New("onboard record needs --plan <file> and a step: gate-proof, doctor, or live-proof"))
		return 2
	}
	step, rest := flags.Arg(0), flags.Args()[1:]
	if *asJSON {
		rest = append(rest, "--json")
	}
	switch step {
	case "gate-proof":
		return runRecordGateProof(ctx, *planPath, rest, output, errorsOutput)
	case "doctor":
		return runRecordDoctor(ctx, *planPath, rest, output, errorsOutput)
	case "live-proof":
		return runRecordLiveProof(ctx, *planPath, rest, output, errorsOutput)
	default:
		writeError(errorsOutput, fmt.Errorf("onboard record cannot record %q: the steps are gate-proof, doctor, and live-proof; the other steps are observed by onboard plan and onboard status", step))
		return 2
	}
}

// parseRecordJSON parses the flags of one record step and requires --json.
func parseRecordJSON(flags *flag.FlagSet, args []string, errorsOutput io.Writer) bool {
	asJSON := flags.Bool("json", false, "write the result as one schema-versioned JSON document; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return false
	}
	if !*asJSON {
		writeError(errorsOutput, fmt.Errorf("%s needs --json", flags.Name()))
		return false
	}
	return true
}

// runRecordGateProof proves the gates of the planned repository and records
// the result. Evidence that is still valid is reused without a new proof.
func runRecordGateProof(ctx context.Context, planPath string, args []string, output, errorsOutput io.Writer) int {
	if !parseRecordJSON(newFlagSet("onboard record gate-proof", errorsOutput), args, errorsOutput) {
		return 2
	}
	path, plan, observation, err := observePlan(ctx, planPath)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if plan.Valid(onboarding.StepGateProof, observation) {
		return writeRecord(output, errorsOutput, onboardingRecordDocument{Step: onboarding.StepGateProof, Reused: true}, path, plan, observation)
	}
	root, err := proofRoot()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	report := gateproof.Verifier{Runtime: proofRuntimeForCLI(), Root: root}.Verify(ctx, plan.Repository)
	if !inputsUnchanged(ctx, path, onboarding.StepGateProof, observation, errorsOutput) {
		writeJSONDocument(output, errorsOutput, report)
		return 1
	}
	evidence := onboarding.Evidence{Outcome: onboarding.OutcomeFailed, Inputs: observation.Subset(onboarding.StepGateProof), EvidenceID: report.EvidenceID, Findings: proofFindings(report)}
	if report.Result == gateproof.ResultPassed {
		evidence.Outcome = onboarding.OutcomePassed
	}
	return recordEvidence(ctx, path, onboarding.StepGateProof, evidence, report, output, errorsOutput)
}

// inputsUnchanged observes the plan again after a slow observation and
// reports whether every input of the step is still the one the observation
// started with. Evidence of changed inputs proves neither state, and a
// writer that started earlier must not replace newer evidence with it.
// Evidence of an input that could not be observed would stay valid while it
// cannot be observed, so it is refused too.
func inputsUnchanged(ctx context.Context, path string, step onboarding.Step, before onboarding.Observation, errorsOutput io.Writer) bool {
	_, _, after, err := observePlan(ctx, path)
	if err != nil {
		writeError(errorsOutput, err)
		return false
	}
	inputs := after.Subset(step)
	if !maps.Equal(before.Subset(step), inputs) {
		writeError(errorsOutput, fmt.Errorf("an input of %s changed while it ran, so nothing was recorded: run it again", step))
		return false
	}
	for input, identity := range inputs {
		if identity == "" {
			writeError(errorsOutput, fmt.Errorf("the input %s of %s could not be observed, so nothing was recorded; onboard status names the problem", input, step))
			return false
		}
	}
	return true
}

// proofFindings names the setup and the gates a failed proof did not
// accept.
func proofFindings(report gateproof.Report) []string {
	var findings []string
	if report.Setup != nil && report.Setup.Outcome != gateproof.OutcomePassed {
		findings = append(findings, "setup")
	}
	for _, gate := range report.Gates {
		if !gate.Accepted {
			findings = append(findings, "gate:"+gate.Name)
		}
	}
	if report.Result != gateproof.ResultPassed && len(findings) == 0 {
		findings = append(findings, "proof:"+string(report.Result))
	}
	return findings
}

// runRecordDoctor runs the startup diagnosis of the planned registration and
// records whether it is ready. It always runs, because the doctor also
// observes provider state that no plan input identifies.
func runRecordDoctor(ctx context.Context, planPath string, args []string, output, errorsOutput io.Writer) int {
	if !parseRecordJSON(newFlagSet("onboard record doctor", errorsOutput), args, errorsOutput) {
		return 2
	}
	path, plan, observation, err := observePlan(ctx, planPath)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	result, err := onboardingDoctorForCLI(ctx, plan.HostConfig)
	if err != nil {
		writeError(errorsOutput, fmt.Errorf("run the doctor: %w", err))
		return 1
	}
	if !inputsUnchanged(ctx, path, onboarding.StepDoctor, observation, errorsOutput) {
		return 1
	}
	evidence := onboarding.Evidence{Outcome: onboarding.OutcomeFailed, Inputs: observation.Subset(onboarding.StepDoctor)}
	for _, check := range result.Report.Results {
		if check.BlocksStartup() {
			evidence.Findings = append(evidence.Findings, check.Code)
		}
	}
	if result.Ready() {
		evidence.Outcome = onboarding.OutcomePassed
	}
	return recordEvidence(ctx, path, onboarding.StepDoctor, evidence, newDoctorDocument(result.Report, plan.HostConfig), output, errorsOutput)
}

// recordEvidence writes the evidence of one step and the resulting status.
// It exits 0 when the step's own evidence passed.
func recordEvidence(ctx context.Context, path string, step onboarding.Step, evidence onboarding.Evidence, report any, output, errorsOutput io.Writer) int {
	plan, err := onboarding.Update(path, func(plan *onboarding.Plan, exists bool) error {
		if !exists {
			return fmt.Errorf("the plan %s no longer exists", path)
		}
		plan.Record(step, evidence)
		return nil
	})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	_, _, observation, err := observePlan(ctx, path)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	status := writeRecord(output, errorsOutput, onboardingRecordDocument{Step: step, Report: report}, path, plan, observation)
	if status == 0 && evidence.Outcome != onboarding.OutcomePassed {
		return 1
	}
	return status
}

// writeRecord writes one record document with the evaluated plan.
func writeRecord(output, errorsOutput io.Writer, document onboardingRecordDocument, path string, plan onboarding.Plan, observation onboarding.Observation) int {
	document.SchemaVersion = onboardingStatusSchema
	document.Status = newOnboardingStatusDocument(path, plan, observation)
	if !writeJSONDocument(output, errorsOutput, document) {
		return 1
	}
	return 0
}

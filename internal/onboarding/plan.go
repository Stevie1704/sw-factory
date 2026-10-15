// Package onboarding persists the progress of one repository onboarding, so
// an agent that starts after an interruption can tell what is done, what is
// still valid, and what remains. The plan describes work. It is not a
// workflow controller, and it does not replace the coordinator's operational
// store: each step is done by its own supported command, and the plan only
// records evidence and evaluates it against the current inputs.
package onboarding

import (
	"fmt"
	"strings"
	"time"
)

// PlanSchema is the version of the persisted plan. Increment it when a field
// changes meaning or is removed.
const PlanSchema = 1

// Step names one onboarding step.
type Step string

// The onboarding steps, in procedure order.
const (
	StepPrepare       Step = "prepare"
	StepConfiguration Step = "configuration"
	StepImage         Step = "image"
	StepGateProof     Step = "gate_proof"
	StepRegistration  Step = "registration"
	StepSkillSmoke    Step = "skill_smoke"
	StepDoctor        Step = "doctor"
	StepLiveProof     Step = "live_proof"
)

// State is the evaluated state of one step.
type State string

const (
	// StatePending means the step has no valid evidence and can be done now.
	StatePending State = "pending"
	// StateBlocked means the step waits for another step, an unavailable
	// observation, or the reconciliation of an unknown outcome.
	StateBlocked State = "blocked"
	// StateVerified means valid evidence proves the step.
	StateVerified State = "verified"
	// StateStale means evidence exists, but an input it depends on changed.
	StateStale State = "stale"
	// StateSkipped means the operator declined the optional step.
	StateSkipped State = "skipped"
)

// Input names one identity that evidence can depend on.
type Input string

const (
	// InputRelease identifies the Factory build and its resources.
	InputRelease Input = "release"
	// InputPolicy identifies the content of factory.yaml.
	InputPolicy Input = "policy"
	// InputWorkerImage is the pinned worker image as IMAGE@DIGEST.
	InputWorkerImage Input = "worker_image"
	// InputWorkerDefinition identifies the worker build context, which holds
	// the worker definition and the vendored skill set.
	InputWorkerDefinition Input = "worker_definition"
	// InputGateInputs identifies the candidate files a gate proof tests.
	InputGateInputs Input = "gate_inputs"
	// InputRegistration identifies the host registration of the repository.
	InputRegistration Input = "registration"
	// InputSkillEvidence identifies the smoke records of the pinned image.
	InputSkillEvidence Input = "skill_evidence"
)

// Outcome is the recorded outcome of one observation.
type Outcome string

const (
	// OutcomePassed means the observation proved the step.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed means the observation ran and did not prove the step.
	OutcomeFailed Outcome = "failed"
)

// Result is the overall onboarding result.
type Result string

const (
	// ResultIncomplete means the doctor is not verified yet.
	ResultIncomplete Result = "incomplete"
	// ResultDoctorReady means the doctor is verified and the operator has not
	// decided the live proof yet.
	ResultDoctorReady Result = "doctor_ready"
	// ResultLiveProofPassed means the disposable-issue proof passed.
	ResultLiveProofPassed Result = "live_proof_passed"
	// ResultLiveProofDeclined means the doctor is verified and the operator
	// declined the disposable-issue proof.
	ResultLiveProofDeclined Result = "live_proof_declined"
)

// LiveProofState is the recorded state of the disposable-issue proof.
type LiveProofState string

const (
	// LiveProofStarted means the proof was started and its issue is not
	// observed yet, so the outcome of the issue creation is unknown.
	LiveProofStarted LiveProofState = "started"
	// LiveProofIssueRecorded means the proof issue is observed and its run
	// has not reached a draft pull request yet.
	LiveProofIssueRecorded LiveProofState = "issue_recorded"
	// LiveProofPassed means the run of the proof issue reached a draft pull
	// request.
	LiveProofPassed LiveProofState = "passed"
	// LiveProofDeclined means the operator declined the proof.
	LiveProofDeclined LiveProofState = "declined"
)

// Plan is the persisted onboarding record of one repository. It stores no
// credential, no credential path, and no harness transcript.
type Plan struct {
	// SchemaVersion is PlanSchema.
	SchemaVersion int `json:"schema_version"`
	// Revision increases with each write, so a reader can tell two states
	// apart.
	Revision int `json:"revision"`
	// Repository is the absolute path of the target checkout.
	Repository string `json:"repository"`
	// HostConfig is the host configuration that registers the repository.
	HostConfig string `json:"host_config"`
	// Resources is the prepared release resource directory, or empty.
	Resources string `json:"resources,omitempty"`
	// AuthorizationNote documents the operator's stated session scope and
	// constraints. It grants no authority, supplies no step evidence, and
	// must contain no secrets, credential paths, or transcripts.
	AuthorizationNote string `json:"authorization_note,omitempty"`
	// Evidence holds the recorded observations by step.
	Evidence map[Step]Evidence `json:"evidence"`
	// LiveProof is the externally mutating disposable-issue proof, or nil.
	LiveProof *LiveProof `json:"live_proof,omitempty"`
}

// Evidence is one recorded observation and the input identities it depends
// on. A change of one of those inputs makes it stale.
type Evidence struct {
	// Outcome tells whether the observation passed.
	Outcome Outcome `json:"outcome"`
	// Inputs are the identities the observation depended on.
	Inputs map[Input]string `json:"inputs"`
	// RecordedAt is the RFC 3339 instant at which the observation started.
	RecordedAt string `json:"recorded_at"`
	// EvidenceID is the gate proof evidence identity, when there is one.
	EvidenceID string `json:"evidence_id,omitempty"`
	// Findings are the stable codes of the blocking doctor findings or the
	// failed gates.
	Findings []string `json:"findings,omitempty"`
}

// LiveProof records the identity and outcome of the disposable-issue proof.
// The marker is recorded before the issue exists, so a lost response can be
// reconciled by finding the issue that carries it.
type LiveProof struct {
	// State is the recorded proof state.
	State LiveProofState `json:"state"`
	// Marker is the unique text the proof issue body carries.
	Marker string `json:"marker,omitempty"`
	// Issue is the observed proof issue number, or 0.
	Issue int `json:"issue,omitempty"`
	// RunStage and RunStatus are the last observed state of its run.
	RunStage  string `json:"run_stage,omitempty"`
	RunStatus string `json:"run_status,omitempty"`
	// Inputs are the identities at the start of the proof, which a passed
	// proof depends on.
	Inputs map[Input]string `json:"inputs,omitempty"`
	// UpdatedAt is the RFC 3339 instant of the last change.
	UpdatedAt string `json:"updated_at"`
}

// NewPlan returns an empty plan for one repository.
func NewPlan(repository, hostConfig, resources string) Plan {
	return Plan{SchemaVersion: PlanSchema, Repository: repository, HostConfig: hostConfig, Resources: resources, Evidence: map[Step]Evidence{}}
}

// Record replaces the evidence of one step and leaves every other step
// unchanged.
func (p *Plan) Record(step Step, evidence Evidence) {
	if p.Evidence == nil {
		p.Evidence = map[Step]Evidence{}
	}
	if evidence.RecordedAt == "" {
		evidence.RecordedAt = now()
	}
	p.Evidence[step] = evidence
}

// now returns the current instant in RFC 3339 form.
func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// Check is the result of one cheap local observation.
type Check struct {
	// Done reports that the observation proves the step.
	Done bool
	// Unobserved reports that the observation could not be made, for example
	// because Docker is not reachable.
	Unobserved bool
	// Problem tells why the step is not done.
	Problem string
}

// HarnessSmoke is the recorded skill smoke coverage of one harness that the
// repository policy can select.
type HarnessSmoke struct {
	Harness        string   `json:"harness"`
	HarnessVersion string   `json:"harness_version,omitempty"`
	Required       []string `json:"required"`
	Verified       []string `json:"verified"`
	Missing        []string `json:"missing"`
}

// Observation is the current state of the inputs and of the cheap, locally
// observed steps. Observing never writes.
type Observation struct {
	// Inputs are the current input identities. A missing input is empty.
	Inputs map[Input]string
	// Release names the running Factory build.
	Release Release
	// FactoryCommand is the absolute path of the running factory executable,
	// and PlanPath the path of the plan; actions name both.
	FactoryCommand string
	PlanPath       string
	Resources      Check
	Configuration  Check
	Image          Check
	Registration   Check
	// Smoke lists each harness the policy can select.
	Smoke []HarnessSmoke
}

// Release names one Factory build and its resources.
type Release struct {
	Version          string `json:"factory_version"`
	Revision         string `json:"factory_revision"`
	ResourceIdentity string `json:"resource_identity"`
}

// stepDefinition is the static description of one step.
type stepDefinition struct {
	name      Step
	dependsOn []Step
	// inputs are the identities the step's evidence depends on.
	inputs []Input
	// choice names the operator decision the step needs, if any.
	choice string
}

// steps lists every step in procedure order.
var steps = []stepDefinition{
	{name: StepPrepare},
	{name: StepConfiguration, dependsOn: []Step{StepPrepare}, inputs: []Input{InputRelease, InputPolicy},
		choice: "the optional roles, and the harness, models, and reasoning effort of each role"},
	{name: StepImage, dependsOn: []Step{StepConfiguration}, inputs: []Input{InputWorkerImage, InputWorkerDefinition}},
	{name: StepGateProof, dependsOn: []Step{StepImage}, inputs: []Input{InputRelease, InputPolicy, InputWorkerImage, InputGateInputs}},
	{name: StepRegistration, dependsOn: []Step{StepConfiguration}, inputs: []Input{InputRegistration},
		choice: "approval of factory init, factory register, and factory bootstrap-labels, the host configuration path, and the credential paths"},
	{name: StepSkillSmoke, dependsOn: []Step{StepImage}, inputs: []Input{InputWorkerImage, InputSkillEvidence},
		choice: "approval of the paid smoke calls and of any Keychain credential export"},
	{name: StepDoctor, dependsOn: []Step{StepGateProof, StepRegistration, StepSkillSmoke}, inputs: []Input{InputRelease, InputPolicy, InputWorkerImage, InputRegistration, InputSkillEvidence}},
	{name: StepLiveProof, dependsOn: []Step{StepDoctor}, inputs: []Input{InputRelease, InputPolicy, InputWorkerImage, InputRegistration},
		choice: "approval or refusal of the disposable-issue proof, which creates an issue, a branch, and a draft pull request and starts paid agent sessions"},
}

// definition returns the static description of one step.
func definition(name Step) stepDefinition {
	for _, step := range steps {
		if step.name == name {
			return step
		}
	}
	panic("unknown onboarding step " + string(name))
}

// Subset returns the current identities that the evidence of one step
// depends on.
func (o Observation) Subset(step Step) map[Input]string {
	subset := map[Input]string{}
	for _, input := range definition(step).inputs {
		subset[input] = o.Inputs[input]
	}
	return subset
}

// StepStatus is the evaluated state of one step.
type StepStatus struct {
	Name           Step           `json:"name"`
	State          State          `json:"state"`
	DependsOn      []Step         `json:"depends_on"`
	Reason         string         `json:"reason,omitempty"`
	Action         string         `json:"action,omitempty"`
	OperatorChoice string         `json:"operator_choice,omitempty"`
	Evidence       *Evidence      `json:"evidence,omitempty"`
	Harnesses      []HarnessSmoke `json:"harnesses,omitempty"`
	LiveProof      *LiveProof     `json:"live_proof,omitempty"`
}

// Status is the evaluation of one plan against one observation.
type Status struct {
	Result Result       `json:"result"`
	Steps  []StepStatus `json:"steps"`
}

// Evaluate derives the state of every step from the recorded evidence and
// the current observation. A step whose dependency is not verified or
// skipped is blocked, but its evidence stays in the plan and is reported.
func Evaluate(plan Plan, observation Observation) Status {
	evaluator := evaluator{plan: plan, observation: observation, states: map[Step]State{}}
	status := Status{Steps: make([]StepStatus, 0, len(steps))}
	for _, step := range steps {
		evaluated := evaluator.evaluate(step)
		evaluator.states[step.name] = evaluated.State
		status.Steps = append(status.Steps, evaluated)
	}
	status.Result = result(evaluator.states)
	return status
}

// result derives the overall onboarding result from the step states.
func result(states map[Step]State) Result {
	if states[StepDoctor] != StateVerified {
		return ResultIncomplete
	}
	switch states[StepLiveProof] {
	case StateVerified:
		return ResultLiveProofPassed
	case StateSkipped:
		return ResultLiveProofDeclined
	default:
		return ResultDoctorReady
	}
}

// evaluator holds the inputs of one evaluation and the states derived so
// far.
type evaluator struct {
	plan        Plan
	observation Observation
	states      map[Step]State
}

// evaluate derives one step's state from its own evidence, then applies its
// dependencies.
func (e evaluator) evaluate(step stepDefinition) StepStatus {
	status := StepStatus{Name: step.name, DependsOn: append([]Step{}, step.dependsOn...), OperatorChoice: step.choice}
	e.own(&status)
	if status.State == StateVerified || status.State == StatePending {
		if waiting := e.unmet(step); len(waiting) != 0 {
			status.State = StateBlocked
			status.Reason = "waiting for " + strings.Join(waiting, ", ")
			status.Action = ""
		}
	}
	if status.State == StateVerified {
		status.OperatorChoice = ""
	}
	return status
}

// unmet returns the dependencies that are neither verified nor skipped.
func (e evaluator) unmet(step stepDefinition) []string {
	var waiting []string
	for _, dependency := range step.dependsOn {
		if state := e.states[dependency]; state != StateVerified && state != StateSkipped {
			waiting = append(waiting, string(dependency))
		}
	}
	return waiting
}

// own derives the state that a step's own evidence supports.
func (e evaluator) own(status *StepStatus) {
	switch status.Name {
	case StepPrepare:
		e.prepare(status)
	case StepConfiguration:
		applyCheck(status, e.observation.Configuration, "write factory.yaml as Step 4 of the procedure describes, then check it with "+e.command()+" config validate --file "+quote(e.plan.Repository+"/factory.yaml")+" --json")
	case StepImage:
		e.image(status)
	case StepGateProof:
		e.recorded(status, "run "+e.command()+" onboard record --plan "+quote(e.observation.PlanPath)+" gate-proof --json")
	case StepRegistration:
		applyCheck(status, e.observation.Registration, "within explicit session authority for this registration, register as Step 6 of the procedure describes, then run onboard plan again with the same --config; request missing scope per factory guide authorization")
	case StepSkillSmoke:
		e.smoke(status)
	case StepDoctor:
		e.recorded(status, "run "+e.command()+" onboard record --plan "+quote(e.observation.PlanPath)+" doctor --json")
	case StepLiveProof:
		e.liveProof(status)
	}
}

// command returns the factory command that actions name.
func (e evaluator) command() string {
	if e.observation.FactoryCommand == "" {
		return "factory"
	}
	return quote(e.observation.FactoryCommand)
}

// quote returns a value that a POSIX shell reads as one word.
func quote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-@:=+", r))
	}) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// applyCheck sets the state of a step that a cheap local observation decides.
func applyCheck(status *StepStatus, check Check, action string) {
	switch {
	case check.Done:
		status.State = StateVerified
	case check.Unobserved:
		status.State, status.Reason = StateBlocked, check.Problem
	default:
		status.State, status.Reason, status.Action = StatePending, check.Problem, action
	}
}

// prepare evaluates the prepared resource directory.
func (e evaluator) prepare(status *StepStatus) {
	action := "run " + e.command() + " onboard prepare --output <new directory below $HOME> --json, then run onboard plan again with --resources <that directory>"
	if e.plan.Resources == "" {
		status.State, status.Reason, status.Action = StatePending, "the plan names no prepared resource directory", action
		return
	}
	check := e.observation.Resources
	if !check.Done {
		status.State, status.Reason, status.Action = StatePending, check.Problem, action
		return
	}
	status.State = StateVerified
}

// image evaluates the pinned worker image and the build context it was
// observed with.
func (e evaluator) image(status *StepStatus) {
	applyCheck(status, e.observation.Image, "build the worker image and record its digest in factory.yaml as Step 3 of the procedure describes")
	if status.State != StateVerified {
		return
	}
	evidence, ok := e.plan.Evidence[StepImage]
	if !ok {
		return
	}
	status.Evidence = &evidence
	if changed := e.changed(StepImage, evidence.Inputs); len(changed) != 0 {
		status.State = StateStale
		status.Reason = describeChanges(changed) + " since the image was observed"
		status.Action = "rebuild the worker image, record the new digest in factory.yaml, and run onboard plan again"
	}
}

// recorded evaluates a step whose evidence a record command stores.
func (e evaluator) recorded(status *StepStatus, action string) {
	evidence, ok := e.plan.Evidence[status.Name]
	if !ok {
		status.State, status.Reason, status.Action = StatePending, "no evidence is recorded", action
		return
	}
	status.Evidence = &evidence
	if changed := e.changed(status.Name, evidence.Inputs); len(changed) != 0 {
		status.State, status.Reason, status.Action = StateStale, describeChanges(changed)+" since the evidence was recorded", action
		return
	}
	if evidence.Outcome != OutcomePassed {
		reason := "the recorded observation failed"
		if len(evidence.Findings) != 0 {
			reason += ": " + strings.Join(evidence.Findings, ", ")
		}
		status.State, status.Reason, status.Action = StatePending, reason, "repair the findings, then "+action
		return
	}
	status.State = StateVerified
}

// changed returns the inputs of one step whose recorded identity differs
// from the current one, in definition order.
func (e evaluator) changed(step Step, recorded map[Input]string) []Input {
	var changed []Input
	for _, input := range definition(step).inputs {
		if recorded[input] != e.observation.Inputs[input] {
			changed = append(changed, input)
		}
	}
	return changed
}

// describeChanges names the changed inputs, for example "policy changed,
// gate_inputs changed".
func describeChanges(changed []Input) string {
	names := make([]string, len(changed))
	for index, input := range changed {
		names[index] = string(input) + " changed"
	}
	return strings.Join(names, ", ")
}

// smoke evaluates the recorded skill smoke of each selectable harness.
func (e evaluator) smoke(status *StepStatus) {
	status.Harnesses = e.observation.Smoke
	if len(e.observation.Smoke) == 0 {
		status.State, status.Reason = StateBlocked, "the harnesses the policy selects are not known"
		return
	}
	var retries []string
	for _, smoke := range e.observation.Smoke {
		if len(smoke.Missing) != 0 {
			retries = append(retries, e.smokeCommand(smoke))
		}
	}
	if len(retries) == 0 {
		status.State = StateVerified
		return
	}
	status.State = StatePending
	status.Reason = "the recorded smoke does not cover every required skill of the pinned image " + e.observation.Inputs[InputWorkerImage] + "; recorded skills are kept"
	status.Action = "within explicit session authority and supplied limits for these paid smoke calls, run: " + strings.Join(retries, "; ") + "; request missing scope per factory guide authorization"
}

// smokeCommand returns the smoke script call that repeats only the missing
// skills of one harness.
func (e evaluator) smokeCommand(smoke HarnessSmoke) string {
	image, digest, _ := strings.Cut(e.observation.Inputs[InputWorkerImage], "@")
	repository := e.plan.Repository
	return fmt.Sprintf("cd %s && EVIDENCE_FILE=%s WORKER_IMAGE=%s WORKER_DIGEST=%s HARNESSES=%s MANDATORY_SKILLS=%s %s",
		quote(repository), quote(repository+"/worker/skill-smoke.json"), quote(image), quote(digest), quote(smoke.Harness),
		quote(strings.Join(smoke.Missing, " ")), quote(e.plan.Resources+"/scripts/smoke-skills.sh"))
}

// liveProof evaluates the disposable-issue proof.
func (e evaluator) liveProof(status *StepStatus) {
	record := "run " + e.command() + " onboard record --plan " + quote(e.observation.PlanPath) + " live-proof"
	proof := e.plan.LiveProof
	if proof == nil {
		status.State = StatePending
		status.Reason = "no disposable-issue proof decision is recorded"
		status.Action = "use the operator's explicit session decision, or request the missing decision per factory guide authorization; " + record + " --start --json before you create the issue, or " + record + " --decline --json"
		return
	}
	status.LiveProof = proof
	switch proof.State {
	case LiveProofDeclined:
		status.State, status.Reason = StateSkipped, "the operator declined the disposable-issue proof"
	case LiveProofStarted:
		status.State = StateBlocked
		status.Reason = "the outcome of the proof issue creation is unknown: an issue whose body carries " + proof.Marker + " can already exist"
		status.Action = "find the issue whose body carries the marker and " + record + " --issue <number> --json; do not create another issue. Only when the operator confirms that no such issue exists, " + record + " --abandon --json"
	case LiveProofIssueRecorded:
		status.State = StatePending
		status.Reason = fmt.Sprintf("the run of issue #%d has not reached a draft pull request (stage %q, status %q)", proof.Issue, proof.RunStage, proof.RunStatus)
		status.Action = fmt.Sprintf("follow the run, then %s --issue %d --json", record, proof.Issue)
	case LiveProofPassed:
		if changed := e.changed(StepLiveProof, proof.Inputs); len(changed) != 0 {
			status.State, status.Reason = StateStale, describeChanges(changed)+" since the proof passed"
			status.Action = "a new paid proof needs explicit session scope for the changed inputs; request missing scope per factory guide authorization; " + record + " --start --json, or " + record + " --decline --json with an explicit operator decision"
			return
		}
		status.State = StateVerified
	}
}

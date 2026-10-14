package gateproof_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gateproof"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// testDigest is the worker digest the candidate factory.yaml pins.
const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// proofConfig is a candidate factory.yaml with setup, an advisory gate, and a
// blocking gate that depends on nothing.
const proofConfig = `schema_version: 1
target_branch: main
setup: npm ci
setup_environment_policy: clean
gates:
  - name: lint
    command: npm run lint
    timeout: 5m
    blocking: false
    environment_policy: clean
  - name: test
    command: npm test
    timeout: 5m
    blocking: true
    environment_policy: role
role_harness_defaults:
  implementation: claude
model_options:
  implementation: [claude-opus-5]
timeouts:
  setup: 5m
  agent: 90m
  gate: 15m
  review: 30m
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: advisory
caches:
  - name: npm
    read_only: false
worker_build:
  image: ghcr.io/example/project-worker
  digest: ` + testDigest + `
  definition: worker/Dockerfile
base_synchronization:
  mode: never
`

// commandScript answers one worker command.
type commandScript func(ctx context.Context, request worker.CommandRequest) (worker.CommandResult, error)

// fakeRuntime is a worker runtime that records requests and answers commands
// from a script. Unscripted commands pass.
type fakeRuntime struct {
	mu       sync.Mutex
	scripts  map[string]commandScript
	startErr error
	started  []worker.StartRequest
	commands []worker.CommandRequest
	cleaned  []worker.CleanupRequest
	// projected records README.md as the worker saw it during setup.
	projected string
	// inspect runs once, at the first command, with the start request.
	inspect func(worker.StartRequest)
}

// Start records the request.
func (f *fakeRuntime) Start(_ context.Context, request worker.StartRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, request)
	return f.startErr
}

// Resume is not used by a proof.
func (f *fakeRuntime) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand records the request and runs its script.
func (f *fakeRuntime) RunCommand(ctx context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	f.mu.Lock()
	f.commands = append(f.commands, request)
	script := f.scripts[request.Command]
	first := len(f.started) > 0 && f.projected == ""
	if first {
		data, _ := os.ReadFile(filepath.Join(f.started[0].WorktreePath, "README.md"))
		f.projected = string(data)
	}
	f.mu.Unlock()
	if script != nil {
		result, err := script(ctx, request)
		if first && f.inspect != nil {
			f.inspect(f.started[0])
		}
		return result, err
	}
	if script == nil {
		return worker.CommandResult{Stdout: "ok\n"}, nil
	}
	return script(ctx, request)
}

// Stop is not used by a proof.
func (f *fakeRuntime) Stop(context.Context, string) error { return nil }

// Inspect reports a running worker.
func (f *fakeRuntime) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// Cleanup records the request.
func (f *fakeRuntime) Cleanup(_ context.Context, request worker.CleanupRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleaned = append(f.cleaned, request)
	return nil
}

// proofRepository creates a candidate checkout whose uncommitted factory.yaml
// is contents.
func proofRepository(t *testing.T, contents string) string {
	t.Helper()
	repository := candidateRepository(t)
	writeFile(t, repository, "factory.yaml", contents, 0o644)
	return repository
}

// gateByName returns the named gate report.
func gateByName(t *testing.T, report gateproof.Report, name string) gateproof.GateReport {
	t.Helper()
	for _, gate := range report.Gates {
		if gate.Name == name {
			return gate
		}
	}
	t.Fatalf("report has no gate %q: %#v", name, report.Gates)
	return gateproof.GateReport{}
}

// TestVerifyProvesTheCandidateInAnIsolatedWorker verifies a passing proof
// runs setup and every gate in the pinned worker against a disposable copy of
// the candidate, reports its identity, and removes what it created.
func TestVerifyProvesTheCandidateInAnIsolatedWorker(t *testing.T) {
	repository := proofRepository(t, proofConfig)
	before := checkoutState(t, repository)
	root := t.TempDir()
	runtime := &fakeRuntime{}

	report := gateproof.Verifier{Runtime: runtime, Root: root}.Verify(context.Background(), repository)

	if report.Result != gateproof.ResultPassed {
		t.Fatalf("Result = %q, want passed: %#v", report.Result, report)
	}
	if len(runtime.started) != 1 {
		t.Fatalf("started %d workers, want 1", len(runtime.started))
	}
	start := runtime.started[0]
	if !strings.HasPrefix(start.WorktreePath, root) || !strings.HasPrefix(start.GitMetadataPath, root) {
		t.Errorf("worker mounts %q and %q, want both below the proof root %q", start.WorktreePath, start.GitMetadataPath, root)
	}
	if start.Image != "ghcr.io/example/project-worker" || start.ImageDigest != testDigest || start.Role != "gate" {
		t.Errorf("start request = %#v, want the pinned gate worker", start)
	}
	if start.Limits != config.EffectiveWorkerLimits(config.WorkerLimitsConfig{}) {
		t.Errorf("limits = %#v, want the production defaults", start.Limits)
	}
	if len(start.Caches) != 1 || start.Caches[0].Name != "npm" || !strings.HasPrefix(start.Caches[0].HostPath, root) {
		t.Errorf("caches = %#v, want one disposable npm cache below the proof root", start.Caches)
	}
	if runtime.projected != "changed by onboarding\n" {
		t.Errorf("worker saw README.md = %q, want the uncommitted candidate", runtime.projected)
	}
	wantCommands := []struct{ command, role string }{{"npm ci", "coordinator"}, {"npm run lint", "gate"}, {"npm test", "gate"}}
	for index, want := range wantCommands {
		if index >= len(runtime.commands) || runtime.commands[index].Command != want.command || runtime.commands[index].Role != want.role {
			t.Fatalf("commands = %#v, want %v first", runtime.commands, wantCommands)
		}
	}
	if runtime.commands[2].EnvironmentPolicy != worker.EnvironmentPolicyRole {
		t.Errorf("test gate policy = %q, want role", runtime.commands[2].EnvironmentPolicy)
	}
	if report.Setup == nil || report.Setup.Outcome != gateproof.OutcomePassed {
		t.Errorf("setup = %#v, want passed", report.Setup)
	}
	if gate := gateByName(t, report, "test"); gate.Outcome != gateproof.OutcomePassed || !gate.Accepted || !gate.Blocking {
		t.Errorf("test gate = %#v, want an accepted blocking pass", gate)
	}
	if report.ImageReference != "ghcr.io/example/project-worker@"+testDigest || report.Candidate == nil || report.EvidenceID == "" {
		t.Errorf("report identity = %q %#v %q, want image, candidate, and evidence", report.ImageReference, report.Candidate, report.EvidenceID)
	}
	if len(runtime.cleaned) != 1 || runtime.cleaned[0].RunID != start.RunID || report.Cleanup.Status != gateproof.CleanupRemoved {
		t.Errorf("cleanup = %#v, report %#v; want the proof worker removed", runtime.cleaned, report.Cleanup)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("proof root holds %v (%v), want it empty after the proof", entries, err)
	}
	if after := checkoutState(t, repository); after != before {
		t.Errorf("proof changed the live checkout:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// exitWith answers a command with an exit code and stderr.
func exitWith(code int, stderr string) commandScript {
	return func(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
		return worker.CommandResult{ExitCode: code, Stderr: stderr}, nil
	}
}

// failWith answers a command with a worker error.
func failWith(err error) commandScript {
	return func(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
		return worker.CommandResult{}, err
	}
}

// verify runs one proof of contents with the scripted commands.
func verify(t *testing.T, ctx context.Context, contents string, scripts map[string]commandScript) (gateproof.Report, *fakeRuntime) {
	t.Helper()
	runtime := &fakeRuntime{scripts: scripts}
	report := gateproof.Verifier{Runtime: runtime, Root: t.TempDir()}.Verify(ctx, proofRepository(t, contents))
	return report, runtime
}

// dependentConfig makes the blocking test gate depend on the advisory lint gate.
var dependentConfig = strings.Replace(proofConfig, "    command: npm test\n", "    command: npm test\n    depends_on: [lint]\n", 1)

// TestVerifyAppliesTheProductionGateRules verifies advisory failures, dependency
// skips, setup failures, and worker errors get the outcomes final readiness
// would give them.
func TestVerifyAppliesTheProductionGateRules(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		scripts map[string]commandScript
		result  gateproof.Result
		setup   gateproof.Outcome
		lint    gateproof.Outcome
		test    gateproof.Outcome
		lintOK  bool
		testOK  bool
	}{
		{
			name:    "advisory failure is accepted",
			config:  proofConfig,
			scripts: map[string]commandScript{"npm run lint": exitWith(1, "lint error")},
			result:  gateproof.ResultPassed, setup: gateproof.OutcomePassed,
			lint: gateproof.OutcomeFailed, lintOK: true, test: gateproof.OutcomePassed, testOK: true,
		},
		{
			name:    "blocking gate skipped after an advisory failure blocks",
			config:  dependentConfig,
			scripts: map[string]commandScript{"npm run lint": exitWith(1, "lint error")},
			result:  gateproof.ResultFailed, setup: gateproof.OutcomePassed,
			lint: gateproof.OutcomeFailed, lintOK: true, test: gateproof.OutcomeSkipped,
		},
		{
			name:    "setup failure",
			config:  proofConfig,
			scripts: map[string]commandScript{"npm ci": exitWith(1, "npm ERR!")},
			result:  gateproof.ResultFailed, setup: gateproof.OutcomeFailed,
			lint: gateproof.OutcomeSetupFailed, test: gateproof.OutcomeSetupFailed,
		},
		{
			name:    "capture overflow blocks even an advisory gate",
			config:  proofConfig,
			scripts: map[string]commandScript{"npm run lint": failWith(&worker.OutputLimitExceededError{Operation: "exec", Stream: "stdout", Limit: 8})},
			result:  gateproof.ResultFailed, setup: gateproof.OutcomePassed,
			lint: gateproof.OutcomeOutputLimitExceeded, test: gateproof.OutcomePassed, testOK: true,
		},
		{
			name:    "out of memory",
			config:  proofConfig,
			scripts: map[string]commandScript{"npm test": failWith(&worker.OutOfMemoryError{})},
			result:  gateproof.ResultFailed, setup: gateproof.OutcomePassed,
			lint: gateproof.OutcomePassed, lintOK: true, test: gateproof.OutcomeOutOfMemory,
		},
		{
			name:    "unconfirmed termination halts the suite",
			config:  proofConfig,
			scripts: map[string]commandScript{"npm run lint": failWith(&worker.CommandTerminationError{Reason: "still running"})},
			result:  gateproof.ResultFailed, setup: gateproof.OutcomePassed,
			lint: gateproof.OutcomeTerminationUnconfirmed, test: gateproof.OutcomeSkipped,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, _ := verify(t, context.Background(), test.config, test.scripts)

			if report.Result != test.result {
				t.Errorf("Result = %q, want %q", report.Result, test.result)
			}
			if report.Setup == nil || report.Setup.Outcome != test.setup {
				t.Errorf("setup = %#v, want %q", report.Setup, test.setup)
			}
			lint, gate := gateByName(t, report, "lint"), gateByName(t, report, "test")
			if lint.Outcome != test.lint || lint.Accepted != test.lintOK {
				t.Errorf("lint = %q accepted %v, want %q accepted %v", lint.Outcome, lint.Accepted, test.lint, test.lintOK)
			}
			if gate.Outcome != test.test || gate.Accepted != test.testOK {
				t.Errorf("test = %q accepted %v, want %q accepted %v", gate.Outcome, gate.Accepted, test.test, test.testOK)
			}
		})
	}
}

// TestVerifyReportsATimedOutGate verifies a gate that outlives its declared
// timeout is reported as timed out and blocks when the gate is blocking.
func TestVerifyReportsATimedOutGate(t *testing.T) {
	config := strings.Replace(proofConfig, "    command: npm test\n    timeout: 5m", "    command: npm test\n    timeout: 20ms", 1)
	block := func(ctx context.Context, _ worker.CommandRequest) (worker.CommandResult, error) {
		<-ctx.Done()
		return worker.CommandResult{}, ctx.Err()
	}

	report, _ := verify(t, context.Background(), config, map[string]commandScript{"npm test": block})

	if gate := gateByName(t, report, "test"); gate.Outcome != gateproof.OutcomeTimedOut || gate.Accepted {
		t.Fatalf("test gate = %#v, want a refused timeout", gate)
	}
	if report.Result != gateproof.ResultFailed {
		t.Fatalf("Result = %q, want failed", report.Result)
	}
}

// TestVerifyCleansUpAfterAnInterruption verifies a cancelled proof reports
// the interruption and still removes its worker and workspace.
func TestVerifyCleansUpAfterAnInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupt := func(ctx context.Context, _ worker.CommandRequest) (worker.CommandResult, error) {
		cancel()
		return worker.CommandResult{}, ctx.Err()
	}

	report, runtime := verify(t, ctx, proofConfig, map[string]commandScript{"npm ci": interrupt})

	if report.Result != gateproof.ResultInterrupted || report.Setup == nil || report.Setup.Outcome != gateproof.OutcomeInterrupted {
		t.Fatalf("report = %#v, want an interrupted setup", report)
	}
	if len(runtime.cleaned) != 1 || report.Cleanup.Status != gateproof.CleanupRemoved {
		t.Fatalf("cleanup = %#v, %#v; want the worker removed after the interruption", runtime.cleaned, report.Cleanup)
	}
}

// TestVerifyCannotPassWithAHostOnlyTool verifies every command runs in the
// worker: a tool that exists only on the host does not make a gate pass.
func TestVerifyCannotPassWithAHostOnlyTool(t *testing.T) {
	bin := t.TempDir()
	writeFile(t, bin, "npm", "#!/bin/sh\nexit 0\n", 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	notInWorker := exitWith(127, "/bin/sh: npm: not found")

	report, _ := verify(t, context.Background(), proofConfig, map[string]commandScript{"npm ci": notInWorker})

	if report.Result != gateproof.ResultFailed || report.Setup.Outcome != gateproof.OutcomeFailed || report.Setup.ExitCode != 127 {
		t.Fatalf("report = %#v, want the worker's missing npm to fail setup", report)
	}
}

// TestVerifyEvidenceFollowsCandidateAndWorker verifies a candidate file
// change, a gate change, and a worker digest change each give new evidence.
func TestVerifyEvidenceFollowsCandidateAndWorker(t *testing.T) {
	repository := proofRepository(t, proofConfig)
	evidence := func() string {
		t.Helper()
		report := gateproof.Verifier{Runtime: &fakeRuntime{}, Root: t.TempDir()}.Verify(context.Background(), repository)
		if report.EvidenceID == "" {
			t.Fatalf("report has no evidence: %#v", report)
		}
		return report.EvidenceID
	}

	original := evidence()
	if again := evidence(); again != original {
		t.Fatalf("evidence changed without a change: %s then %s", original, again)
	}
	seen := map[string]string{original: "original"}
	for name, change := range map[string]func(){
		"candidate file": func() { writeFile(t, repository, "src/new.js", "new\n", 0o644) },
		"gate": func() {
			writeFile(t, repository, "factory.yaml", strings.Replace(proofConfig, "npm run lint", "npm run lint -- --strict", 1), 0o644)
		},
		"worker digest": func() {
			writeFile(t, repository, "factory.yaml", strings.ReplaceAll(proofConfig, testDigest, "sha256:"+strings.Repeat("2", 64)), 0o644)
		},
	} {
		change()
		got := evidence()
		if previous, exists := seen[got]; exists {
			t.Fatalf("%s change kept the evidence of %s", name, previous)
		}
		seen[got] = name
	}
}

// TestVerifyIsUnavailableWithoutAValidPolicyOrWorker verifies a proof that
// cannot run says so and starts or leaves nothing.
func TestVerifyIsUnavailableWithoutAValidPolicyOrWorker(t *testing.T) {
	t.Run("invalid policy", func(t *testing.T) {
		report, runtime := verify(t, context.Background(), strings.Replace(proofConfig, "timeout: 5m", "timeout: soon", 1), nil)
		if report.Result != gateproof.ResultUnavailable || len(runtime.started) != 0 || len(report.Diagnostics) == 0 {
			t.Fatalf("report = %#v, started %d; want an unavailable proof without a worker", report, len(runtime.started))
		}
		if !strings.Contains(report.Diagnostics[0], "gates[0].timeout") {
			t.Fatalf("diagnostics = %v, want the invalid field", report.Diagnostics)
		}
	})
	t.Run("worker does not start", func(t *testing.T) {
		root := t.TempDir()
		runtime := &fakeRuntime{startErr: os.ErrPermission}
		report := gateproof.Verifier{Runtime: runtime, Root: root}.Verify(context.Background(), proofRepository(t, proofConfig))
		if report.Result != gateproof.ResultUnavailable || report.Cleanup.Status != gateproof.CleanupRemoved {
			t.Fatalf("report = %#v, want an unavailable proof that cleaned up", report)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Fatalf("proof root holds %v, want it empty", entries)
		}
	})
}

// TestCleanupRemovesLeftoversButNotARunningProof verifies leftover proof
// workspaces and workers are removed, while a workspace whose proof still
// holds its lock stays.
func TestCleanupRemovesLeftoversButNotARunningProof(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "onboard-proof-0000000000000000")
	writeFile(t, stale, "work/node_modules/x.js", "left behind\n", 0o644)
	writeFile(t, root, "unrelated/keep.txt", "not a proof\n", 0o644)
	unpublished := filepath.Join(root, ".onboard-proof-1111111111111111")
	writeFile(t, unpublished, "work/README.md", "killed before publication\n", 0o644)
	runtime := &fakeRuntime{}
	var result gateproof.CleanupResult
	var cleanupErr error
	cleanDuringProof := func(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
		result, cleanupErr = gateproof.Verifier{Runtime: runtime, Root: root}.Cleanup(context.Background())
		return worker.CommandResult{}, nil
	}

	report := gateproof.Verifier{Runtime: &fakeRuntime{scripts: map[string]commandScript{"npm ci": cleanDuringProof}}, Root: root}.Verify(context.Background(), proofRepository(t, proofConfig))

	if report.Result != gateproof.ResultPassed || report.Cleanup.Status != gateproof.CleanupRemoved {
		t.Fatalf("proof = %q, cleanup %#v; want a passed proof that cleaned up", report.Result, report.Cleanup)
	}
	if cleanupErr != nil {
		t.Fatalf("Cleanup() error = %v", cleanupErr)
	}
	if len(result.Active) != 1 || len(result.Removed) != 2 || result.Removed[0] != "onboard-proof-1111111111111111" || result.Removed[1] != filepath.Base(stale) {
		t.Fatalf("Cleanup() = %#v, want the stale and the unpublished proofs removed and the running proof active", result)
	}
	if len(runtime.cleaned) != 2 {
		t.Fatalf("cleaned %#v, want the two leftover workers", runtime.cleaned)
	}
	if _, err := os.Stat(unpublished); !os.IsNotExist(err) {
		t.Fatalf("unpublished workspace remains (stat error %v)", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale workspace remains (stat error %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "unrelated", "keep.txt")); err != nil {
		t.Fatalf("cleanup touched an unrelated directory: %v", err)
	}
}

// TestCleanupWithoutARootIsEmpty verifies cleanup before any proof succeeds.
func TestCleanupWithoutARootIsEmpty(t *testing.T) {
	result, err := gateproof.Verifier{Runtime: &fakeRuntime{}, Root: filepath.Join(t.TempDir(), "absent")}.Cleanup(context.Background())
	if err != nil || len(result.Removed) != 0 || len(result.Active) != 0 {
		t.Fatalf("Cleanup() = %#v, %v; want an empty result", result, err)
	}
}

// TestVerifyGivesTheWorkerACheckpointOfTheCandidate verifies the worker sees
// production-shaped Git metadata: the repository history, without
// configuration or hooks, and a checkpoint commit of exactly the candidate
// files on top of HEAD, so Git-reading gates are proven too.
func TestVerifyGivesTheWorkerACheckpointOfTheCandidate(t *testing.T) {
	repository := proofRepository(t, proofConfig)
	git(t, repository, "config", "core.hooksPath", "hooks")
	parent := strings.TrimSpace(git(t, repository, "rev-parse", "HEAD"))
	var inspected map[string]string
	inspect := func(_ context.Context, _ worker.CommandRequest) (worker.CommandResult, error) {
		inspected = map[string]string{}
		return worker.CommandResult{}, nil
	}
	runtime := &fakeRuntime{scripts: map[string]commandScript{"npm ci": inspect}}
	runtime.inspect = func(start worker.StartRequest) {
		inGit := func(args ...string) string {
			t.Helper()
			return strings.TrimSpace(git(t, start.WorktreePath, append([]string{"--git-dir", start.GitMetadataPath, "--work-tree", start.WorktreePath}, args...)...))
		}
		inspected["parent"] = inGit("rev-parse", "HEAD^")
		inspected["readme"] = inGit("show", "HEAD:README.md")
		inspected["status"] = inGit("status", "--porcelain")
		inspected["files"] = inGit("ls-tree", "-r", "--name-only", "HEAD")
		for _, forbidden := range []string{"config", "hooks"} {
			if _, err := os.Stat(filepath.Join(start.GitMetadataPath, forbidden)); !os.IsNotExist(err) {
				inspected["forbidden"] += forbidden
			}
		}
	}

	report := gateproof.Verifier{Runtime: runtime, Root: t.TempDir()}.Verify(context.Background(), repository)

	if report.Result != gateproof.ResultPassed {
		t.Fatalf("Result = %q: %#v", report.Result, report)
	}
	if inspected["parent"] != parent || inspected["readme"] != "changed by onboarding" || inspected["status"] != "" || inspected["forbidden"] != "" {
		t.Fatalf("worker Git metadata = %#v, want a clean checkpoint of the candidate on %s without config or hooks", inspected, parent)
	}
	if want := ".gitignore\nREADME.md\nfactory.yaml\nscripts/check.sh"; inspected["files"] != want {
		t.Fatalf("checkpoint files = %q, want %q", inspected["files"], want)
	}
	if report.GitHead == "" || report.GitHead == parent {
		t.Fatalf("GitHead = %q, want the checkpoint commit", report.GitHead)
	}
}

// TestVerifyEvidenceFollowsTheHistory verifies a new commit gives new
// evidence, because a gate can read the history.
func TestVerifyEvidenceFollowsTheHistory(t *testing.T) {
	repository := proofRepository(t, proofConfig)
	evidence := func() string {
		return gateproof.Verifier{Runtime: &fakeRuntime{}, Root: t.TempDir()}.Verify(context.Background(), repository).EvidenceID
	}
	before := evidence()
	git(t, repository, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "later")
	if after := evidence(); after == before || after == "" {
		t.Fatalf("evidence %q after a commit, was %q; want a new identity", after, before)
	}
}

// TestVerifyReportsAnInterruptionBeforeTheWorkerStarts verifies a proof
// cancelled before it ran a command reports the interruption, not an
// unavailable proof.
func TestVerifyReportsAnInterruptionBeforeTheWorkerStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, runtime := verify(t, ctx, proofConfig, nil)

	if report.Result != gateproof.ResultInterrupted || len(runtime.started) != 0 || report.Cleanup.Status != gateproof.CleanupRemoved {
		t.Fatalf("report = %#v, started %d; want an interrupted proof that cleaned up", report, len(runtime.started))
	}
}

// TestVerifyKeepsTheHistoryOfALinkedWorktree verifies a candidate in a linked
// worktree gets a checkpoint on that worktree's HEAD, not on the HEAD of the
// primary checkout, as a run projects it.
func TestVerifyKeepsTheHistoryOfALinkedWorktree(t *testing.T) {
	primary := proofRepository(t, proofConfig)
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, primary, "worktree", "add", "-q", "-b", "feature", linked)
	writeFile(t, linked, "factory.yaml", proofConfig, 0o644)
	git(t, linked, "add", "factory.yaml")
	git(t, linked, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "feature work")
	want := strings.TrimSpace(git(t, linked, "rev-parse", "HEAD"))
	var parent string
	runtime := &fakeRuntime{scripts: map[string]commandScript{"npm ci": exitWith(0, "")}}
	runtime.inspect = func(start worker.StartRequest) {
		parent = strings.TrimSpace(git(t, start.WorktreePath, "--git-dir", start.GitMetadataPath, "--work-tree", start.WorktreePath, "rev-parse", "HEAD^"))
	}

	report := gateproof.Verifier{Runtime: runtime, Root: t.TempDir()}.Verify(context.Background(), linked)

	if report.Result != gateproof.ResultPassed {
		t.Fatalf("Result = %q: %#v", report.Result, report)
	}
	if parent != want {
		t.Fatalf("checkpoint parent = %q, want the linked worktree HEAD %q", parent, want)
	}
}

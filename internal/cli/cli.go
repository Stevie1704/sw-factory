package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// commandHandler runs one validated CLI command.
type commandHandler func(context.Context, []string, string, io.Writer, io.Writer) int

// commandDefinition associates a user-facing command name with its handler.
type commandDefinition struct {
	name    string
	handler commandHandler
}

// commandTable is the single source of truth for supported CLI commands and
// their dispatch order in validation diagnostics.
var commandTable = []commandDefinition{
	{name: "init", handler: runInit},
	{name: "register", handler: runRegister},
	{name: "bootstrap-labels", handler: runBootstrapLabels},
	{name: "doctor", handler: runDoctor},
	{name: "start", handler: runStart},
	{name: "stop", handler: runStop},
	{name: "resume", handler: runResume},
	{name: "attach", handler: runAttach},
	{name: "auth", handler: runAuth},
	{name: "issue", handler: runIssue},
	{name: "agent", handler: runAgent},
	{name: "agent-report", handler: runAgentReport},
	{name: "draft-pr", handler: runDraftPullRequest},
	{name: "poll", handler: runPollCommands},
	{name: "reconcile", handler: runReconcile},
	{name: "status", handler: runStatus},
	{name: "evaluation", handler: runEvaluation},
	{name: "evaluation-delete", handler: runEvaluationDelete},
	{name: "evaluation-disposition", handler: runEvaluationDisposition},
	{name: "cleanup", handler: runCleanup},
}

// Run dispatches the requested CLI command and returns its exit status.
// It returns 0 for successful execution, 1 for operational errors, and 2 for
// missing or unknown commands. Errors are written to errorsOutput.
func Run(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	expectedCommands := commandNames()
	if len(args) == 0 {
		writeError(errorsOutput, fmt.Errorf("a command is required: %s", expectedCommands))
		return 2
	}
	handler, ok := commandFor(args[0])
	if !ok {
		writeError(errorsOutput, fmt.Errorf("unknown command %q: expected %s", args[0], expectedCommands))
		return 2
	}
	configPath, err := config.DefaultHostConfigPath()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	return handler(ctx, args[1:], configPath, output, errorsOutput)
}

// commandFor resolves one user-facing command name.
func commandFor(name string) (commandHandler, bool) {
	for _, command := range commandTable {
		if command.name == name {
			return command.handler, true
		}
	}
	return nil, false
}

// commandNames returns the supported command names in their CLI order.
func commandNames() string {
	names := make([]string, 0, len(commandTable))
	for _, command := range commandTable {
		names = append(names, command.name)
	}
	return strings.Join(names, ", ")
}

// runBootstrapLabels handles explicit creation of the factory-owned labels.
func runBootstrapLabels(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("bootstrap-labels", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("bootstrap-labels does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).BootstrapLabels(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "bootstrapped factory labels for %s: %s\n", result.Repository, strings.Join(result.Labels, ", ")) {
		return 1
	}
	return 0
}

// runDoctor renders every startup diagnosis and returns a nonzero status when
// any blocking prerequisite remains unresolved.
func runDoctor(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("doctor does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).Doctor(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	for _, check := range result.Report.Results {
		if check.Status == doctor.StatusPassed {
			if !writeOutput(output, errorsOutput, "doctor: %s: passed\n", check.Name) {
				return 1
			}
			continue
		}
		if !writeOutput(output, errorsOutput, "doctor: %s: %s\nproblem: %s\naction: %s\n", check.Name, check.Status, check.Problem, check.Action) {
			return 1
		}
	}
	if result.Ready() {
		if !writeOutput(output, errorsOutput, "doctor: ready\n") {
			return 1
		}
		return 0
	}
	if !writeOutput(output, errorsOutput, "doctor: blocked\n") {
		return 1
	}
	return 1
}

// runStart starts the persistent polling coordinator and returns when its
// context is cancelled by factory stop or an operating-system signal.
func runStart(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("start does not accept positional arguments"))
		return 2
	}
	if !writeOutput(output, errorsOutput, "factory polling starting\n") {
		return 1
	}
	if err := factory.New(*configPath).Start(ctx); err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "factory polling stopped\n") {
		return 1
	}
	return 0
}

// runStop signals the persistent polling coordinator without changing any
// active run state or removing recoverable run artifacts.
func runStop(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("stop", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("stop does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).Stop(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if result.Running {
		if !writeOutput(output, errorsOutput, "factory stop requested (pid=%d)\n", result.PID) {
			return 1
		}
		return 0
	}
	if !writeOutput(output, errorsOutput, "factory is not running\n") {
		return 1
	}
	return 0
}

// runResume performs one explicit native-session or harness-capacity recovery
// for the active run, or re-enters a coordinator-owned check after restart
// reconciliation. A successful native resume remains behind the attach gate
// until the operator acknowledges the visible terminal session.
func runResume(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "active factory run identifier")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("resume does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).Resume(ctx, factory.ResumeRequest{RunID: *runID})
	if result.Invocation.ID != "" {
		status := "resumed"
		switch {
		case result.WaitingForAttach:
			status = "resumed; attach required"
		case result.Run.Status == store.StatusWaitingForHarness:
			status = "waiting for harness capacity"
		case result.Run.Status == store.StatusWaitingForHuman:
			status = "waiting for human"
		}
		if !writeOutput(output, errorsOutput, "agent %s\nrun: %s\ninvocation: %s\nstatus: %s\n", status, result.Run.ID, result.Invocation.ID, status) {
			return 1
		}
	} else if err == nil && result.Run.ID != "" {
		if !writeOutput(output, errorsOutput, "check evaluation resumed\nrun: %s\nstage: %s\nstatus: %s\n", result.Run.ID, result.Run.Stage, result.Run.Status) {
			return 1
		}
	}
	if err != nil {
		var attachRequired *factory.ManualResumeRequiredError
		if errors.As(err, &attachRequired) && result.WaitingForAttach {
			return 0
		}
		writeError(errorsOutput, err)
		return 1
	}
	return 0
}

// runAttach restores the worker and visible terminal topology, then releases
// the manual native-session gate for the active run.
func runAttach(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("attach", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "active factory run identifier")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("attach does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).Attach(ctx, factory.AttachRequest{RunID: *runID})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "agent attached\nrun: %s\ninvocation: %s\nstatus: %s\n", result.Run.ID, result.Invocation.ID, result.Run.Status) {
		return 1
	}
	return 0
}

// runAuth dispatches authentication maintenance subcommands. The refresh
// operation only seeds a factory-managed worker credential store.
func runAuth(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	if len(args) == 0 || args[0] != "refresh" {
		writeError(errorsOutput, errors.New("auth requires the refresh subcommand"))
		return 2
	}
	flags := flag.NewFlagSet("auth refresh", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "active factory run identifier")
	harnessName := flags.String("harness", "", "codex or claude; empty uses the invocation harness")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("auth refresh does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).RefreshAuth(ctx, factory.AuthRefreshRequest{RunID: *runID, Harness: config.Harness(*harnessName)})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "authentication refreshed\nrun: %s\ninvocation: %s\nharness: %s\n", result.Run.ID, result.Invocation.ID, result.Harness) {
		return 1
	}
	return 0
}

// runIssue handles the one-shot issue claim command.
func runIssue(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("issue", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	issueFlag := flags.Int("issue", 0, "GitHub issue number")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		writeError(errorsOutput, errors.New("issue accepts one issue number as a positional argument"))
		return 2
	}
	issueNumber := *issueFlag
	if flags.NArg() == 1 {
		if issueNumber != 0 {
			writeError(errorsOutput, errors.New("issue number must be provided either positionally or with --issue, not both"))
			return 2
		}
		parsed, err := strconv.Atoi(flags.Arg(0))
		if err != nil {
			writeError(errorsOutput, fmt.Errorf("invalid issue number %q: %w", flags.Arg(0), err))
			return 2
		}
		issueNumber = parsed
	}
	if issueNumber <= 0 {
		writeError(errorsOutput, errors.New("issue requires a positive issue number"))
		return 2
	}
	service := factory.New(*configPath)
	result, err := service.ClaimIssue(ctx, issueNumber)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	baseline, err := service.RunBaseline(ctx, factory.BaselineRequest{RunID: result.Run.ID})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	policyMode := result.Packet.RepositoryConfig.TestPolicy.Mode
	if !writeOutput(output, errorsOutput, "claimed issue #%d\nrun: %s\nbranch: %s\nworktree: %s\nstage: %s\nstatus: %s\ntest policy: %s\nroute: %s\nbaseline gates: %d\n", baseline.Run.IssueNumber, baseline.Run.ID, baseline.Run.Branch, baseline.Run.Worktree, baseline.Run.Stage, baseline.Run.Status, factory.TestPolicyDescription(policyMode), result.Packet.Route.Description(), len(baseline.Gates)) {
		return 1
	}
	return 0
}

// runAgent starts the visible agent for the active run, selecting the frozen
// policy's independent test stage or implementation-owned TDD path.
func runAgent(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "active factory run identifier")
	role := flags.String("role", "", "workflow role; empty selects the active stage")
	stage := flags.String("stage", "", "workflow stage; empty selects the active stage")
	harnessName := flags.String("harness", "", "validated harness override")
	model := flags.String("model", "", "validated model override")
	reasoningEffort := flags.String("reasoning-effort", "", "validated reasoning-effort override")
	codexAuthPath := flags.String("codex-auth", "", "explicit Codex auth.json source")
	claudeAuthPath := flags.String("claude-auth", "", "explicit Claude credential source")
	permittedPaths := stringList{}
	flags.Var(&permittedPaths, "permitted-path", "repository-relative handoff path prefix; may be repeated")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("agent does not accept positional arguments"))
		return 2
	}
	launch, err := factory.New(*configPath).StartAgent(ctx, factory.AgentRequest{
		RunID:           *runID,
		Role:            *role,
		Stage:           store.Stage(*stage),
		Harness:         config.Harness(*harnessName),
		Model:           *model,
		ReasoningEffort: *reasoningEffort,
		CodexAuthPath:   *codexAuthPath,
		ClaudeAuthPath:  *claudeAuthPath,
		PermittedPaths:  append([]string(nil), permittedPaths...),
	})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	agentSurfaceID := launch.Invocation.RoleSurfaceID
	if agentSurfaceID == "" {
		agentSurfaceID = launch.Invocation.ImplementationSurfaceID
	}
	message := fmt.Sprintf("agent started\nrun: %s\nrole: %s\nstage: %s\ntest policy: %s\nroute: %s\ninvocation: %s\nworkspace: %s\nagent surface: %s\n", launch.Invocation.RunID, launch.Invocation.Role, launch.Invocation.Stage, factory.TestPolicyDescription(launch.TestPolicyMode), launch.Route.Description(), launch.Invocation.ID, launch.Invocation.WorkspaceID, agentSurfaceID)
	if launch.Invocation.ChecksSurfaceID != "" {
		message += fmt.Sprintf("checks surface: %s\n", launch.Invocation.ChecksSurfaceID)
	}
	if !writeOutput(output, errorsOutput, "%s", message) {
		return 1
	}
	return 0
}

// runAgentReport accepts the structured report written by one visible agent.
func runAgentReport(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("agent-report", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "active factory run identifier")
	invocationID := flags.String("invocation-id", "", "invocation identifier")
	permittedPaths := stringList{}
	flags.Var(&permittedPaths, "permitted-path", "repository-relative handoff path prefix; may be repeated")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("agent-report does not accept positional arguments"))
		return 2
	}
	if strings.TrimSpace(*invocationID) == "" {
		writeError(errorsOutput, errors.New("agent-report requires --invocation-id"))
		return 2
	}
	result, err := factory.New(*configPath).AcceptAgentReport(ctx, factory.AgentReportRequest{RunID: *runID, InvocationID: *invocationID, PermittedPaths: append([]string(nil), permittedPaths...)})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "agent report accepted\nrun: %s\ninvocation: %s\noutcome: %s\nstatus: %s\n", result.Invocation.RunID, result.Invocation.ID, result.Report.Outcome, result.Invocation.Status) {
		return 1
	}
	return 0
}

// runDraftPullRequest advances the accepted implementation to one draft pull
// request through host-side checkpoint, gate, Git, and GitHub effects.
func runDraftPullRequest(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("draft-pr", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "active factory run identifier")
	intervention := flags.String("intervention", "", "operator-visible intervention marker")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("draft-pr does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).CreateDraftPullRequest(ctx, factory.DraftPullRequestRequest{RunID: *runID, Intervention: *intervention})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if result.Repair != nil {
		if !writeOutput(output, errorsOutput, "check repair: outcome=%s\nrun: %s\nattempts: %d/%d\nremaining: %d\nstage: %s\nstatus: %s\n", result.Repair.Outcome, result.Repair.Run.ID, result.Repair.Attempt, result.Repair.Budget, result.Repair.Remaining, result.Repair.Run.Stage, result.Repair.Run.Status) {
			return 1
		}
		for _, gateResult := range result.Gates {
			if !writeOutput(output, errorsOutput, "gate: %s (%s)\n", gateResult.GateName, gateResult.Status.State) {
				return 1
			}
		}
		return 0
	}
	if !writeOutput(output, errorsOutput, "draft pull request ready\nrun: %s\ncheckpoint: %s\npull request: #%d %s\nstage: %s\n", result.Run.ID, result.Run.CheckpointSHA, result.PullRequest.Number, result.PullRequest.URL, result.Run.Stage) {
		return 1
	}
	for _, gateResult := range result.Gates {
		if !writeOutput(output, errorsOutput, "gate: %s (%s)\n", gateResult.GateName, gateResult.Status.State) {
			return 1
		}
	}
	return 0
}

// runPollCommands performs one production command poll for the current run.
// Repeating this command is safe because the coordinator persists the
// processed-comment watermark and revision.
func runPollCommands(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("poll", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "active or latest factory run identifier")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("poll does not accept positional arguments"))
		return 2
	}
	service := factory.New(*configPath)
	lifecycle, err := service.PollLifecycle(ctx, factory.LifecycleRequest{RunID: *runID})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if lifecycle.Outcome != factory.LifecycleUnchanged {
		if !writeOutput(output, errorsOutput, "lifecycle: %s run=%s status=%s\n", lifecycle.Outcome, lifecycle.Run.ID, lifecycle.Run.Status) {
			return 1
		}
		return 0
	}
	results, err := service.PollCommands(ctx, factory.CommandPollRequest{RunID: *runID})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if len(results) == 0 {
		if !writeOutput(output, errorsOutput, "no structured commands found\n") {
			return 1
		}
		return 0
	}
	for _, result := range results {
		name := string(result.Command.Kind)
		if name == "" {
			name = "malformed"
		}
		if !writeOutput(output, errorsOutput, "command: %s outcome: %s\n", name, result.Outcome) {
			return 1
		}
		if result.Rejection != nil && !writeOutput(output, errorsOutput, "rejection: %s (%s)\n", result.Rejection.Code, result.Rejection.Problem) {
			return 1
		}
	}
	return 0
}

// runInit handles the init command, creating the host configuration at the requested path and reporting its result.
func runInit(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("init does not accept positional arguments"))
		return 2
	}
	service := factory.New(*configPath)
	result, err := service.Init(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "created host configuration: %s\n", result.ConfigPath) {
		return 1
	}
	return 0
}

// runRegister registers a repository using command-line options and reports the resulting repository and operational store paths.
func runRegister(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	repositoryPath := flags.String("repository", "", "registered repository path")
	githubOwner := flags.String("github-owner", "", "GitHub repository owner")
	githubRepository := flags.String("github-repository", "", "GitHub repository name")
	operationalDataPath := flags.String("operational-data", "", "SQLite operational data path")
	pollingInterval := flags.String("poll-interval", "30s", "polling interval")
	pollingBackoff := flags.String("poll-backoff", "5m", "transport backoff")
	cmuxSocketPath := flags.String("cmux-socket", "", "cmux socket path")
	cmuxWorkspace := flags.String("cmux-workspace", "", "cmux control workspace")
	codexAuthPath := flags.String("codex-auth", "", "host Codex auth.json path")
	claudeAuthPath := flags.String("claude-auth", "", "host Claude credential path")
	repositoryConfigPath := flags.String("repository-config", "", "checked-in repository configuration path")
	authorizedUsers := stringList{}
	flags.Var(&authorizedUsers, "authorized-user", "authorized GitHub username; may be repeated")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("register does not accept positional arguments"))
		return 2
	}
	if strings.TrimSpace(*repositoryPath) == "" || strings.TrimSpace(*githubOwner) == "" || strings.TrimSpace(*githubRepository) == "" || len(authorizedUsers) == 0 {
		writeError(errorsOutput, errors.New("register requires --repository, --github-owner, --github-repository, and at least one --authorized-user"))
		return 2
	}
	service := factory.New(*configPath)
	result, err := service.Register(ctx, factory.RegisterRequest{
		RepositoryPath:       *repositoryPath,
		GitHubOwner:          *githubOwner,
		GitHubRepository:     *githubRepository,
		AuthorizedUsers:      []string(authorizedUsers),
		OperationalDataPath:  *operationalDataPath,
		PollingInterval:      *pollingInterval,
		PollingBackoff:       *pollingBackoff,
		CmuxSocketPath:       *cmuxSocketPath,
		CmuxControlWorkspace: *cmuxWorkspace,
		CodexAuthPath:        *codexAuthPath,
		ClaudeAuthPath:       *claudeAuthPath,
		RepositoryConfigPath: *repositoryConfigPath,
	})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "registered repository: %s\noperational store: %s\n", result.RepositoryPath, result.OperationalDataPath) {
		return 1
	}
	return 0
}

// runStatus reports the current configuration, repository registration, and active run.
// It returns 0 on success, 1 when status retrieval fails, or 2 when arguments are invalid.
func runStatus(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("status does not accept positional arguments"))
		return 2
	}
	service := factory.New(*configPath)
	result, err := service.Status(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "config: %s\n", result.ConfigPath) {
		return 1
	}
	if result.RepositoryPath == "" {
		if !writeOutput(output, errorsOutput, "repository: none registered\n") {
			return 1
		}
	} else {
		if !writeOutput(output, errorsOutput, "repository: %s\n", result.RepositoryPath) {
			return 1
		}
	}
	if result.LatestRun == nil {
		if !writeOutput(output, errorsOutput, "active run: none\n") {
			return 1
		}
	} else {
		label := "active run"
		if store.IsTerminalStatus(result.LatestRun.Status) {
			label = "last run"
		}
		if !writeOutput(output, errorsOutput, "%s: %s (stage=%s status=%s test_policy=%s route=%s branch=%s worktree=%s)\n", label, result.LatestRun.ID, result.LatestRun.Stage, result.LatestRun.Status, factory.TestPolicyDescription(result.TestPolicyMode), result.Route.Description(), result.LatestRun.Branch, result.LatestRun.Worktree) {
			return 1
		}
	}
	if result.Recovery != nil {
		agreement := "disagree"
		if result.Recovery.SourcesAgree {
			agreement = "agree"
		}
		if !writeOutput(output, errorsOutput, "recovery: %s (run=%s sources=%s)\n", result.Recovery.Code, result.Recovery.RunID, agreement) {
			return 1
		}
		if pending := result.Recovery.PendingEffect; pending != nil {
			if !writeOutput(output, errorsOutput, "recovery pending effect: id=%s kind=%s\n", pending.ID, pending.Kind) {
				return 1
			}
		}
		for _, discrepancy := range result.Recovery.Discrepancies {
			if !writeOutput(output, errorsOutput, "recovery discrepancy: %s.%s expected=%q observed=%q\n", discrepancy.Source, discrepancy.Field, discrepancy.Expected, discrepancy.Observed) {
				return 1
			}
		}
		for _, action := range result.Recovery.SafeActions {
			if !writeOutput(output, errorsOutput, "recovery safe action: %s\n", action) {
				return 1
			}
		}
	}
	return 0
}

// runReconcile executes one restart reconciliation or explicitly abandons the
// effect named by --abandon-effect after a human has inspected it.
func runReconcile(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "optional run identifier")
	effectID := flags.String("abandon-effect", "", "effect identity to abandon after human review")
	reason := flags.String("reason", "", "reason for abandoning the pending effect")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("reconcile does not accept positional arguments"))
		return 2
	}
	if *effectID == "" && *reason != "" {
		writeError(errorsOutput, errors.New("--reason applies only to --abandon-effect"))
		return 2
	}
	if *effectID == "" && *runID != "" {
		writeError(errorsOutput, errors.New("--run-id applies only to --abandon-effect"))
		return 2
	}
	service := factory.New(*configPath)
	var (
		result factory.RecoveryResult
		err    error
	)
	if *effectID != "" {
		result, err = service.AbandonPendingEffect(ctx, factory.AbandonPendingEffectRequest{RunID: *runID, EffectID: *effectID, Reason: *reason})
	} else {
		result, err = service.Reconcile(ctx)
	}
	// A paused run is a reported outcome, not a failed command: reconciliation
	// returns a typed discrepancy error alongside the diagnosis it just wrote.
	// Print the structured result first so the operator sees the run, the
	// pending effect, and every discrepancy, then surface the typed error.
	if result.Run == nil {
		if err != nil {
			writeError(errorsOutput, err)
			return 1
		}
		if !writeOutput(output, errorsOutput, "reconciliation: no persisted run\n") {
			return 1
		}
		return 0
	}
	if !writeOutput(output, errorsOutput, "reconciliation: outcome=%s run=%s status=%s stage=%s\n", result.Outcome, result.Run.ID, result.Run.Status, result.Run.Stage) {
		return 1
	}
	if pending := result.Diagnosis.PendingEffect; pending != nil {
		if !writeOutput(output, errorsOutput, "reconciliation pending effect: id=%s kind=%s\n", pending.ID, pending.Kind) {
			return 1
		}
	}
	for _, discrepancy := range result.Diagnosis.Discrepancies {
		if !writeOutput(output, errorsOutput, "reconciliation discrepancy: %s.%s expected=%q observed=%q\n", discrepancy.Source, discrepancy.Field, discrepancy.Expected, discrepancy.Observed) {
			return 1
		}
	}
	for _, action := range result.Diagnosis.SafeActions {
		if !writeOutput(output, errorsOutput, "reconciliation safe action: %s\n", action) {
			return 1
		}
	}
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	return 0
}

// runEvaluation reports retained per-run summaries and local aggregate values
// without making a network call or displaying work content.
func runEvaluation(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("evaluation", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "one opaque evaluation run identifier")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("evaluation does not accept positional arguments"))
		return 2
	}
	result, err := factory.New(*configPath).EvaluationReport(ctx, factory.EvaluationReportRequest{RunID: *runID})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "evaluation summaries: %d\n", len(result.Summaries)) {
		return 1
	}
	if result.Retention == "" {
		if !writeOutput(output, errorsOutput, "retention: explicit deletion only\n") {
			return 1
		}
	} else if !writeOutput(output, errorsOutput, "retention: %s (deletion remains explicit)\n", result.Retention) {
		return 1
	}
	for _, summary := range result.Summaries {
		if !writeOutput(output, errorsOutput, "run: %s outcome=%s started=%s completed=%s wall=%s stages=%v versions=%v invocations=%d gates=%d check_repairs=%d test_revisions=%d review_revisions=%d budget_exhausted=%t exemptions=%v escalations=%v blockers=%v usage_available=%t\n", summary.RunID, summary.Outcome, summary.StartedAt.Format(time.RFC3339Nano), summary.CompletedAt.Format(time.RFC3339Nano), summary.TotalWallTime, summary.StageDurations, summary.InvocationVersions, summary.InvocationCount, summary.GateCount, summary.CheckRepairCount, summary.TestRevisionCount, summary.ReviewRevisionCount, summary.BudgetExhausted, summary.Exemptions, summary.EscalationCategories, summary.RepeatedBlockers, summary.Usage.Available) {
			return 1
		}
		if summary.Usage.Available {
			if !writeOutput(output, errorsOutput, "run usage: run=%s input_tokens=%d output_tokens=%d total_tokens=%d cost_reported=%t cost_micros=%d currency=%s cost_status=%s\n", summary.RunID, summary.Usage.InputTokens, summary.Usage.OutputTokens, summary.Usage.TotalTokens, summary.Usage.CostReported, summary.Usage.CostMicros, summary.Usage.Currency, evaluationCostStatus(summary.Usage)) {
				return 1
			}
		} else if !writeOutput(output, errorsOutput, "run usage: run=%s unavailable (%s)\n", summary.RunID, summary.Usage.UnavailableReason) {
			return 1
		}
	}
	aggregate := result.Aggregate
	if !writeOutput(output, errorsOutput, "aggregate: runs=%d outcomes=%v outcome_rates=%v success_rate=%.4f budget_exhaustion_rate=%.4f total_wall=%s average_wall=%s stages=%v invocations=%d gates=%d check_repairs=%d test_revisions=%d review_revisions=%d escalations=%v escalation_rates=%v dispositions=%v usage_available_runs=%d usage_cost_reported_runs=%d\n", aggregate.RunCount, aggregate.OutcomeCounts, aggregate.OutcomeRates, aggregate.SuccessRate, aggregate.BudgetExhaustionRate, aggregate.TotalWallTime, aggregate.AverageTotalWallTime, aggregate.StageDurations, aggregate.InvocationCount, aggregate.GateCount, aggregate.CheckRepairCount, aggregate.TestRevisionCount, aggregate.ReviewRevisionCount, aggregate.EscalationCounts, aggregate.EscalationRates, aggregate.DispositionCounts, aggregate.UsageAvailableRuns, aggregate.UsageCostReportedRuns) {
		return 1
	}
	if aggregate.Usage.Available {
		if !writeOutput(output, errorsOutput, "aggregate usage: input_tokens=%d output_tokens=%d total_tokens=%d cost_reported=%t cost_micros=%d currency=%s cost_status=%s\n", aggregate.Usage.InputTokens, aggregate.Usage.OutputTokens, aggregate.Usage.TotalTokens, aggregate.Usage.CostReported, aggregate.Usage.CostMicros, aggregate.Usage.Currency, evaluationCostStatus(aggregate.Usage)) {
			return 1
		}
	} else if !writeOutput(output, errorsOutput, "aggregate usage: unavailable (%s)\n", aggregate.Usage.UnavailableReason) {
		return 1
	}
	return 0
}

// evaluationCostStatus renders whether aggregate cost was reported or why it
// remains unavailable, independently of token availability.
func evaluationCostStatus(usage store.EvaluationUsage) string {
	if usage.CostReported {
		return "reported"
	}
	if usage.UnavailableReason == "" {
		return "not_reported"
	}
	return "unavailable:" + usage.UnavailableReason
}

// runEvaluationDelete performs deliberate, visible deletion of selected
// terminal evaluation summaries and requires an explicit confirmation flag.
func runEvaluationDelete(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("evaluation-delete", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	before := flags.String("before", "", "RFC3339 cutoff; only terminal summaries before it are deleted")
	confirm := flags.Bool("confirm", false, "confirm deliberate deletion of evaluation summaries")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("evaluation-delete does not accept positional arguments"))
		return 2
	}
	if !*confirm {
		writeError(errorsOutput, errors.New("evaluation-delete requires --confirm"))
		return 2
	}
	cutoff, err := time.Parse(time.RFC3339Nano, *before)
	if err != nil {
		writeError(errorsOutput, errors.New("evaluation-delete requires --before as an RFC3339 timestamp"))
		return 2
	}
	result, err := factory.New(*configPath).DeleteEvaluation(ctx, factory.EvaluationDeleteRequest{Before: cutoff})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "deleted evaluation summaries: %d (dispositions=%d usage=%d cutoff=%s)\n", result.Deleted.Summaries, result.Deleted.Dispositions, result.Deleted.UsageRows, result.Before.Format(time.RFC3339Nano)) {
		return 1
	}
	return 0
}

// runEvaluationDisposition attaches one explicit human disposition to a
// test-dispute or review-finding escalation without retaining finding text.
func runEvaluationDisposition(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("evaluation-disposition", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "evaluation run identifier")
	eventID := flags.String("event-id", "", "opaque escalation identity; it is hashed before persistence")
	category := flags.String("category", "", "test_dispute or review_finding")
	disposition := flags.String("disposition", "", "upheld, advisory, or overturned")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("evaluation-disposition does not accept positional arguments"))
		return 2
	}
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*eventID) == "" || strings.TrimSpace(*category) == "" || strings.TrimSpace(*disposition) == "" {
		writeError(errorsOutput, errors.New("evaluation-disposition requires --run-id, --event-id, --category, and --disposition"))
		return 2
	}
	result, err := factory.New(*configPath).AttachEvaluationDisposition(ctx, factory.EvaluationDispositionRequest{
		RunID:       *runID,
		EventID:     *eventID,
		Category:    store.EvaluationEscalationCategory(*category),
		Disposition: store.EvaluationDisposition(*disposition),
		DecidedAt:   time.Now().UTC(),
	})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "evaluation disposition recorded: run=%s category=%s disposition=%s event_id_hash=%s\n", result.Disposition.RunID, result.Disposition.Category, result.Disposition.Disposition, result.Disposition.EventIDHash) {
		return 1
	}
	return 0
}

// runCleanup displays the exact local cleanup plan and requires --confirm
// before asking the Factory service to remove any run resources.
func runCleanup(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "terminal factory run identifier")
	confirm := flags.Bool("confirm", false, "confirm cleanup of the displayed local targets")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("cleanup does not accept positional arguments"))
		return 2
	}

	service := factory.New(*configPath)
	preview, err := service.Cleanup(ctx, factory.CleanupRequest{RunID: *runID})
	var confirmationErr *factory.CleanupConfirmationRequiredError
	if err != nil && !errors.As(err, &confirmationErr) {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeCleanupPlan(output, errorsOutput, preview.Plan) {
		return 1
	}
	if !*confirm {
		if !writeOutput(output, errorsOutput, "cleanup requires --confirm; no resources removed\n") {
			return 1
		}
		return 2
	}

	result, err := service.Cleanup(ctx, factory.CleanupRequest{
		RunID:        *runID,
		Confirm:      true,
		Before:       preview.Plan.Before,
		ExpectedPlan: &preview.Plan,
	})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "cleanup complete: runs=%d\n", len(result.Deleted)) {
		return 1
	}
	return 0
}

// writeCleanupPlan renders every exact target before any confirmed mutation.
func writeCleanupPlan(output, errorsOutput io.Writer, plan factory.CleanupPlan) bool {
	if !writeOutput(output, errorsOutput, "cleanup cutoff: %s\ncleanup runs: %d\n", plan.Before.Format(time.RFC3339Nano), len(plan.Runs)) {
		return false
	}
	for _, run := range plan.Runs {
		if !writeOutput(output, errorsOutput, "cleanup run: %s status=%s eligible_at=%s\ncleanup worktree: %s\ncleanup branch: %s (local only; remote retained)\ncleanup container: run=%s\n", run.RunID, run.Status, run.EligibleAt.Format(time.RFC3339Nano), run.Worktree, run.Branch, run.WorkerRunID) {
			return false
		}
		if len(run.Roles) > 0 && !writeOutput(output, errorsOutput, "cleanup worker roles: %s\n", strings.Join(run.Roles, ", ")) {
			return false
		}
		for _, outputPath := range run.StoredOutputs {
			if !writeOutput(output, errorsOutput, "cleanup stored output: %s\n", outputPath) {
				return false
			}
		}
		if len(run.StoredOutputs) == 0 && !writeOutput(output, errorsOutput, "cleanup stored output: none\n") {
			return false
		}
	}
	for _, skipped := range plan.Skipped {
		if !writeOutput(output, errorsOutput, "cleanup skipped run: %s reason=%s\n", skipped.RunID, skipped.Reason) {
			return false
		}
	}
	return true
}

// writeOutput writes one successful command response and reports writer errors
// through the same error path used for operational failures.
func writeOutput(output, errorsOutput io.Writer, format string, args ...any) bool {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		writeError(errorsOutput, err)
		return false
	}
	return true
}

// writeError writes a non-nil error to output using the format "error: <message>".
func writeError(output io.Writer, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(output, "error: %v\n", err)
	}
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value must not be empty")
	}
	*s = append(*s, value)
	return nil
}

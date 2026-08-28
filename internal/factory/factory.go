package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/google/uuid"
)

// Factory is the high-level interface exposed by the command-line coordinator.
type Factory interface {
	Init(context.Context) (InitResult, error)
	Register(context.Context, RegisterRequest) (RegisterResult, error)
	BootstrapLabels(context.Context) (BootstrapLabelsResult, error)
	// Doctor runs every configured startup diagnosis and reports whether a run
	// can be claimed safely.
	Doctor(context.Context) (DoctorResult, error)
	// Start runs the supervised polling coordinator until its context is
	// cancelled or a separate Stop command signals it.
	Start(context.Context) error
	// Stop asks a running coordinator to stop polling without changing the
	// state of its active run.
	Stop(context.Context) (StopResult, error)
	RunCoordinator
	// RunBaseline evaluates the frozen repository gate model before agent edits.
	RunBaseline(context.Context, BaselineRequest) (BaselineResult, error)
	RunGate(context.Context, RunGateRequest) (gate.Result, error)
	// StartAgent launches the visible Codex test, implementation, or review role for an active run.
	StartAgent(context.Context, AgentRequest) (AgentLaunchResult, error)
	// AcceptAgentReport validates and accepts one structured visible-agent handoff.
	AcceptAgentReport(context.Context, AgentReportRequest) (AgentResult, error)
	// RunAgent launches a visible agent and accepts its already-written report.
	RunAgent(context.Context, AgentRequest) (AgentResult, error)
	// Resume performs an explicit native-session or harness-capacity recovery,
	// or re-enters a coordinator-owned check paused by restart reconciliation.
	Resume(context.Context, ResumeRequest) (ResumeResult, error)
	// Attach acknowledges and reattaches a manually resumed visible session.
	Attach(context.Context, AttachRequest) (AttachResult, error)
	// RefreshAuth reseeds one factory-managed harness credential from its host
	// source without modifying the source.
	RefreshAuth(context.Context, AuthRefreshRequest) (AuthRefreshResult, error)
	// CreateDraftPullRequest checkpoints the accepted implementation, runs every
	// configured gate, pushes the branch, and creates or updates one draft PR.
	CreateDraftPullRequest(context.Context, DraftPullRequestRequest) (DraftPullRequestResult, error)
	Status(context.Context) (StatusResult, error)
	// HandleCommand processes one structured GitHub comment exactly once.
	HandleCommand(context.Context, CommandRequest) (CommandResult, error)
	// PollCommands reads issue and pull-request comments for the current run.
	PollCommands(context.Context, CommandPollRequest) ([]CommandResult, error)
	// PollLifecycle observes merge and closure decisions for the current run.
	PollLifecycle(context.Context, LifecycleRequest) (LifecycleResult, error)
	// Reconcile completes one restart reconciliation pass for the current run.
	Reconcile(context.Context) (RecoveryResult, error)
	// AbandonPendingEffect explicitly discards one ambiguous effect after human
	// review and leaves the run waiting for human reconciliation.
	AbandonPendingEffect(context.Context, AbandonPendingEffectRequest) (RecoveryResult, error)
	// EvaluationReport reads content-free local evaluation summaries and aggregates.
	EvaluationReport(context.Context, EvaluationReportRequest) (EvaluationReportResult, error)
	// DeleteEvaluation deliberately removes selected terminal evaluation summaries.
	DeleteEvaluation(context.Context, EvaluationDeleteRequest) (EvaluationDeleteResult, error)
	// AttachEvaluationDisposition records an explicit human escalation disposition.
	AttachEvaluationDisposition(context.Context, EvaluationDispositionRequest) (EvaluationDispositionResult, error)
	// Cleanup previews and, when explicitly confirmed, removes eligible local
	// run artifacts without deleting remote branches or evaluation summaries.
	Cleanup(context.Context, CleanupRequest) (CleanupResult, error)
}

// RunCoordinator is the single claim/state-transition seam used by the
// one-shot command and the persistent polling coordinator.
type RunCoordinator interface {
	ClaimIssue(context.Context, int) (IssueResult, error)
	Transition(context.Context, TransitionRequest) (store.Run, error)
}

type ConfigRepository interface {
	Load(string) (config.HostConfig, error)
	Save(string, config.HostConfig) error
	Create(string) (config.HostConfig, error)
}

type OperationalStore interface {
	CurrentRun(context.Context) (*store.Run, error)
	Close() error
}

// RunStore is the operational-store seam required by claim and transition
// operations.
type RunStore interface {
	OperationalStore
	SaveRun(context.Context, store.Run) error
	ClaimLifecycleNotification(context.Context, string, store.Status) (bool, error)
	ReleaseLifecycleNotification(context.Context, string, store.Status) error
}

// LatestRunStore extends the operational-store seam with the most recently
// claimed run, including terminal runs used by status reporting.
type LatestRunStore interface {
	OperationalStore
	LatestRun(context.Context) (*store.Run, error)
}

// StoreOpener opens the host-local operational store.
type StoreOpener func(context.Context, string) (OperationalStore, error)

// RepositoryChecker validates a registered repository path.
type RepositoryChecker func(string) error

// RepositoryConfigLoader loads the checked-in repository configuration.
type RepositoryConfigLoader func(string) (config.RepositoryConfig, error)

// Clock supplies the coordinator's current time.
type Clock func() time.Time

// RunIDGenerator supplies an immutable identifier for a new run.
type RunIDGenerator func() (string, error)

// StartupDiagnosis runs the complete pre-poll startup diagnosis.
type StartupDiagnosis func(context.Context) (DoctorResult, error)

// Dependencies are the adapters at the high-level factory seam.
type Dependencies struct {
	Config          ConfigRepository
	OpenStore       StoreOpener
	CheckRepository RepositoryChecker
	LoadRepository  RepositoryConfigLoader
	GitHub          github.Client
	// IssuePoller lists eligible GitHub work without broadening the mutation
	// authority of the existing issue client seam.
	IssuePoller github.IssuePoller
	// Lease publishes the coordinator's visible GitHub heartbeat.
	Lease          github.LeaseClient
	CommitStatuses github.CommitStatusPublisher
	Worktree       gitadapter.WorktreeManager
	// GitWorkspace owns checkpoint, base-sync, push, and cleanup effects on the host.
	GitWorkspace gitadapter.GitWorkspace
	// PullRequests owns idempotent draft pull-request discovery and mutation.
	PullRequests github.PullRequestClient
	// Comments lists issue and pull-request comments for command polling.
	Comments github.CommentReader
	Worker   worker.WorkerRuntime
	// Terminal owns visible control and run workspaces.
	Terminal terminal.TerminalRuntime
	// Harness owns interactive role lifecycle and native session recovery.
	Harness harness.Runtime
	// HarnessCapabilities resolves static adapter capabilities for startup and
	// claim checks without launching a worker or terminal surface.
	HarnessCapabilities harness.CapabilityResolver
	Now                 Clock
	NewRunID            RunIDGenerator
	Coordinator         string
	// StartupDiagnosis is injectable for embedders and deterministic polling
	// tests; nil uses the service's full Doctor implementation.
	StartupDiagnosis StartupDiagnosis
}

type Service struct {
	configPath        string
	deps              Dependencies
	commandMu         sync.Mutex
	runtimeMu         sync.Mutex
	runtimeSocketPath string
	runtimePathSet    bool
	harnessRuntimes   map[config.Harness]harness.Runtime
	// startedInvocations records invocations launched by this coordinator
	// process, so same-process duplicate commands are not mistaken for restart
	// recovery.
	startedInvocations map[string]struct{}
	startupMu          sync.Mutex
	startupChecked     bool
	startupErr         error
	pollMu             sync.Mutex
	pollCancel         context.CancelFunc
}

// markInvocationStarted records an invocation launched by this coordinator
// process so a repeated command in the same process is rejected rather than
// mistaken for a restart recovery.
func (s *Service) markInvocationStarted(invocationID string) {
	if strings.TrimSpace(invocationID) == "" {
		return
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.startedInvocations == nil {
		s.startedInvocations = make(map[string]struct{})
	}
	s.startedInvocations[invocationID] = struct{}{}
}

// invocationStartedHere reports whether this service already launched the
// persisted invocation during its current process lifetime.
func (s *Service) invocationStartedHere(invocationID string) bool {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	_, exists := s.startedInvocations[invocationID]
	return exists
}

type InitResult struct {
	ConfigPath string
}

type RegisterRequest struct {
	RepositoryPath       string
	GitHubOwner          string
	GitHubRepository     string
	AuthorizedUsers      []string
	OperationalDataPath  string
	PollingInterval      string
	PollingBackoff       string
	CmuxSocketPath       string
	CmuxControlWorkspace string
	// CodexAuthPath is an optional host-side Codex auth.json path.
	CodexAuthPath string
	// ClaudeAuthPath is an optional host-side Claude credential file path.
	ClaudeAuthPath       string
	RepositoryConfigPath string
}

type RegisterResult struct {
	RepositoryPath      string
	OperationalDataPath string
}

// StatusResult reports the registered repository and the latest known run.
// ActiveRun is retained as the foundation field name for compatibility; it is
// an alias of LatestRun and may contain a terminal run when no run is active.
type StatusResult struct {
	ConfigPath     string
	RepositoryPath string
	LatestRun      *store.Run
	// TestPolicyMode identifies the frozen TDD ownership mode of LatestRun.
	TestPolicyMode config.TestMode
	// Recovery contains the read-only diagnosis for an interrupted run.
	Recovery *RecoveryDiagnosis
	// Deprecated: use LatestRun.
	ActiveRun *store.Run
}

type fileConfigRepository struct{}

func (fileConfigRepository) Load(path string) (config.HostConfig, error) {
	return config.LoadHost(path)
}

func (fileConfigRepository) Save(path string, value config.HostConfig) error {
	return config.SaveHost(path, value)
}

func (fileConfigRepository) Create(path string) (config.HostConfig, error) {
	return config.CreateHost(path)
}

// New creates a Service configured with the specified host configuration path and default dependencies.
func New(configPath string) *Service {
	return NewWithDependencies(configPath, Dependencies{})
}

// NewWithDependencies creates a Service with the specified configuration path
// and fills missing dependencies with their default implementations.
func NewWithDependencies(configPath string, dependencies Dependencies) *Service {
	if dependencies.Config == nil {
		dependencies.Config = fileConfigRepository{}
	}
	if dependencies.OpenStore == nil {
		dependencies.OpenStore = func(ctx context.Context, path string) (OperationalStore, error) {
			return store.Open(ctx, path)
		}
	}
	if dependencies.CheckRepository == nil {
		dependencies.CheckRepository = checkRepository
	}
	if dependencies.LoadRepository == nil {
		dependencies.LoadRepository = config.LoadRepository
	}
	if dependencies.GitHub == nil {
		dependencies.GitHub = github.NewClient()
	}
	if dependencies.IssuePoller == nil {
		if poller, ok := dependencies.GitHub.(github.IssuePoller); ok {
			dependencies.IssuePoller = poller
		}
	}
	if dependencies.Lease == nil {
		if lease, ok := dependencies.GitHub.(github.LeaseClient); ok {
			dependencies.Lease = lease
		}
	}
	if dependencies.CommitStatuses == nil {
		if publisher, ok := dependencies.GitHub.(github.CommitStatusPublisher); ok {
			dependencies.CommitStatuses = publisher
		}
	}
	if dependencies.Worktree == nil {
		dependencies.Worktree = gitadapter.NewGitWorkspace()
	}
	if dependencies.GitWorkspace == nil {
		if workspace, ok := dependencies.Worktree.(gitadapter.GitWorkspace); ok {
			dependencies.GitWorkspace = workspace
		}
	}
	if dependencies.PullRequests == nil {
		if client, ok := dependencies.GitHub.(github.PullRequestClient); ok {
			dependencies.PullRequests = client
		}
	}
	if dependencies.Comments == nil {
		if reader, ok := dependencies.GitHub.(github.CommentReader); ok {
			dependencies.Comments = reader
		}
	}
	if dependencies.Worker == nil {
		dependencies.Worker = worker.NewDockerRuntime()
	}
	if dependencies.HarnessCapabilities == nil {
		dependencies.HarnessCapabilities = harness.CapabilitiesFor
	}
	if dependencies.Now == nil {
		dependencies.Now = func() time.Time { return time.Now().UTC() }
	}
	if dependencies.NewRunID == nil {
		dependencies.NewRunID = func() (string, error) { return "run-" + uuid.NewString(), nil }
	}
	if strings.TrimSpace(dependencies.Coordinator) == "" {
		dependencies.Coordinator = defaultCoordinator()
	}
	return &Service{configPath: configPath, deps: dependencies}
}

// ensureTerminalRuntime lazily constructs the default terminal only after
// registration has supplied the cmux socket path. The mutex keeps the first
// construction atomic without mutating a live runtime during a launch. A
// service owns one registered cmux endpoint, so a later conflicting path is
// rejected rather than silently reusing the wrong terminal adapter.
func (s *Service) ensureTerminalRuntime(socketPath string) (terminal.TerminalRuntime, error) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	return s.terminalRuntimeLocked(socketPath)
}

// terminalRuntimeLocked resolves the cached terminal adapter. Callers hold the
// runtime mutex.
func (s *Service) terminalRuntimeLocked(socketPath string) (terminal.TerminalRuntime, error) {
	if s.runtimePathSet && s.runtimeSocketPath != socketPath {
		return nil, fmt.Errorf("cmux socket path %q conflicts with cached path %q", socketPath, s.runtimeSocketPath)
	}
	if !s.runtimePathSet {
		s.runtimeSocketPath = socketPath
		s.runtimePathSet = true
	}
	if s.deps.Terminal == nil {
		s.deps.Terminal = terminal.NewCmuxRuntime(nil, socketPath)
	}
	return s.deps.Terminal, nil
}

// ensureAgentRuntime resolves the terminal and the adapter for one validated
// harness. An explicitly injected harness runtime always wins, so an embedding
// caller keeps one seam; otherwise each selected harness gets its own cached
// adapter.
func (s *Service) ensureAgentRuntime(socketPath string, selected config.Harness) (terminal.TerminalRuntime, harness.Runtime, error) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	terminalRuntime, err := s.terminalRuntimeLocked(socketPath)
	if err != nil {
		return nil, nil, err
	}
	if s.deps.Harness != nil {
		return terminalRuntime, s.deps.Harness, nil
	}
	if cached, exists := s.harnessRuntimes[selected]; exists {
		return terminalRuntime, cached, nil
	}
	harnessRuntime, err := harness.New(string(selected), s.deps.Worker, terminalRuntime)
	if err != nil {
		return nil, nil, err
	}
	if s.harnessRuntimes == nil {
		s.harnessRuntimes = make(map[config.Harness]harness.Runtime, 2)
	}
	s.harnessRuntimes[selected] = harnessRuntime
	return terminalRuntime, harnessRuntime, nil
}

func (s *Service) Init(_ context.Context) (InitResult, error) {
	if s.configPath == "" {
		return InitResult{}, errors.New("host configuration path is required")
	}
	if _, err := s.deps.Config.Create(s.configPath); err != nil {
		return InitResult{}, err
	}
	return InitResult{ConfigPath: s.configPath}, nil
}

func (s *Service) Register(ctx context.Context, request RegisterRequest) (RegisterResult, error) {
	if s.configPath == "" {
		return RegisterResult{}, errors.New("host configuration path is required")
	}
	host, err := s.deps.Config.Load(s.configPath)
	if err != nil {
		return RegisterResult{}, err
	}
	if len(host.Repositories) != 0 {
		return RegisterResult{}, errors.New("version one already has a registered repository")
	}
	if err := s.deps.CheckRepository(request.RepositoryPath); err != nil {
		return RegisterResult{}, err
	}
	repositoryPath, err := filepath.Abs(request.RepositoryPath)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve repository path: %w", err)
	}
	operationalPath := request.OperationalDataPath
	if operationalPath == "" {
		operationalPath = config.DefaultOperationalDataPath(s.configPath)
	}
	operationalPath, err = filepath.Abs(operationalPath)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve operational data path: %w", err)
	}
	repositoryConfigPath := request.RepositoryConfigPath
	if repositoryConfigPath == "" {
		repositoryConfigPath = config.DefaultRepositoryConfigPath(repositoryPath)
	}
	repositoryConfigPath, err = filepath.Abs(repositoryConfigPath)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve repository configuration path: %w", err)
	}
	if err := validateOperationalPath(repositoryPath, operationalPath); err != nil {
		return RegisterResult{}, err
	}
	if !pathWithin(repositoryPath, repositoryConfigPath) {
		return RegisterResult{}, &config.ValidationError{Field: "repositories[0].repository_config_path", Message: "must be inside the registered repository checkout"}
	}
	if _, err := config.LoadRepository(repositoryConfigPath); err != nil {
		return RegisterResult{}, err
	}
	registration := config.RepositoryRegistration{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: strings.TrimSpace(request.GitHubOwner), Repository: strings.TrimSpace(request.GitHubRepository)},
		AuthorizedUsers:      request.AuthorizedUsers,
		Polling:              config.PollingConfig{Interval: defaultString(request.PollingInterval, "30s"), Backoff: defaultString(request.PollingBackoff, "5m")},
		Cmux:                 config.CmuxConfig{SocketPath: request.CmuxSocketPath, ControlWorkspace: request.CmuxControlWorkspace},
		Authentication:       config.AuthenticationConfig{CodexAuthPath: request.CodexAuthPath, ClaudeAuthPath: request.ClaudeAuthPath},
		OperationalDataPath:  operationalPath,
		RepositoryConfigPath: repositoryConfigPath,
	}
	host.Repositories = []config.RepositoryRegistration{registration}
	if err := config.ValidateHost(host); err != nil {
		return RegisterResult{}, err
	}
	opened, err := s.deps.OpenStore(ctx, operationalPath)
	if err != nil {
		return RegisterResult{}, err
	}
	if err := opened.Close(); err != nil {
		return RegisterResult{}, fmt.Errorf("close operational store after initialization: %w", err)
	}
	if err := s.deps.Config.Save(s.configPath, host); err != nil {
		return RegisterResult{}, fmt.Errorf("save host configuration after creating operational store %q: %w", operationalPath, err)
	}
	return RegisterResult{RepositoryPath: repositoryPath, OperationalDataPath: operationalPath}, nil
}

func (s *Service) Status(ctx context.Context) (StatusResult, error) {
	if s.configPath == "" {
		return StatusResult{}, errors.New("host configuration path is required")
	}
	host, err := s.deps.Config.Load(s.configPath)
	if err != nil {
		return StatusResult{}, err
	}
	result := StatusResult{ConfigPath: s.configPath}
	if len(host.Repositories) == 0 {
		return result, nil
	}
	registration := host.Repositories[0]
	result.RepositoryPath = registration.Path
	if err := validateOperationalPath(registration.Path, registration.OperationalDataPath); err != nil {
		return StatusResult{}, err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return StatusResult{}, err
	}
	defer func() { _ = opened.Close() }()
	if latestStore, ok := opened.(LatestRunStore); ok {
		result.LatestRun, err = latestStore.LatestRun(ctx)
	} else {
		result.LatestRun, err = opened.CurrentRun(ctx)
	}
	result.ActiveRun = result.LatestRun
	if err != nil {
		return StatusResult{}, err
	}
	if result.LatestRun != nil {
		result.TestPolicyMode = testPolicyModeForRun(*result.LatestRun)
		var pending *store.PendingEffect
		if journal, ok := opened.(PendingEffectStore); ok {
			pending, err = journal.PendingEffect(ctx, result.LatestRun.ID)
			if err != nil {
				return StatusResult{}, fmt.Errorf("read pending effect for status: %w", err)
			}
		}
		if !store.IsTerminalStatus(result.LatestRun.Status) {
			diagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, opened, *result.LatestRun)
			diagnosis.PendingEffect = pending
			appendPendingEffectAction(&diagnosis)
			result.Recovery = &diagnosis
		} else if pending != nil {
			// A terminal projection can be durable before the final journal clear.
			// Keep that ambiguity visible without downgrading the terminal run or
			// replaying an external effect from the read-only status command.
			diagnosis := newRecoveryDiagnosis(result.LatestRun.ID)
			diagnosis.PendingEffect = pending
			diagnosis.SourcesAgree = false
			appendPendingEffectAction(&diagnosis)
			result.Recovery = &diagnosis
		}
	}
	return result, nil
}

// checkRepository validates that path is an absolute path to an existing directory.
func checkRepository(path string) error {
	if strings.TrimSpace(path) == "" {
		return &config.ValidationError{Field: "repositories[0].path", Message: "is required"}
	}
	if !filepath.IsAbs(path) {
		return &config.ValidationError{Field: "repositories[0].path", Message: "must be absolute"}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect repository path %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository path %q is not a directory", path)
	}
	return nil
}

// defaultString returns fallback when value is blank; otherwise, it returns value.
func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// pathWithin reports whether target is equal to or contained within root after resolving both paths.
func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(resolvePath(root), resolvePath(target))
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// validateOperationalPath verifies that the operational data path is outside the registered repository checkout.
func validateOperationalPath(repositoryPath, operationalPath string) error {
	if pathWithin(repositoryPath, operationalPath) {
		return &config.ValidationError{Field: "repositories[0].operational_data_path", Message: "must be outside the registered repository checkout"}
	}
	return nil
}

// resolvePath returns an absolute, cleaned path with symlinks resolved where possible.
// It preserves unresolved trailing components and falls back to the absolute path when
// resolution fails.
func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	suffix := []string{}
	current := abs
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
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
	{name: "issue", handler: runIssue},
	{name: "status", handler: runStatus},
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
	result, err := factory.New(*configPath).ClaimIssue(ctx, issueNumber)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "claimed issue #%d\nrun: %s\nbranch: %s\nworktree: %s\nstage: %s\nstatus: %s\n", result.Run.IssueNumber, result.Run.ID, result.Run.Branch, result.Run.Worktree, result.Run.Stage, result.Run.Status) {
		return 1
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
		if result.LatestRun.Status == store.StatusComplete || result.LatestRun.Status == store.StatusCancelled || result.LatestRun.Status == store.StatusFailed {
			label = "last run"
		}
		if !writeOutput(output, errorsOutput, "%s: %s (stage=%s status=%s branch=%s worktree=%s)\n", label, result.LatestRun.ID, result.LatestRun.Stage, result.LatestRun.Status, result.LatestRun.Branch, result.LatestRun.Worktree) {
			return 1
		}
	}
	return 0
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

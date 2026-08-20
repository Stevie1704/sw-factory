package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
)

// Run dispatches a CLI command and returns its exit code.
// It supports the init, register, and status commands and reports usage or
// Run dispatches the requested CLI command and returns its exit status.
// It returns 0 for successful execution, 1 for operational errors, and 2 for
// missing or unknown commands. Errors are written to errorsOutput.
func Run(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	if len(args) == 0 {
		writeError(errorsOutput, errors.New("a command is required: init, register, or status"))
		return 2
	}
	configPath, err := config.DefaultHostConfigPath()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	switch args[0] {
	case "init":
		return runInit(ctx, args[1:], configPath, output, errorsOutput)
	case "register":
		return runRegister(ctx, args[1:], configPath, output, errorsOutput)
	case "status":
		return runStatus(ctx, args[1:], configPath, output, errorsOutput)
	default:
		writeError(errorsOutput, fmt.Errorf("unknown command %q: expected init, register, or status", args[0]))
		return 2
	}
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
	fmt.Fprintf(output, "created host configuration: %s\n", result.ConfigPath)
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
	fmt.Fprintf(output, "registered repository: %s\noperational store: %s\n", result.RepositoryPath, result.OperationalDataPath)
	return 0
}

// runStatus displays the host configuration, repository registration, and active-run status.
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
	fmt.Fprintf(output, "config: %s\n", result.ConfigPath)
	if result.RepositoryPath == "" {
		fmt.Fprintln(output, "repository: none registered")
	} else {
		fmt.Fprintf(output, "repository: %s\n", result.RepositoryPath)
	}
	if result.ActiveRun == nil {
		fmt.Fprintln(output, "active run: none")
	} else {
		fmt.Fprintf(output, "active run: %s (stage=%s status=%s)\n", result.ActiveRun.ID, result.ActiveRun.Stage, result.ActiveRun.Status)
	}
	return 0
}

// writeError writes a non-nil error to output using the format "error: <message>".
func writeError(output io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(output, "error: %v\n", err)
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

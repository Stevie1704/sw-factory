package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	swfactory "github.com/Stevie1704/sw-factory"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// onboardingProcedure is the resource-relative path of the procedure the
// onboarding agent follows.
const onboardingProcedure = "docs/repository-initialization.md"

// pinnedImage matches an image reference pinned by a sha256 digest, which is
// the only form a setup step may build from.
var pinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)

// onboardingPrompt is the first message of the onboarding session. The
// procedure document owns the steps and the approval list; the prompt names
// the paths, the base image, the factory command, and the rules that apply
// before the agent has read the document.
const onboardingPrompt = `Prepare the repository at %[1]s for Software Factory runs.

Run every factory command as %[5]s, the build that started this
session; the procedure calls it <FACTORY_COMMAND>. A factory on PATH can be
another build, so do not use it.

Run %[5]s guide and %[5]s guide setup first; they describe the installed
release. The installed release's resources are at %[2]s. Read
%[2]s/%[3]s in full and follow its ordered procedure from
the first step to the last. In the procedure, <FACTORY_RESOURCES> means
%[2]s. The worker base image is %[4]s; the procedure calls it
<FACTORY_BASE_IMAGE>. Run every step yourself; do not hand steps back to me.

The resource directory is removed when this session ends. Copy what the
target repository keeps, and never reference the directory from a committed
file.

Use its field reference rather than another repository's factory.yaml; where
the two disagree, docs/configuration.md decides.

Stop and ask me before each action that its section "Actions that need
operator approval" lists. Never read a credential file and never invent a
credential path; if one is missing, stop and ask. Never commit an absolute
host path into factory.yaml.
`

// runOnboard starts an interactive harness session in the current repository
// with a prompt that gives the whole repository initialization to the agent.
func runOnboard(ctx context.Context, args []string, _ string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("onboard", errorsOutput)
	harness := flags.String("harness", string(config.HarnessClaude), "interactive harness that runs the onboarding: claude or codex")
	baseImage := flags.String("worker-base-image", version.Current().WorkerBaseImage, "worker base image as IMAGE@sha256:DIGEST; defaults to the image the release names")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("onboard does not accept positional arguments"))
		return 2
	}
	selected := config.Harness(*harness)
	if selected != config.HarnessClaude && selected != config.HarnessCodex {
		writeError(errorsOutput, fmt.Errorf("--harness must be claude or codex, got %q", *harness))
		return 2
	}
	if *baseImage == "" {
		writeError(errorsOutput, errors.New("this build names no released worker base image, because it is a development build or was not installed from a release archive: install a Factory release archive, or build a base image with make worker-build and pass --worker-base-image IMAGE@sha256:DIGEST"))
		return 1
	}
	if !pinnedImage.MatchString(*baseImage) {
		writeError(errorsOutput, fmt.Errorf("--worker-base-image must be pinned as IMAGE@sha256:DIGEST, got %q", *baseImage))
		return 2
	}
	targetPath, err := gitTopLevel(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	command, err := factoryCommand()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	resourcesPath, err := exportOnboardingResources()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(resourcesPath) }()
	if err := startOnboardingSession(ctx, selected, onboardingSession{targetPath, resourcesPath, *baseImage, command}, output, errorsOutput); err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	return 0
}

// factoryCommand returns the absolute path of the running factory executable.
// The harness shell can load a login profile that puts another factory build
// first on PATH, so the session names this exact build.
func factoryCommand() (string, error) {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return "", fmt.Errorf("locate the running factory executable for the onboarding session: %w", err)
	}
	return executable, nil
}

// onboardingSession holds the values the onboarding prompt names.
type onboardingSession struct {
	targetPath     string
	resourcesPath  string
	baseImage      string
	factoryCommand string
}

// exportOnboardingResources writes the installed release's resources into a
// new directory below the user cache directory. The cache directory lies in
// the operator's home, which a VM-backed Docker daemon can bind-mount, and a
// new directory per session keeps resources of different releases apart.
func exportOnboardingResources() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate the user cache directory for the onboarding resources; set HOME: %w", err)
	}
	parent := filepath.Join(cache, "sw-factory")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create the onboarding resource directory: %w", err)
	}
	directory, err := os.MkdirTemp(parent, "onboard-")
	if err != nil {
		return "", fmt.Errorf("create the onboarding resource directory: %w", err)
	}
	if err := swfactory.ExportResources(directory); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	return directory, nil
}

// startOnboardingSession runs the interactive harness in the target repository
// until the operator ends the session. The resource directory is added as a
// second working directory, because the procedure copies skills and runs
// scripts from it.
func startOnboardingSession(ctx context.Context, harness config.Harness, values onboardingSession, output, errorsOutput io.Writer) error {
	prompt := fmt.Sprintf(onboardingPrompt, values.targetPath, values.resourcesPath, onboardingProcedure, values.baseImage, values.factoryCommand)
	// The prompt comes first: Claude Code's --add-dir accepts several values and
	// would take a later positional prompt as a directory.
	session := exec.CommandContext(ctx, string(harness), prompt, "--add-dir", values.resourcesPath)
	session.Dir = values.targetPath
	// The command handler signature carries no input stream; the session is
	// interactive, so it reads the operator's terminal directly.
	session.Stdin = os.Stdin
	session.Stdout = output
	session.Stderr = errorsOutput
	if err := session.Run(); err != nil {
		return fmt.Errorf("onboarding session with %s: %w", harness, err)
	}
	return nil
}

// gitTopLevel returns the root of the Git checkout that contains the current
// directory, which is the repository to onboard.
func gitTopLevel(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("run onboard inside the Git checkout of the repository to onboard: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// factoryCheckout is the Software Factory checkout the binary was built from.
// The Makefile sets it at link time, so an installed factory can find the
// procedure, the worker skills, and the scripts that onboarding uses.
var factoryCheckout string

// onboardingProcedure is the checkout-relative path of the procedure the
// onboarding agent follows.
const onboardingProcedure = "docs/repository-initialization.md"

// onboardingPrompt is the first message of the onboarding session. The
// procedure document owns the steps and the approval list; the prompt names
// the two paths and the rules that apply before the agent has read the
// document.
const onboardingPrompt = `Prepare the repository at %[1]s for Software Factory runs.

The Software Factory checkout is at %[2]s. Read
%[2]s/%[3]s in full and follow its ordered procedure from
the first step to the last. Run every step yourself; do not hand steps back to me.

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
	flags := flag.NewFlagSet("onboard", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	harness := flags.String("harness", string(config.HarnessClaude), "interactive harness that runs the onboarding: claude or codex")
	checkout := flags.String("factory-checkout", factoryCheckout, "Software Factory checkout; defaults to the checkout the binary was built from")
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
	checkoutPath, err := resolveFactoryCheckout(*checkout)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	targetPath, err := gitTopLevel(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if err := startOnboardingSession(ctx, selected, targetPath, checkoutPath, output, errorsOutput); err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	return 0
}

// startOnboardingSession runs the interactive harness in the target repository
// until the operator ends the session. The checkout is added as a second
// working directory, because the procedure builds images and runs scripts
// from it.
func startOnboardingSession(ctx context.Context, harness config.Harness, targetPath, checkoutPath string, output, errorsOutput io.Writer) error {
	prompt := fmt.Sprintf(onboardingPrompt, targetPath, checkoutPath, onboardingProcedure)
	// The prompt comes first: Claude Code's --add-dir accepts several values and
	// would take a later positional prompt as a directory.
	session := exec.CommandContext(ctx, string(harness), prompt, "--add-dir", checkoutPath)
	session.Dir = targetPath
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

// resolveFactoryCheckout returns the absolute checkout path after it confirms
// that the checkout contains the onboarding procedure.
func resolveFactoryCheckout(path string) (string, error) {
	if path == "" {
		return "", errors.New("the Software Factory checkout is unknown: install with make install, or pass --factory-checkout")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve --factory-checkout: %w", err)
	}
	if _, err := os.Stat(filepath.Join(absolute, onboardingProcedure)); err != nil {
		return "", fmt.Errorf("%s does not contain %s; when the checkout moved, run make install from its new location, or pass --factory-checkout: %w", absolute, onboardingProcedure, err)
	}
	return absolute, nil
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

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
)

// factoryCheckout is the Software Factory checkout the binary was built from.
// The Makefile sets it at link time, so an installed factory can find the
// procedure, the worker skills, and the scripts that onboarding uses.
var factoryCheckout string

// onboardingProcedure is the checkout-relative path of the procedure the
// onboarding agent follows.
const onboardingProcedure = "docs/repository-initialization.md"

// onboardingPrompt is the first message of the onboarding session. The
// procedure document owns the steps; the prompt only names the two paths and
// the actions that need the operator's approval.
const onboardingPrompt = `Prepare the repository at %[1]s for Software Factory runs.

The Software Factory checkout is at %[2]s. Read
%[2]s/%[3]s in full and follow its ordered procedure from
start to finish. Run every step yourself; do not hand steps back to me.

Stop and ask me before:
- writing any file in the target repository,
- running factory init, factory register, or factory bootstrap-labels,
- running scripts/smoke-skills.sh,
- committing anything.

Never read a credential file. Never commit an absolute host path into
factory.yaml. Finish when factory doctor reports ready.
`

// runOnboard starts an interactive harness session in the current repository
// with a prompt that hands the whole repository initialization to the agent.
func runOnboard(ctx context.Context, args []string, _ string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("onboard", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	harness := flags.String("harness", "claude", "interactive harness that runs the onboarding: claude or codex")
	checkout := flags.String("factory-checkout", factoryCheckout, "Software Factory checkout; defaults to the checkout the binary was built from")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("onboard does not accept positional arguments"))
		return 2
	}
	if *harness != "claude" && *harness != "codex" {
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
	session := exec.CommandContext(ctx, *harness, fmt.Sprintf(onboardingPrompt, targetPath, checkoutPath, onboardingProcedure))
	session.Dir = targetPath
	session.Stdin = os.Stdin
	session.Stdout = output
	session.Stderr = errorsOutput
	if err := session.Run(); err != nil {
		writeError(errorsOutput, fmt.Errorf("onboarding session with %s: %w", *harness, err))
		return 1
	}
	return 0
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
		return "", fmt.Errorf("%s is not a Software Factory checkout: %w", absolute, err)
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

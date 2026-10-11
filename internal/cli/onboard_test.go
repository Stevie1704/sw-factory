package cli_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/cli"
)

// testBaseImage is a well-formed digest-pinned worker base image.
var testBaseImage = "ghcr.io/stevie1704/sw-factory-base@sha256:" + strings.Repeat("cd", 32)

// onboardFixture creates a target Git checkout, a home directory whose path
// contains spaces, and a fake harness on PATH. The harness records its working
// directory, its options, whether the procedure exists in the resource
// directory it was given, and its prompt.
func onboardFixture(t *testing.T, harness string) (target, record string) {
	t.Helper()
	root := t.TempDir()
	target = filepath.Join(root, "target repo")
	home := filepath.Join(root, "operator home")
	bin := filepath.Join(root, "bin")
	record = filepath.Join(root, "record")
	for _, directory := range []string{target, home, bin} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := exec.Command("git", "init", "-q", target).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	script := "#!/bin/sh\n{ pwd; printf '%s\\n' \"$2\" \"$3\"; " +
		"if test -f \"$3/docs/repository-initialization.md\" && test -x \"$3/scripts/smoke-skills.sh\"; then echo present; else echo absent; fi; " +
		"printf '%s' \"$1\"; } > '" + record + "'\n"
	if err := os.WriteFile(filepath.Join(bin, harness), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Chdir(target)
	return target, record
}

// TestOnboardGivesTheHarnessTheReleaseResources proves that onboard starts the
// selected harness in the target checkout with a directory of the installed
// release's resources, names the pinned worker base image and its own
// executable in the prompt, and removes the directory after the session.
func TestOnboardGivesTheHarnessTheReleaseResources(t *testing.T) {
	target, record := onboardFixture(t, "codex")
	var output, errorsOutput bytes.Buffer

	status := cli.Run(context.Background(), []string{"onboard", "--harness", "codex", "--worker-base-image", testBaseImage}, &output, &errorsOutput)

	if status != 0 {
		t.Fatalf("status = %d, stderr = %s", status, errorsOutput.String())
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(recorded), "\n", 5)
	if len(lines) != 5 {
		t.Fatalf("recorded harness call is incomplete: %q", recorded)
	}
	directory, option, resources, presence, prompt := lines[0], lines[1], lines[2], lines[3], lines[4]
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if directory != resolvedTarget {
		t.Errorf("harness ran in %q, want %q", directory, resolvedTarget)
	}
	if option != "--add-dir" || presence != "present" {
		t.Errorf("harness got %s %q with resources %s, want --add-dir and the exported procedure and scripts", option, resources, presence)
	}
	if !strings.HasPrefix(resources, os.Getenv("HOME")+string(filepath.Separator)) {
		t.Errorf("resources %q are outside the operator home, where a VM-backed Docker daemon cannot mount them", resources)
	}
	if _, err := os.Stat(resources); !os.IsNotExist(err) {
		t.Errorf("resources %q remain after the session: %v", resources, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Prepare the repository at " + resolvedTarget,
		"Run every factory command as " + command,
		"<FACTORY_COMMAND>",
		resources + "/docs/repository-initialization.md",
		"The worker base image is " + testBaseImage,
		"Actions that need",
		"docs/configuration.md decides",
		"never invent a",
		"Run " + command + " guide and " + command + " guide setup first",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

// TestOnboardReportsAnUnwritableResourceDirectory proves that onboard names
// the resource directory, and starts no session, when it cannot export the
// release resources.
func TestOnboardReportsAnUnwritableResourceDirectory(t *testing.T) {
	_, record := onboardFixture(t, "claude")
	home := filepath.Join(t.TempDir(), "home is a file")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	var output, errorsOutput bytes.Buffer

	status := cli.Run(context.Background(), []string{"onboard", "--worker-base-image", testBaseImage}, &output, &errorsOutput)

	if status != 1 || !strings.Contains(errorsOutput.String(), "onboarding resource directory") {
		t.Errorf("status = %d, stderr = %q; want 1 and the resource directory", status, errorsOutput.String())
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("onboarding without resources started the harness: %v", err)
	}
}

// TestOnboardRejectsInvalidInput proves that onboard refuses an unknown
// harness, a positional argument, and a missing or mutable worker base image
// before it starts a session, and names the action that fixes each.
func TestOnboardRejectsInvalidInput(t *testing.T) {
	_, record := onboardFixture(t, "claude")
	tests := []struct {
		name   string
		args   []string
		status int
		stderr string
	}{
		{"unknown harness", []string{"--harness", "cursor", "--worker-base-image", testBaseImage}, 2, "--harness must be claude or codex"},
		{"positional argument", []string{"extra"}, 2, "does not accept positional arguments"},
		{"development build without base image", []string{}, 1, "pass --worker-base-image"},
		{"mutable tag", []string{"--worker-base-image", "ghcr.io/stevie1704/sw-factory-base:v1"}, 2, "must be pinned as IMAGE@sha256:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output, errorsOutput bytes.Buffer
			status := cli.Run(context.Background(), append([]string{"onboard"}, test.args...), &output, &errorsOutput)
			if status != test.status || !strings.Contains(errorsOutput.String(), test.stderr) {
				t.Errorf("status = %d, stderr = %q; want %d and %q", status, errorsOutput.String(), test.status, test.stderr)
			}
		})
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("a refused onboarding started the harness: %v", err)
	}
}

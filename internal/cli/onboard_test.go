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

// onboardFixture creates a Software Factory checkout, a target Git checkout,
// and a fake harness on PATH that records its working directory and prompt.
func onboardFixture(t *testing.T, harness string) (checkout, target, record string) {
	t.Helper()
	root := t.TempDir()
	checkout = filepath.Join(root, "sw-factory")
	target = filepath.Join(root, "target")
	bin := filepath.Join(root, "bin")
	record = filepath.Join(root, "record")
	for _, directory := range []string{filepath.Join(checkout, "docs"), target, bin} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(checkout, "docs", "repository-initialization.md"), []byte("# procedure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", target).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	script := "#!/bin/sh\n{ pwd; printf '%s\\n' \"$2\" \"$3\"; printf '%s' \"$1\"; } > " + record + "\n"
	if err := os.WriteFile(filepath.Join(bin, harness), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(target)
	return checkout, target, record
}

// TestOnboardStartsHarnessInTargetWithResolvedPaths proves that onboard starts
// the selected harness in the target checkout, gives it access to the factory
// checkout, and fills both paths into the prompt.
func TestOnboardStartsHarnessInTargetWithResolvedPaths(t *testing.T) {
	checkout, target, record := onboardFixture(t, "codex")
	var output, errorsOutput bytes.Buffer

	status := cli.Run(context.Background(), []string{"onboard", "--harness", "codex", "--factory-checkout", checkout}, &output, &errorsOutput)

	if status != 0 {
		t.Fatalf("status = %d, stderr = %s", status, errorsOutput.String())
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(recorded), "\n", 4)
	if len(lines) != 4 {
		t.Fatalf("recorded harness call is incomplete: %q", recorded)
	}
	directory, prompt := lines[0], lines[3]
	if lines[1] != "--add-dir" || lines[2] != checkout {
		t.Errorf("harness options = %q %q, want --add-dir %q", lines[1], lines[2], checkout)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if directory != resolvedTarget {
		t.Errorf("harness ran in %q, want %q", directory, resolvedTarget)
	}
	for _, want := range []string{
		"Prepare the repository at " + resolvedTarget,
		checkout + "/docs/repository-initialization.md",
		"Actions that need",
		"docs/configuration.md decides",
		"never invent a",
		"Run factory guide and factory guide setup first",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

// TestOnboardRejectsInvalidInput proves that onboard refuses an unknown
// harness, a positional argument, and a missing or wrong factory checkout
// before it starts a session.
func TestOnboardRejectsInvalidInput(t *testing.T) {
	checkout, _, _ := onboardFixture(t, "claude")
	tests := []struct {
		name   string
		args   []string
		status int
		stderr string
	}{
		{"unknown harness", []string{"--harness", "cursor", "--factory-checkout", checkout}, 2, "--harness must be claude or codex"},
		{"positional argument", []string{"extra"}, 2, "does not accept positional arguments"},
		{"unknown checkout", []string{}, 1, "the Software Factory checkout is unknown"},
		{"not a checkout", []string{"--factory-checkout", t.TempDir()}, 1, "does not contain docs/repository-initialization.md"},
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
}

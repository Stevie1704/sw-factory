package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
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
// directory, its options, whether the resource directory it was given is a
// complete preparation with the procedure and scripts, and its prompt.
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
		"if test -f \"$3/.factory-resources\" && test -f \"$3/docs/repository-initialization.md\" && test -x \"$3/scripts/smoke-skills.sh\"; then echo present; else echo absent; fi; " +
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
// selected harness in the target checkout with a directory that the shared
// preparation path of onboard prepare completed, names the pinned worker base image and its own
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
		t.Errorf("harness got %s %q with resources %s, want --add-dir and a completed preparation", option, resources, presence)
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

// onboardingManifest is the documented shape of the prepare manifest.
type onboardingManifest struct {
	SchemaVersion    int    `json:"schema_version"`
	FactoryVersion   string `json:"factory_version"`
	FactoryRevision  string `json:"factory_revision"`
	FactoryCommand   string `json:"factory_command"`
	WorkerBaseImage  string `json:"worker_base_image"`
	Procedure        string `json:"procedure"`
	Resources        string `json:"resources"`
	ResourceIdentity string `json:"resource_identity"`
}

// prepareFixture isolates a host without registration: an empty home whose
// path contains spaces, a working directory outside any checkout, and another
// factory build first on PATH that records every call.
func prepareFixture(t *testing.T) (root, otherFactoryRecord string) {
	t.Helper()
	root = t.TempDir()
	home := filepath.Join(root, "operator home")
	bin := filepath.Join(root, "other factory bin")
	work := filepath.Join(root, "work dir")
	for _, directory := range []string{home, bin, work} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	otherFactoryRecord = filepath.Join(root, "other factory record")
	script := "#!/bin/sh\necho called > '" + otherFactoryRecord + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "factory"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("FACTORY_CONFIG", "")
	t.Chdir(work)
	return root, otherFactoryRecord
}

// runPrepare runs onboard prepare with JSON output and decodes the manifest.
func runPrepare(t *testing.T, args ...string) (onboardingManifest, int, string) {
	t.Helper()
	var output, errorsOutput bytes.Buffer
	status := cli.Run(context.Background(), append([]string{"onboard", "prepare", "--json"}, args...), &output, &errorsOutput)
	var manifest onboardingManifest
	if status == 0 {
		if err := json.Unmarshal(output.Bytes(), &manifest); err != nil {
			t.Fatalf("manifest %q: %v", output.String(), err)
		}
	}
	return manifest, status, errorsOutput.String()
}

// runningFactory returns the resolved path of the running test executable,
// which stands in for the factory binary.
func runningFactory(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

// TestOnboardPrepareExportsTheReleaseResources proves that prepare, on a host
// without registration and outside any checkout, exports the release
// resources into a path with spaces, keeps them after it returns, and names
// the running executable rather than another factory on PATH.
func TestOnboardPrepareExportsTheReleaseResources(t *testing.T) {
	root, otherFactory := prepareFixture(t)
	destination := filepath.Join(root, "agent resources", "factory onboarding")

	manifest, status, stderr := runPrepare(t, "--output", destination, "--worker-base-image", testBaseImage)

	if status != 0 {
		t.Fatalf("status = %d, stderr = %s", status, stderr)
	}
	if manifest.SchemaVersion != 1 || manifest.FactoryVersion != "development" || manifest.FactoryRevision == "" {
		t.Errorf("manifest identity = %+v, want schema 1 of a development build with a revision", manifest)
	}
	if manifest.FactoryCommand != runningFactory(t) {
		t.Errorf("factory_command = %q, want the running executable %q", manifest.FactoryCommand, runningFactory(t))
	}
	if manifest.WorkerBaseImage != testBaseImage || manifest.Resources != destination {
		t.Errorf("manifest = %+v, want base image %s and resources %s", manifest, testBaseImage, destination)
	}
	if manifest.Procedure != filepath.Join(destination, "docs", "repository-initialization.md") {
		t.Errorf("procedure = %q, want the exported repository initialization", manifest.Procedure)
	}
	if !strings.HasPrefix(manifest.ResourceIdentity, "sha256:") || len(manifest.ResourceIdentity) != len("sha256:")+64 {
		t.Errorf("resource_identity = %q, want sha256:<64 hex>", manifest.ResourceIdentity)
	}
	for _, path := range []string{manifest.Procedure, filepath.Join(destination, "operator", "skills", "factory-onboarding", "SKILL.md"), filepath.Join(destination, "worker", "skills", "tdd", "SKILL.md")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("prepared resources lack %s after prepare returned: %v", path, err)
		}
	}
	if info, err := os.Stat(filepath.Join(destination, "scripts", "smoke-skills.sh")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("smoke script is missing or not executable: %v", err)
	}
	if _, err := os.Stat(otherFactory); !os.IsNotExist(err) {
		t.Errorf("prepare called the factory on PATH: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config")); !os.IsNotExist(err) {
		t.Errorf("prepare wrote host configuration: %v", err)
	}
}

// TestOnboardPrepareAcceptsItsOwnEarlierPreparation proves that an agent whose
// session was interrupted can prepare the same directory again and gets the
// same resources, untouched.
func TestOnboardPrepareAcceptsItsOwnEarlierPreparation(t *testing.T) {
	root, _ := prepareFixture(t)
	destination := filepath.Join(root, "resources")
	first, status, stderr := runPrepare(t, "--output", destination, "--worker-base-image", testBaseImage)
	if status != 0 {
		t.Fatalf("first prepare: status = %d, stderr = %s", status, stderr)
	}

	second, status, stderr := runPrepare(t, "--output", destination, "--worker-base-image", testBaseImage)

	if status != 0 || second != first {
		t.Errorf("second prepare: status = %d, stderr = %s, manifest = %+v; want the first manifest %+v", status, stderr, second, first)
	}
}

// TestOnboardPrepareRefusesIncompatibleDestinations proves that prepare never
// mixes releases, never overwrites or reuses changed resources, and never
// writes into a directory with unrelated files.
func TestOnboardPrepareRefusesIncompatibleDestinations(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, destination string)
		file    string
		content string
	}{
		{"unrelated files", func(t *testing.T, destination string) {}, "notes.txt", "operator notes\n"},
		{"another release", func(t *testing.T, destination string) {}, ".factory-resources", "sha256:" + strings.Repeat("0", 64) + "\n"},
		{"changed resource", func(t *testing.T, destination string) {
			if _, status, stderr := runPrepare(t, "--output", destination, "--worker-base-image", testBaseImage); status != 0 {
				t.Fatalf("first prepare: status = %d, stderr = %s", status, stderr)
			}
		}, "docs/repository-initialization.md", "a hand-edited procedure\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, _ := prepareFixture(t)
			destination := filepath.Join(root, "resources")
			test.arrange(t, destination)
			path := filepath.Join(destination, filepath.FromSlash(test.file))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.content), 0o644); err != nil {
				t.Fatal(err)
			}

			_, status, stderr := runPrepare(t, "--output", destination, "--worker-base-image", testBaseImage)

			if status != 1 || !strings.Contains(stderr, "choose a new directory") {
				t.Errorf("status = %d, stderr = %q; want 1 and the advice to choose a new directory", status, stderr)
			}
			if got, _ := os.ReadFile(path); string(got) != test.content {
				t.Errorf("prepare changed %s to %q", test.file, got)
			}
		})
	}
}

// TestOnboardPrepareReportsAnUnavailableDirectory proves that prepare names
// the resource directory when it cannot create it.
func TestOnboardPrepareReportsAnUnavailableDirectory(t *testing.T) {
	root, _ := prepareFixture(t)
	blocker := filepath.Join(root, "a file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, status, stderr := runPrepare(t, "--output", filepath.Join(blocker, "resources"), "--worker-base-image", testBaseImage)

	if status != 1 || !strings.Contains(stderr, "Factory resource directory") {
		t.Errorf("status = %d, stderr = %q; want 1 and the resource directory", status, stderr)
	}
}

// TestOnboardPrepareRejectsInvalidInput proves that prepare refuses a missing
// output, a positional argument, and a missing or mutable worker base image
// before it writes anything.
func TestOnboardPrepareRejectsInvalidInput(t *testing.T) {
	root, _ := prepareFixture(t)
	destination := filepath.Join(root, "resources")
	tests := []struct {
		name   string
		args   []string
		status int
		stderr string
	}{
		{"missing output", []string{"--worker-base-image", testBaseImage}, 2, "needs --output"},
		{"positional argument", []string{"--output", destination, "--worker-base-image", testBaseImage, "extra"}, 2, "does not accept positional arguments"},
		{"development build without base image", []string{"--output", destination}, 1, "pass --worker-base-image"},
		{"mutable tag", []string{"--output", destination, "--worker-base-image", "ghcr.io/stevie1704/sw-factory-base:v1"}, 2, "must be pinned as IMAGE@sha256:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, status, stderr := runPrepare(t, test.args...)
			if status != test.status || !strings.Contains(stderr, test.stderr) {
				t.Errorf("status = %d, stderr = %q; want %d and %q", status, stderr, test.status, test.stderr)
			}
		})
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Errorf("a refused prepare wrote %s: %v", destination, err)
	}
}

// TestOnboardingSkillNamesThePreparedContract proves that the shipped operator
// skill tells an agent to prepare the resources with the running release and
// names only manifest fields that prepare writes, so it stays usable as plain
// instructions and cannot drift from the manifest.
func TestOnboardingSkillNamesThePreparedContract(t *testing.T) {
	root, _ := prepareFixture(t)
	destination := filepath.Join(root, "resources")
	var output, errorsOutput bytes.Buffer
	if status := cli.Run(context.Background(), []string{"onboard", "prepare", "--json", "--output", destination, "--worker-base-image", testBaseImage}, &output, &errorsOutput); status != 0 {
		t.Fatalf("status = %d, stderr = %s", status, errorsOutput.String())
	}
	var fields map[string]any
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(filepath.Join(destination, "operator", "skills", "factory-onboarding", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}

	text := string(skill)
	if !strings.HasPrefix(text, "---\nname: factory-onboarding\ndescription: ") {
		t.Errorf("skill lacks the factory-onboarding front matter:\n%s", text)
	}
	if !strings.Contains(text, "onboard prepare --output") || !strings.Contains(text, "--json") {
		t.Errorf("skill does not prepare the resources with onboard prepare --json")
	}
	for _, field := range []string{"factory_command", "resources", "worker_base_image", "procedure"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("manifest lacks %q", field)
		}
		if !strings.Contains(text, "`"+field+"`") {
			t.Errorf("skill does not name the manifest field %q", field)
		}
	}
}

package worker_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The launcher names its three fixed in-worker paths on these lines. The
// tests relocate only these lines, so the script under test is otherwise the
// one the image installs.
const (
	launcherCredentialLine = "credential_file=/run/factory-auth/copilot-token"
	launcherSettingsLine   = "managed_settings=/usr/local/share/factory-copilot/settings.json"
	launcherHomeLine       = "copilot_home=/home/factory/.copilot"
)

// copilotLauncher is one relocated copy of worker/factory-copilot with a fake
// copilot executable on PATH that records what it received.
type copilotLauncher struct {
	script     string
	credential string
	home       string
	record     string
	path       string
}

// newCopilotLauncher relocates the launcher's fixed paths into a temporary
// root and installs a fake copilot that records its arguments and the
// environment the real CLI would read.
func newCopilotLauncher(t *testing.T) copilotLauncher {
	t.Helper()
	root := t.TempDir()
	body := readRepositoryFile(t, "worker/factory-copilot")
	launcher := copilotLauncher{
		script:     filepath.Join(root, "factory-copilot"),
		credential: filepath.Join(root, "auth", "copilot-token"),
		home:       filepath.Join(root, "home", ".copilot"),
		record:     filepath.Join(root, "record"),
		path:       filepath.Join(root, "bin"),
	}
	settings, err := filepath.Abs("copilot-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	for line, replacement := range map[string]string{
		launcherCredentialLine: "credential_file=" + launcher.credential,
		launcherSettingsLine:   "managed_settings=" + settings,
		launcherHomeLine:       "copilot_home=" + launcher.home,
	} {
		if strings.Count(body, "\n"+line+"\n") != 1 {
			t.Fatalf("worker/factory-copilot must declare %q exactly once", line)
		}
		body = strings.Replace(body, "\n"+line+"\n", "\n"+replacement+"\n", 1)
	}
	if err := os.WriteFile(launcher.script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(launcher.credential), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(launcher.path, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\n" +
		"{ for argument in \"$@\"; do printf 'arg=%s\\n' \"$argument\"; done; env | sort; } > '" + launcher.record + "'\n" +
		"printf 'copilot ran\\n'\n"
	if err := os.WriteFile(filepath.Join(launcher.path, "copilot"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	return launcher
}

// run starts the launcher with arguments and the extra environment, from a
// minimal base environment like the worker's env -i launch.
func (l copilotLauncher) run(t *testing.T, environment []string, arguments ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(l.script, arguments...)
	command.Env = append([]string{"PATH=" + l.path + ":/usr/bin:/bin", "HOME=" + filepath.Dir(l.home)}, environment...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run launcher: %v", err)
	}
	return stdout.String(), stderr.String(), code
}

// TestCopilotLauncherRefusesAMissingOrEmptyCredential verifies the launcher
// fails with the one seam line and exit 78 when no usable token is projected,
// without starting Copilot or naming the projection.
func TestCopilotLauncherRefusesAMissingOrEmptyCredential(t *testing.T) {
	for _, test := range []struct {
		name     string
		contents *string
	}{
		{name: "missing"},
		{name: "empty", contents: new(string)},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher := newCopilotLauncher(t)
			if test.contents != nil {
				if err := os.WriteFile(launcher.credential, []byte(*test.contents), 0o444); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, code := launcher.run(t, nil, "--version")
			if code != 78 || stderr != "factory-copilot: credential unavailable\n" || stdout != "" {
				t.Fatalf("launcher = exit %d stdout %q stderr %q, want exit 78 and the one seam line", code, stdout, stderr)
			}
			if _, err := os.Stat(launcher.record); !os.IsNotExist(err) {
				t.Fatal("launcher started Copilot without a credential")
			}
		})
	}
}

// TestCopilotLauncherRefusesAnUnreadableCredential verifies a projection the
// worker user cannot read is reported as unavailable without the shell's own
// error text, which would name the projection path.
func TestCopilotLauncherRefusesAnUnreadableCredential(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	launcher := newCopilotLauncher(t)
	if err := os.WriteFile(launcher.credential, []byte("github_pat_test-only\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := launcher.run(t, nil, "--version")
	if code != 78 || stderr != "factory-copilot: credential unavailable\n" {
		t.Fatalf("launcher = exit %d stderr %q, want exit 78 and the one seam line", code, stderr)
	}
}

// TestCopilotLauncherExportsOnlyTheProjectedTokenToCopilot verifies the
// launcher passes the adapter argv through unchanged, exports the projected
// token only as COPILOT_GITHUB_TOKEN, pins the private Copilot home and the
// update switch, and removes every other GitHub token and Copilot, telemetry,
// or CI control the caller's environment carried.
func TestCopilotLauncherExportsOnlyTheProjectedTokenToCopilot(t *testing.T) {
	launcher := newCopilotLauncher(t)
	if err := os.WriteFile(launcher.credential, []byte("github_pat_test-only\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"--prompt=Implement it; keep \"quotes\" and  spaces", "--output-format=json", "--allow-all-tools",
		"--secret-env-vars=COPILOT_GITHUB_TOKEN", "--session-id=5d1f2a83-0c4e-4f7a-9b2e-6a1c8d3e5f70",
	}
	stdout, stderr, code := launcher.run(t, []string{
		"FACTORY_INVOCATION_ID=invocation-1",
		"COPILOT_AUTO_UPDATE=false",
		"GH_TOKEN=host-gh", "GITHUB_TOKEN=host-github", "GH_ENTERPRISE_TOKEN=host-enterprise", "GITHUB_ASKPASS=/bin/true",
		"COPILOT_ALLOW_ALL=true", "COPILOT_MODEL=other", "COPILOT_HOME=/elsewhere", "COPILOT_GITHUB_TOKEN=planted",
		"COPILOT_CUSTOM_INSTRUCTIONS_DIRS=/work", "OTEL_EXPORTER_OTLP_ENDPOINT=http://collector", "CI=true",
	}, arguments...)
	if code != 0 || stdout != "copilot ran\n" || stderr != "" {
		t.Fatalf("launcher = exit %d stdout %q stderr %q, want Copilot's own result", code, stdout, stderr)
	}
	record, err := os.ReadFile(launcher.record)
	if err != nil {
		t.Fatal(err)
	}
	var gotArguments []string
	environment := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(record), "\n"), "\n") {
		if argument, ok := strings.CutPrefix(line, "arg="); ok {
			gotArguments = append(gotArguments, argument)
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok {
			environment[name] = value
		}
	}
	if strings.Join(gotArguments, "\x00") != strings.Join(arguments, "\x00") {
		t.Fatalf("Copilot arguments = %q, want %q unchanged", gotArguments, arguments)
	}
	for name, want := range map[string]string{
		"COPILOT_GITHUB_TOKEN":  "github_pat_test-only",
		"COPILOT_HOME":          launcher.home,
		"COPILOT_AUTO_UPDATE":   "false",
		"FACTORY_INVOCATION_ID": "invocation-1",
	} {
		if environment[name] != want {
			t.Errorf("Copilot environment %s = %q, want %q", name, environment[name], want)
		}
	}
	for _, name := range []string{
		"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ASKPASS", "COPILOT_ALLOW_ALL", "COPILOT_MODEL",
		"COPILOT_CUSTOM_INSTRUCTIONS_DIRS", "OTEL_EXPORTER_OTLP_ENDPOINT", "CI",
	} {
		if value, ok := environment[name]; ok {
			t.Errorf("Copilot environment kept %s=%q", name, value)
		}
	}
}

// TestCopilotLauncherRestoresTheManagedConfigurationBeforeEveryLaunch
// verifies an earlier session cannot leave user-level settings, MCP servers,
// hooks, or plugins behind for the next launch, while the native session state
// stays in place for resume.
func TestCopilotLauncherRestoresTheManagedConfigurationBeforeEveryLaunch(t *testing.T) {
	launcher := newCopilotLauncher(t)
	if err := os.WriteFile(launcher.credential, []byte("github_pat_test-only"), 0o444); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(launcher.home, "session-state", "5d1f2a83-0c4e-4f7a-9b2e-6a1c8d3e5f70", "events.jsonl")
	planted := map[string]string{
		"settings.json":                   `{"disableAllHooks":false,"trustedFolders":["/work"]}`,
		"mcp-config.json":                 `{"mcpServers":{"planted":{"command":"touch"}}}`,
		"hooks/planted.json":              `{"hooks":{}}`,
		"installed-plugins/planted/x.txt": "planted",
	}
	for relative, contents := range planted {
		path := filepath.Join(launcher.home, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(session), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := launcher.run(t, nil, "--version"); code != 0 {
		t.Fatalf("launcher = exit %d stderr %q", code, stderr)
	}
	settings, err := os.ReadFile(filepath.Join(launcher.home, "settings.json"))
	if err != nil || string(settings) != readRepositoryFile(t, "worker/copilot-settings.json") {
		t.Fatalf("Copilot settings = %q, %v; want the managed settings", settings, err)
	}
	for _, relative := range []string{"mcp-config.json", "hooks", "installed-plugins"} {
		if _, err := os.Lstat(filepath.Join(launcher.home, relative)); !os.IsNotExist(err) {
			t.Errorf("launcher kept planted %s (stat error %v)", relative, err)
		}
	}
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("launcher removed native session state: %v", err)
	}
}

// TestCopilotManagedSettingsSuppressUnapprovedBehavior verifies the image's
// Copilot user settings carry every switch the compatibility record requires
// and leave every role-mandated skill model-visible.
func TestCopilotManagedSettingsSuppressUnapprovedBehavior(t *testing.T) {
	var settings map[string]any
	if err := json.Unmarshal([]byte(readRepositoryFile(t, "worker/copilot-settings.json")), &settings); err != nil {
		t.Fatalf("worker/copilot-settings.json is not JSON: %v", err)
	}
	for key, want := range map[string]string{
		"ignoredSkillsLocations": `["/work"]`,
		"disableAllHooks":        `true`,
		"remoteExport":           `false`,
		"autoUpdate":             `false`,
		"memory":                 `false`,
		"continueOnAutoMode":     `false`,
		"trustedFolders":         `[]`,
	} {
		got, err := json.Marshal(settings[key])
		if err != nil || string(got) != want {
			t.Errorf("settings %s = %s, want %s", key, got, want)
		}
	}
	for _, forbidden := range []string{"hooks", "enabledPlugins", "model", "remote"} {
		if _, ok := settings[forbidden]; ok {
			t.Errorf("settings declare %q, which the worker must never set", forbidden)
		}
	}
	disabled, _ := settings["disabledSkills"].([]any)
	set := workerSkillSet(t)
	for _, name := range disabled {
		if _, ok := set[name.(string)]; ok {
			t.Errorf("settings disable the curated worker skill %q", name)
		}
	}
}

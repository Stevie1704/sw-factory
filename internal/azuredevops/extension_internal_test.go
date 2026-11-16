package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
)

// installFakeAz puts an executable az script first on PATH. The script
// receives the real argv, environment, and standard input of the production
// runner.
func installFakeAz(t *testing.T, script string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "az"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestExtensionRunnerIsNonInteractiveAndReturnsJSON verifies that the
// production runner gives az an empty standard input and the environment
// that suppresses prompts and dynamic extension installs, and returns
// standard output.
func TestExtensionRunnerIsNonInteractiveAndReturnsJSON(t *testing.T) {
	installFakeAz(t, `if read line; then echo "stdin was not empty" >&2; exit 3; fi
[ "$AZURE_EXTENSION_USE_DYNAMIC_INSTALL" = no ] || { echo "dynamic install enabled" >&2; exit 4; }
[ "$AZURE_CORE_DISABLE_CONFIRM_PROMPT" = 1 ] || { echo "prompts enabled" >&2; exit 5; }
printf '{"argv":"%s"}' "$*"
`)
	output, err := extensionRunner{timeout: 10 * time.Second}.Run(context.Background(), []string{"repos", "show", "--output", "json"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(output) != `{"argv":"repos show --output json"}` {
		t.Fatalf("Run() output = %q", output)
	}
}

// TestExtensionRunnerTurnsAMissingOrExpiredLoginIntoARepairInstruction
// verifies that the extension's missing-credential and HTTP 401 texts become
// a LoginRequiredError that names az devops login for the targeted
// organization and carries no CLI output.
func TestExtensionRunnerTurnsAMissingOrExpiredLoginIntoARepairInstruction(t *testing.T) {
	for name, stderr := range map[string]string{
		"missing": "ERROR: Before you can run Azure DevOps commands, you need to run the login command(az login if using AAD/MSA identity else az devops login if using PAT token) to setup credentials.  Please see https://aka.ms/azure-devops-cli-auth for more information.",
		"expired": "ERROR: The requested resource requires user authentication: https://dev.azure.com/contoso/_apis/git/repositories/service",
	} {
		t.Run(name, func(t *testing.T) {
			installFakeAz(t, "echo '"+stderr+"' >&2\nexit 1\n")
			_, err := extensionRunner{timeout: 10 * time.Second}.Run(context.Background(), []string{"repos", "show", "--organization", "https://dev.azure.com/contoso"})
			var login *LoginRequiredError
			if !errors.As(err, &login) {
				t.Fatalf("Run() error = %v, want LoginRequiredError", err)
			}
			want := "Azure DevOps CLI sign-in is missing or expired for https://dev.azure.com/contoso: run az devops login --organization https://dev.azure.com/contoso"
			if err.Error() != want {
				t.Fatalf("Run() error = %q, want %q", err, want)
			}
		})
	}
}

// TestExtensionRunnerRedactsSecretsFromCLIErrors verifies that a failure
// returns one bounded line of standard error with the PAT and token-like
// values removed.
func TestExtensionRunnerRedactsSecretsFromCLIErrors(t *testing.T) {
	pat := "patvalue-0123456789"
	t.Setenv("AZURE_DEVOPS_EXT_PAT", pat)
	token := strings.Repeat("a1B2", 13)
	installFakeAz(t, "printf 'ERROR: TF401019: repository missing "+pat+" "+token+"\\nsecond line\\n' >&2\nexit 1\n")
	_, err := extensionRunner{timeout: 10 * time.Second}.Run(context.Background(), []string{"repos", "show", "--repository", "service"})
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	message := err.Error()
	if strings.Contains(message, pat) || strings.Contains(message, token) || strings.Contains(message, "second line") || strings.Contains(message, "--repository") {
		t.Fatalf("Run() error = %q, want a redacted first line without argv", message)
	}
	if !strings.Contains(message, "az repos show") || !strings.Contains(message, "TF401019") {
		t.Fatalf("Run() error = %q, want the command and the Azure DevOps error code", message)
	}
}

// TestExtensionRunnerReportsATimeout verifies that a hanging command is
// bounded and reports the hostcmd timeout.
func TestExtensionRunnerReportsATimeout(t *testing.T) {
	installFakeAz(t, "sleep 5\n")
	_, err := extensionRunner{timeout: 100 * time.Millisecond}.Run(context.Background(), []string{"boards", "query"})
	var timeout *hostcmd.TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Run() error = %v, want a TimeoutError", err)
	}
}

// recordingRunner records argv and the --in-file body while the call runs.
type recordingRunner struct {
	call     azuredevopstest.ExtensionCall
	bodyPath string
	output   string
}

// Run records one extension command and returns the scripted output.
func (r *recordingRunner) Run(_ context.Context, args []string) ([]byte, error) {
	call, err := azuredevopstest.ParseExtensionCall(args)
	if err != nil {
		return nil, err
	}
	r.call = call
	r.bodyPath = call.Flag("--in-file")
	return []byte(r.output), nil
}

// TestInvokeTargetsTheResourceAndPassesTheBodyAsAPrivateFile verifies that
// az devops invoke names area, resource, method, version, and sorted route
// and query parameters, passes the JSON body through a temporary file that
// is removed afterwards, and decodes the response.
func TestInvokeTargetsTheResourceAndPassesTheBodyAsAPrivateFile(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{output: `{"id":7}`}
	client := &Client{Runner: runner, Organization: "contoso", Route: config.AzureRouteDevOpsExtension}
	var response struct {
		ID int `json:"id"`
	}
	err := client.invoke(context.Background(), "contoso", invokeRequest{
		Area: "git", Resource: "statuses", Method: "post", APIVersion: "7.1",
		Route: map[string]string{"repositoryId": "service", "project": "Factory Pilot", "commitId": "abc"},
		Body:  map[string]string{"state": "succeeded", "description": "<ok> & done"},
	}, &response)
	if err != nil || response.ID != 7 {
		t.Fatalf("invoke() = %+v, %v", response, err)
	}
	call := runner.call
	if call.Route() != "devops invoke POST git/statuses" || call.Flag("--api-version") != "7.1" || call.Flag("--media-type") != "application/json" {
		t.Fatalf("call = %+v", call)
	}
	if got := strings.Join(call.Flags["--route-parameters"], " "); got != "commitId=abc project=Factory Pilot repositoryId=service" {
		t.Fatalf("route parameters = %q", got)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(call.Body), &body); err != nil || body["description"] != "<ok> & done" {
		t.Fatalf("body = %q, %v", call.Body, err)
	}
	if _, err := os.Stat(runner.bodyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request file %s still exists: %v", runner.bodyPath, err)
	}
}

// TestInvokeRefusesAnAPIVersionTheExtensionCannotParse verifies that a
// resource-versioned preview such as 7.1-preview.4, which az devops invoke
// cannot convert to a number, is refused before any command runs.
func TestInvokeRefusesAnAPIVersionTheExtensionCannotParse(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{}
	client := &Client{Runner: runner, Organization: "contoso", Route: config.AzureRouteDevOpsExtension}
	err := client.invoke(context.Background(), "contoso", invokeRequest{Area: "wit", Resource: "comments", APIVersion: "7.1-preview.4"}, nil)
	if err == nil || runner.call.Command != nil {
		t.Fatalf("invoke() error = %v, call = %+v; want a refusal without a command", err, runner.call)
	}
	if err := client.invoke(context.Background(), "contoso", invokeRequest{Area: "wit", Resource: "comments", APIVersion: "7.1-preview"}, nil); err != nil {
		t.Fatalf("invoke() with 7.1-preview error = %v", err)
	}
}

// TestAnExtensionClientWithoutARunnerUsesTheExtensionRunner verifies that a
// client on the extension route never falls back to the az rest runner,
// which returns standard error verbatim.
func TestAnExtensionClientWithoutARunnerUsesTheExtensionRunner(t *testing.T) {
	t.Parallel()

	if _, ok := (&Client{Route: config.AzureRouteDevOpsExtension}).runner().(extensionRunner); !ok {
		t.Fatal("runner() is not the extension runner")
	}
	if _, ok := (&Client{}).runner().(commandRunner); !ok {
		t.Fatal("runner() without a route is not the az rest runner")
	}
}

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/config"
)

// diagnosticDocument is the consumer view of one doctor diagnostic. It
// declares only the fields the contract tests read, as any consumer would.
type diagnosticDocument struct {
	Code          string `json:"code"`
	Subsystem     string `json:"subsystem"`
	Check         string `json:"check"`
	Status        string `json:"status"`
	BlocksStartup bool   `json:"blocks_startup"`
	Observed      bool   `json:"observed"`
	Problem       string `json:"problem"`
	LikelyOrigin  string `json:"likely_origin"`
	Action        *struct {
		Summary       string   `json:"summary"`
		Effect        string   `json:"effect"`
		Preconditions []string `json:"preconditions"`
		GuideTopic    string   `json:"guide_topic"`
		ID            string   `json:"id"`
		Kind          string   `json:"kind"`
		Authorization string   `json:"authorization"`
		Verify        []string `json:"verify"`
		Target        *struct {
			ConfigPath string `json:"config_path"`
		} `json:"target"`
		Postcondition *struct {
			Code string `json:"code"`
		} `json:"postcondition"`
	} `json:"action"`
}

// doctorDocument is the consumer view of `factory doctor --json`.
type doctorDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Command       string               `json:"command"`
	Outcome       string               `json:"outcome"`
	Ready         bool                 `json:"ready"`
	Complete      bool                 `json:"complete"`
	Diagnostics   []diagnosticDocument `json:"diagnostics"`
	Error         *struct {
		Code         string `json:"code"`
		LikelyOrigin string `json:"likely_origin"`
		Message      string `json:"message"`
	} `json:"error"`
}

// decodeSingleDocument fails unless stdout holds exactly one JSON object and
// nothing else.
func decodeSingleDocument(t *testing.T, stdout string, target any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout contains more than one JSON value: %q", stdout)
	}
}

// diagnostic returns the diagnostic with code, or fails.
func (d doctorDocument) diagnostic(t *testing.T, code string) diagnosticDocument {
	t.Helper()
	for _, diagnostic := range d.Diagnostics {
		if diagnostic.Code == code {
			return diagnostic
		}
	}
	t.Fatalf("diagnostics = %#v, want code %s", d.Diagnostics, code)
	return diagnosticDocument{}
}

// TestRunDoctorJSONReportsAnInvalidRepositoryPolicy verifies the JSON view
// carries the same typed outcome as the text view: a blocking repository
// condition with a stable code, a classified action, and an incomplete
// collection because dependent checks could not run.
func TestRunDoctorJSONReportsAnInvalidRepositoryPolicy(t *testing.T) {
	configPath := doctorFixture(t, func(registration config.RepositoryRegistration) config.RepositoryRegistration { return registration })

	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), []string{"doctor", "--json", "--config", configPath}, &output, &errorsOutput)
	if code != 1 || errorsOutput.Len() != 0 {
		t.Fatalf("doctor --json exit = %d stderr = %q, want 1 and no stderr", code, errorsOutput.String())
	}
	var document doctorDocument
	decodeSingleDocument(t, output.String(), &document)
	if document.SchemaVersion != 1 || document.Command != "doctor" || document.Outcome != "blocked" || document.Ready || document.Complete {
		t.Fatalf("document = %+v, want schema 1 blocked incomplete doctor", document)
	}
	invalid := document.diagnostic(t, "config.repository.invalid")
	if invalid.Subsystem != "config" || invalid.Status != "failed" || !invalid.BlocksStartup || invalid.LikelyOrigin != "repository" {
		t.Fatalf("invalid repository diagnostic = %+v, want blocking repository-origin failure", invalid)
	}
	if !strings.Contains(invalid.Problem, "model_options.test") || invalid.Action == nil || invalid.Action.Effect != "local_mutation" || invalid.Action.GuideTopic != "setup" {
		t.Fatalf("invalid repository diagnostic = %+v, want the field and a classified action", invalid)
	}
	action := invalid.Action
	if action.ID != "config.repository.invalid.resolve" || action.Kind != "human_decision" || action.Authorization != "operator" || action.Postcondition == nil || action.Postcondition.Code != "config.repository.invalid.resolved" {
		t.Fatalf("action = %+v, want a typed human-decision action with a postcondition", action)
	}
	if strings.Join(action.Verify, " ") != "factory doctor --json --config "+configPath || action.Target == nil || action.Target.ConfigPath != configPath {
		t.Fatalf("action = %+v, want verification against the selected configuration", action)
	}
	if caches := document.diagnostic(t, "config.caches.not_checked"); caches.Status != "skipped" || caches.Observed {
		t.Fatalf("caches diagnostic = %+v, want an unobserved skipped check", caches)
	}
}

// TestRunDoctorJSONReportsMalformedHostConfiguration verifies a host
// configuration that cannot load still yields the structured envelope.
func TestRunDoctorJSONReportsMalformedHostConfiguration(t *testing.T) {
	doctorFixture(t, func(registration config.RepositoryRegistration) config.RepositoryRegistration { return registration })
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("schema_version: [not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), []string{"doctor", "--json", "--config", configPath}, &output, &errorsOutput)
	if code != 1 || errorsOutput.Len() != 0 {
		t.Fatalf("doctor --json exit = %d stderr = %q, want 1 and no stderr", code, errorsOutput.String())
	}
	var document doctorDocument
	decodeSingleDocument(t, output.String(), &document)
	if host := document.diagnostic(t, "config.host.invalid"); !host.BlocksStartup || host.LikelyOrigin != "environment" {
		t.Fatalf("host diagnostic = %+v, want a blocking environment failure", host)
	}
}

// TestRunDoctorJSONNeverExposesSubprocessOutput verifies a failing probe
// that prints a secret cannot leak it into the JSON stream.
func TestRunDoctorJSONNeverExposesSubprocessOutput(t *testing.T) {
	configPath := doctorFixture(t, func(registration config.RepositoryRegistration) config.RepositoryRegistration { return registration })
	const secret = "ghp_secretTokenValue0123456789"
	stubs := os.Getenv("PATH")
	for _, name := range []string{"gh", "git", "docker"} {
		script := "#!/bin/sh\necho 'token " + secret + "'\necho 'token " + secret + "' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var output, errorsOutput bytes.Buffer
	cli.Run(context.Background(), []string{"doctor", "--json", "--config", configPath}, &output, &errorsOutput)
	if strings.Contains(output.String()+errorsOutput.String(), secret) {
		t.Fatalf("doctor --json exposed subprocess output: %s", output.String())
	}
	var document doctorDocument
	decodeSingleDocument(t, output.String(), &document)
	if docker := document.diagnostic(t, "worker.docker.unreachable"); docker.LikelyOrigin != "environment" {
		t.Fatalf("docker diagnostic = %+v, want an environment failure", docker)
	}
}

// TestRunDoctorJSONKeepsUsageErrorsOnStderr verifies the documented
// exception: an argument error is not a JSON document and exits 2.
func TestRunDoctorJSONKeepsUsageErrorsOnStderr(t *testing.T) {
	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), []string{"doctor", "--json", "extra"}, &output, &errorsOutput)
	if code != 2 || output.Len() != 0 || errorsOutput.Len() == 0 {
		t.Fatalf("exit = %d stdout = %q stderr = %q, want 2 with stderr only", code, output.String(), errorsOutput.String())
	}
}

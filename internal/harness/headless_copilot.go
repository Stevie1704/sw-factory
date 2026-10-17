package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// copilotLauncher is the in-worker launcher owned by the worker image. It
// projects the Copilot credential into its own process only and then runs the
// pinned Copilot CLI with the arguments unchanged, so the adapter never names
// the credential or the bare CLI.
const copilotLauncher = "factory-copilot"

// CopilotHeadless implements HeadlessRuntime for the GitHub Copilot CLI
// through the worker's detached process extension. It composes the shared
// headless lifecycle and adds only the Copilot command translation, identity
// evidence, and failure vocabulary.
type CopilotHeadless struct {
	headless
	// NewSessionID assigns the native session identifier for a fresh launch.
	// The Copilot CLI accepts a caller-supplied UUID, so the adapter never has
	// to discover an identity the CLI chose itself.
	NewSessionID func() (string, error)
}

// NewCopilotHeadless creates a terminal-free Copilot CLI adapter.
func NewCopilotHeadless(runtime worker.HeadlessProcessRuntime) *CopilotHeadless {
	return &CopilotHeadless{
		headless: headless{worker: runtime, protocol: headlessProtocol{
			name:            NameCopilot,
			nativeSessionID: copilotNativeSessionID,
			classify:        classifyCopilotOutput,
			// The CLI version is pinned by the worker image. This switch and
			// the --no-auto-update option keep the CLI from replacing itself
			// halfway through a run.
			environment: map[string]string{"COPILOT_AUTO_UPDATE": "false"},
		}},
		NewSessionID: newSessionID,
	}
}

// Capabilities reports Copilot CLI headless native-resume support.
func (*CopilotHeadless) Capabilities() Capabilities {
	return Capabilities{Name: NameCopilot, NativeResume: true, Headless: true}
}

// StartHeadless launches a fresh non-interactive Copilot CLI process with an
// adapter-assigned native session identifier and waits for identity evidence
// from that process.
func (c *CopilotHeadless) StartHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) != "" {
		return HeadlessSession{}, errors.New("fresh Copilot headless start cannot include a resume session")
	}
	if err := validateCopilotSelection(request); err != nil {
		return HeadlessSession{}, err
	}
	generate := c.NewSessionID
	if generate == nil {
		generate = newSessionID
	}
	assigned, err := generate()
	if err != nil {
		return HeadlessSession{}, fmt.Errorf("assign Copilot native session id: %w", err)
	}
	// A newly started process must confirm the assigned identity, so a CLI
	// that ignores --session-id fails closed. A replayed fresh start that
	// adopts the process already owning this invocation keeps the identity
	// that process was given earlier; the shared launch skips the check then.
	return c.launch(ctx, request, copilotHeadlessCommand(request, "--session-id="+assigned), worker.HeadlessLaunchFresh, "", assigned)
}

// ResumeHeadless continues exactly the recorded native session. The CLI
// fails closed on an unknown full UUID, so missing native storage cannot
// silently start a new session.
func (c *CopilotHeadless) ResumeHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) == "" {
		return HeadlessSession{}, errors.New("Copilot headless resume session id is required")
	}
	// --resume also matches ID prefixes, session names, and summaries. Only
	// the complete identity the adapter could have assigned is passed on.
	if !validSessionID(request.ResumeSessionID) {
		return HeadlessSession{}, errors.New("Copilot native session id must be a version-four UUID")
	}
	if err := validateCopilotSelection(request); err != nil {
		return HeadlessSession{}, err
	}
	return c.launch(ctx, request, copilotHeadlessCommand(request, "--resume="+request.ResumeSessionID), worker.HeadlessLaunchResume, request.ResumeSessionID, "")
}

// validateCopilotSelection refuses, before launch, a model or effort
// selection that the pinned CLI cannot honor exactly. The value sets live in
// the repository configuration, so a launch and a configuration accept the
// same selections. Whether a supported value is honored at runtime has no
// machine evidence yet (release blocker RB-3).
func validateCopilotSelection(request HeadlessStartRequest) error {
	if err := config.ValidateHarnessSelection(config.HarnessCopilot, request.Model, request.ReasoningEffort); err != nil {
		return fmt.Errorf("Copilot selection: %w", err)
	}
	return nil
}

// copilotHeadlessCommand translates one neutral request into the proved
// non-interactive Copilot CLI command. sessionOption is either the fresh
// --session-id or the exact --resume option. Every value uses the
// --option=value form, so a prompt that starts with a dash is never read as
// an option and the variadic --secret-env-vars takes exactly one name.
func copilotHeadlessCommand(request HeadlessStartRequest, sessionOption string) []string {
	// The worker is the security boundary. --allow-all-tools is required for
	// a non-interactive run; the broader --allow-all and --yolo are never
	// passed. The remaining options keep the session local, free of the
	// built-in GitHub MCP servers and repository instructions, and on the
	// pinned CLI version.
	command := []string{
		copilotLauncher,
		"--prompt=" + strings.TrimSpace(request.Prompt),
		"--output-format=json", "--allow-all-tools", "--no-ask-user",
		"--no-auto-update", "--no-remote", "--no-remote-export",
		"--disable-builtin-mcps", "--no-custom-instructions",
		"--secret-env-vars=COPILOT_GITHUB_TOKEN",
	}
	if request.Model != "" {
		command = append(command, "--model="+request.Model)
	}
	if request.ReasoningEffort != "" {
		command = append(command, "--reasoning-effort="+request.ReasoningEffort)
	}
	return append(command, sessionOption)
}

// copilotNativeSessionID returns the native identity only when the current
// process output carries exactly one well-formed identity. Absent,
// malformed, or conflicting evidence yields no identity.
func copilotNativeSessionID(output string) string {
	identity, conflict := copilotSessionEvidence(output)
	if conflict {
		return ""
	}
	return identity
}

// copilotSessionEvidenceFields are the top-level JSON object keys read as
// native session identity.
//
// PROVISIONAL pending release blocker RB-1: the JSONL event names and fields
// of a `copilot -p --output-format json` run are not observed yet. This is
// the one place that decides which output is identity evidence. It reads
// only the top level of complete JSON lines of the current process output,
// never nested values, event names, model prose, or role-home files, which
// could carry tool output or belong to an older session. Replace it with the
// observed identity event once RB-1 has live evidence.
var copilotSessionEvidenceFields = []string{"sessionId", "session_id"}

// copilotSessionEvidence collects identity evidence from every complete JSON
// line. conflict reports evidence that is not a canonical version-four UUID,
// two different identities, or a line too long to read, so the launch fails
// closed.
func copilotSessionEvidence(output string) (identity string, conflict bool) {
	scanErr := forEachHeadlessEvent(output, func(line string) bool {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			return true
		}
		for _, field := range copilotSessionEvidenceFields {
			nested, found := event[field]
			if !found {
				continue
			}
			// A non-string identity value counts as malformed evidence.
			value, _ := nested.(string)
			if !validSessionID(value) || (identity != "" && value != identity) {
				conflict = true
				return false
			}
			identity = value
		}
		return true
	})
	if scanErr != nil {
		// An unread line could carry a conflicting identity.
		return "", true
	}
	return identity, conflict
}

// copilotAuthenticationMarkers start the stderr lines that need an operator
// to repair the credential: the worker launcher found no projected
// credential, the CLI found none, or the CLI could not validate the one it
// found (observed with the pinned CLI 1.0.98).
var copilotAuthenticationMarkers = []string{
	"factory-copilot: credential unavailable",
	"Error: No authentication information found.",
	"Error: Authentication token found but could not be validated.",
}

// classifyCopilotOutput maps Copilot failure evidence to the factory's typed
// outcomes. Every observed pre-model failure exits 1 with empty stdout, so
// the exit status names no cause and only a stderr line does. A marker counts
// only at the start of a line; a complete JSONL stdout line cannot start
// with one, so model text never becomes an outcome.
//
// Session-selection and argument errors, and every failure shape not yet
// observed (expired token, rate limit, policy or model denial: release
// blocker RB-5), stay unclassified. They settle as an unexpected exit: a
// terminal pause for an operator, never a capacity retry or a fresh session.
func classifyCopilotOutput(output string) *HeadlessFailure {
	if _, conflict := copilotSessionEvidence(output); conflict {
		return &HeadlessFailure{Cause: NewUnexpectedExitError(NameCopilot)}
	}
	for _, line := range strings.Split(output, "\n") {
		for _, marker := range copilotAuthenticationMarkers {
			if strings.HasPrefix(line, marker) {
				return &HeadlessFailure{Cause: NewAuthenticationExpiredError(NameCopilot)}
			}
		}
	}
	return nil
}

var _ HeadlessRuntime = (*CopilotHeadless)(nil)
var _ NativeSessionInspector = (*CopilotHeadless)(nil)
var _ NativeSessionLivenessInspector = (*CopilotHeadless)(nil)
var _ HeadlessFailureInspector = (*CopilotHeadless)(nil)

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// ClaudeHeadless implements HeadlessRuntime through the worker's detached
// process extension. No Claude Code command, SDK loop, file operation, or
// shell command ever runs on the coordinator host.
type ClaudeHeadless struct {
	headless
	// NewSessionID assigns the native session identifier for a fresh launch.
	// Claude Code accepts a caller-supplied identifier, so the adapter never
	// has to discover one after the process starts.
	NewSessionID func() (string, error)
}

// NewClaudeHeadless creates a terminal-free Claude Code adapter.
func NewClaudeHeadless(runtime worker.HeadlessProcessRuntime) *ClaudeHeadless {
	return &ClaudeHeadless{
		headless: headless{worker: runtime, protocol: headlessProtocol{
			name:            NameClaude,
			nativeSessionID: claudeInitSessionID,
			classify:        classifyClaudeEvents,
			// The harness version is pinned by the worker image. Claude Code
			// otherwise attempts a self-update at startup, which must never
			// change the tool halfway through a run.
			environment: map[string]string{"DISABLE_AUTOUPDATER": "1"},
		}},
		NewSessionID: newSessionID,
	}
}

// Capabilities reports Claude Code's headless native-resume support.
func (*ClaudeHeadless) Capabilities() Capabilities {
	return Capabilities{Name: NameClaude, InteractiveResume: true, Headless: true}
}

// StartHeadless launches a fresh non-interactive Claude Code process with an
// adapter-assigned native session identifier and waits for the machine
// readable init event that confirms it.
func (c *ClaudeHeadless) StartHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) != "" {
		return HeadlessSession{}, errors.New("fresh Claude headless start cannot include a resume session")
	}
	generate := c.NewSessionID
	if generate == nil {
		generate = newSessionID
	}
	assigned, err := generate()
	if err != nil {
		return HeadlessSession{}, fmt.Errorf("assign Claude native session id: %w", err)
	}
	// The launch does not require the assigned identity back. A fresh start is
	// idempotent in the worker, so replaying it after response loss adopts the
	// process that already owns this invocation and reports the identity that
	// process was given. Requiring the newly generated identity there would
	// cancel a healthy run. Exact-session enforcement belongs on resume, where
	// it guards against a forked conversation.
	return c.launch(ctx, request, claudeHeadlessCommand(request, assigned, false), worker.HeadlessLaunchFresh, "")
}

// ResumeHeadless continues the exact persisted native session and never
// accepts a substituted identity, so a forked session cannot silently replace
// the invocation's recorded conversation.
func (c *ClaudeHeadless) ResumeHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) == "" {
		return HeadlessSession{}, errors.New("Claude headless resume session id is required")
	}
	// Claude Code also resumes by session name or transcript path, so an
	// identifier the adapter could not have assigned is refused before it
	// becomes a command argument rather than continuing an unrelated session.
	if !validSessionID(request.ResumeSessionID) {
		return HeadlessSession{}, errors.New("Claude native session id must be a version-four UUID")
	}
	return c.launch(ctx, request, claudeHeadlessCommand(request, request.ResumeSessionID, true), worker.HeadlessLaunchResume, request.ResumeSessionID)
}

// claudeHeadlessCommand translates one neutral request into the documented
// non-interactive Claude Code command. Print mode with stream-json output
// needs the verbose option, and the prompt stays the final positional
// argument the CLI expects.
func claudeHeadlessCommand(request HeadlessStartRequest, sessionID string, resume bool) []string {
	// The worker is the security boundary, so Claude Code runs without its
	// redundant interactive approval gates, which would stall an unattended
	// invocation. The strict MCP flags keep a factory session free of any
	// server declared by an ambient configuration file.
	command := []string{
		"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--strict-mcp-config", "--mcp-config", emptyMCPConfiguration,
	}
	if resume {
		command = append(command, "--resume", sessionID)
	} else {
		command = append(command, "--session-id", sessionID)
	}
	if request.Model != "" {
		command = append(command, "--model", request.Model)
	}
	// Claude Code warns about an unrecognized effort level and then silently
	// uses its default, so the coordinator's repository-declared options are
	// what actually constrain this value.
	if request.ReasoningEffort != "" {
		command = append(command, "--effort", request.ReasoningEffort)
	}
	return append(command, strings.TrimSpace(request.Prompt))
}

// claudeInitSessionID extracts only the documented stream-json init event and
// never treats human-readable output as a native session identity.
func claudeInitSessionID(output string) string {
	identity := ""
	forEachHeadlessEvent(output, func(line string) bool {
		var event struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Type != "system" || event.Subtype != "init" {
			return true
		}
		if !validNativeSessionID(event.SessionID) {
			return true
		}
		identity = event.SessionID
		return false
	})
	return identity
}

// classifyClaudeEvents maps documented stream-json events to the factory's
// existing typed outcomes. Claude Code names an API failure with a stable
// machine category on its retry events and on the message that reports an
// unrecoverable API error. Because a retried request can still succeed, that
// category becomes an outcome only when the run also ended in a failed result.
// The human-readable result text is never a failure category.
func classifyClaudeEvents(output string) *HeadlessFailure {
	category := ""
	resultSeen, resultFailed := false, false
	forEachHeadlessEvent(output, func(line string) bool {
		var event struct {
			Type              string `json:"type"`
			Subtype           string `json:"subtype"`
			Error             string `json:"error"`
			IsError           bool   `json:"is_error"`
			IsAPIErrorMessage bool   `json:"is_api_error_message"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			return true
		}
		if named := strings.ToLower(strings.TrimSpace(event.Error)); named != "" {
			if (event.Type == "system" && event.Subtype == "api_retry") || event.IsAPIErrorMessage {
				category = named
			}
		}
		if event.Type == "result" {
			resultSeen = true
			resultFailed = event.IsError || strings.HasPrefix(event.Subtype, "error")
		}
		return true
	})
	if resultSeen && !resultFailed {
		return nil
	}
	switch category {
	case "rate_limit", "overloaded":
		return &HeadlessFailure{Cause: NewRateLimitError(NameClaude)}
	case "authentication_failed", "oauth_org_not_allowed", "cloud_credential_error", "account_on_hold", "billing_error":
		// Every one of these needs a person to repair the account or the
		// credential material, which is exactly the factory's authentication
		// pause. Retrying them on the capacity schedule would never recover.
		return &HeadlessFailure{Cause: NewAuthenticationExpiredError(NameClaude)}
	}
	if resultFailed {
		return &HeadlessFailure{Cause: NewUnexpectedExitError(NameClaude)}
	}
	return nil
}

var _ HeadlessRuntime = (*ClaudeHeadless)(nil)
var _ NativeSessionInspector = (*ClaudeHeadless)(nil)
var _ NativeSessionLivenessInspector = (*ClaudeHeadless)(nil)
var _ HeadlessFailureInspector = (*ClaudeHeadless)(nil)

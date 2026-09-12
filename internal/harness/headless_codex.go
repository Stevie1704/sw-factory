package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// CodexHeadless implements HeadlessRuntime through the worker's detached
// process extension. No Codex command is ever started on the coordinator host.
type CodexHeadless struct {
	headless
}

// NewCodexHeadless creates a terminal-free Codex adapter.
func NewCodexHeadless(runtime worker.HeadlessProcessRuntime) *CodexHeadless {
	return &CodexHeadless{headless: headless{worker: runtime, protocol: headlessProtocol{
		name:            NameCodex,
		nativeSessionID: threadStartedID,
		classify:        classifyHeadlessEvents,
	}}}
}

// Capabilities reports Codex's headless native-resume support.
func (*CodexHeadless) Capabilities() Capabilities {
	return codexCapabilities(true)
}

// StartHeadless launches a fresh Codex exec process and waits for its machine
// readable thread-start event before returning the native identity.
func (c *CodexHeadless) StartHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) != "" {
		return HeadlessSession{}, errors.New("fresh Codex headless start cannot include a resume session")
	}
	return c.launch(ctx, request, codexHeadlessCommand(request), worker.HeadlessLaunchFresh, "")
}

// ResumeHeadless launches Codex exec resume for the exact persisted native
// thread identity and never substitutes a newly generated session.
func (c *CodexHeadless) ResumeHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) == "" {
		return HeadlessSession{}, errors.New("Codex headless resume session id is required")
	}
	return c.launch(ctx, request, codexHeadlessCommand(request), worker.HeadlessLaunchResume, request.ResumeSessionID)
}

// codexHeadlessCommand translates one neutral request into the documented
// Codex exec JSONL command, keeping the factory-owned execution options and
// the immutable prompt in their required order.
func codexHeadlessCommand(request HeadlessStartRequest) []string {
	command := codexCommandOptions([]string{"codex", "exec", "--json"}, request.Model, request.ReasoningEffort)
	if request.ResumeSessionID != "" {
		command = append(command, "resume", request.ResumeSessionID)
	}
	return append(command, strings.TrimSpace(request.Prompt))
}

// threadStartedID extracts only a valid thread.started machine event and never
// treats human-readable output as a native session identity.
func threadStartedID(output string) string {
	identity := ""
	forEachHeadlessEvent(output, func(line string) bool {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Type != "thread.started" || !validNativeSessionID(event.ThreadID) {
			return true
		}
		identity = event.ThreadID
		return false
	})
	return identity
}

// classifyHeadlessEvents maps structured Codex failure events to existing
// typed coordinator outcomes. Human-readable text is considered only when it
// is carried by a documented failure event, never when it is standalone prose.
func classifyHeadlessEvents(output string) *HeadlessFailure {
	var failure *HeadlessFailure
	forEachHeadlessEvent(output, func(line string) bool {
		var event struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Error   struct {
				Code       string `json:"code"`
				Type       string `json:"type"`
				Message    string `json:"message"`
				Status     int    `json:"status"`
				StatusCode int    `json:"status_code"`
				CodexError struct {
					Code    string `json:"code"`
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"codex_error_info"`
			} `json:"error"`
			Status     int `json:"status"`
			StatusCode int `json:"status_code"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			return true
		}
		code := strings.ToLower(strings.TrimSpace(event.Code + " " + event.Error.Code + " " + event.Error.Type + " " + event.Error.CodexError.Code + " " + event.Error.CodexError.Type))
		message := strings.ToLower(strings.TrimSpace(event.Message + " " + event.Error.Message + " " + event.Error.CodexError.Message))
		status := event.Status
		if status == 0 {
			status = event.StatusCode
		}
		if status == 0 {
			status = event.Error.Status
		}
		if status == 0 {
			status = event.Error.StatusCode
		}
		if event.Type != "error" && event.Type != "turn.failed" && strings.TrimSpace(code) == "" && status == 0 {
			return true
		}
		failureText := code + " " + message
		if status != 0 {
			failureText += fmt.Sprintf(" status %d", status)
		}
		switch {
		case containsAny(failureText, "rate_limit", "rate-limit", "rate limit", "too_many_requests", "too many requests", "capacity", "quota", "status 429", "http 429"):
			failure = &HeadlessFailure{Cause: NewRateLimitError(NameCodex)}
		case containsAny(failureText, "unauthorized", "authentication", "auth_required", "invalid_api_key", "invalid api key", "credential", "token_expired", "token expired", "status 401", "http 401"):
			failure = &HeadlessFailure{Cause: NewAuthenticationExpiredError(NameCodex)}
		case containsAny(failureText, "cancel", "aborted"):
			failure = &HeadlessFailure{Cause: context.Canceled}
		case event.Type == "error" || event.Type == "turn.failed":
			failure = &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex)}
		default:
			return true
		}
		return false
	})
	return failure
}

var _ HeadlessRuntime = (*CodexHeadless)(nil)
var _ NativeSessionInspector = (*CodexHeadless)(nil)
var _ NativeSessionLivenessInspector = (*CodexHeadless)(nil)
var _ HeadlessFailureInspector = (*CodexHeadless)(nil)

package harness

import (
	"errors"
	"fmt"
	"strings"
)

// FailureKind identifies a harness failure that has a coordinator-owned
// recovery policy.
type FailureKind string

const (
	// FailureUnexpectedExit means the native harness process exited before it
	// produced a terminal result.
	FailureUnexpectedExit FailureKind = "unexpected_exit"
	// FailureRateLimited means the harness asked the coordinator to wait for
	// capacity rather than treating the run as failed.
	FailureRateLimited FailureKind = "rate_limited"
	// FailureAuthenticationExpired means the configured harness credential is
	// no longer accepted and must be refreshed by an operator.
	FailureAuthenticationExpired FailureKind = "authentication_expired"
)

var (
	// ErrUnexpectedExit is the stable sentinel for an unexpected harness exit.
	ErrUnexpectedExit = errors.New("harness exited unexpectedly")
	// ErrRateLimited is the stable sentinel for a temporary harness capacity
	// limit.
	ErrRateLimited = errors.New("harness capacity is temporarily limited")
	// ErrAuthenticationExpired is the stable sentinel for expired harness
	// authentication.
	ErrAuthenticationExpired = errors.New("harness authentication has expired")
)

// FailureError is a typed, redacted harness error. Its message contains only
// the failure category and harness name; adapter output is never copied into
// it because adapter output can contain credentials or tokens.
type FailureError struct {
	// Kind identifies the coordinator recovery policy.
	Kind FailureKind
	// Harness identifies the adapter that reported the failure.
	Harness string
}

// Error returns a bounded message that cannot expose adapter output or
// credential values.
func (e *FailureError) Error() string {
	if e == nil {
		return "harness failure"
	}
	category := "harness failure"
	switch e.Kind {
	case FailureUnexpectedExit:
		category = ErrUnexpectedExit.Error()
	case FailureRateLimited:
		category = ErrRateLimited.Error()
	case FailureAuthenticationExpired:
		category = ErrAuthenticationExpired.Error()
	}
	if strings.TrimSpace(e.Harness) == "" {
		return category
	}
	return fmt.Sprintf("%s (%s)", category, e.Harness)
}

// Unwrap exposes the stable category sentinel while keeping the message
// independent from the underlying adapter error.
func (e *FailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Kind {
	case FailureUnexpectedExit:
		return ErrUnexpectedExit
	case FailureRateLimited:
		return ErrRateLimited
	case FailureAuthenticationExpired:
		return ErrAuthenticationExpired
	default:
		return nil
	}
}

// NewUnexpectedExitError creates a redacted unexpected-exit failure.
func NewUnexpectedExitError(harnessName string) error {
	return &FailureError{Kind: FailureUnexpectedExit, Harness: safeHarnessName(harnessName)}
}

// NewRateLimitError creates a redacted temporary-capacity failure.
func NewRateLimitError(harnessName string) error {
	return &FailureError{Kind: FailureRateLimited, Harness: safeHarnessName(harnessName)}
}

// NewAuthenticationExpiredError creates a redacted expired-authentication
// failure.
func NewAuthenticationExpiredError(harnessName string) error {
	return &FailureError{Kind: FailureAuthenticationExpired, Harness: safeHarnessName(harnessName)}
}

// ClassifyError preserves a typed failure and recognizes common adapter
// status text without retaining that text in the returned error.
func ClassifyError(err error, harnessName ...string) error {
	if err == nil {
		return nil
	}
	var typed *FailureError
	if errors.As(err, &typed) {
		name := typed.Harness
		if len(harnessName) != 0 {
			name = harnessName[0]
		}
		return &FailureError{Kind: typed.Kind, Harness: safeHarnessName(name)}
	}
	text := strings.ToLower(err.Error())
	name := ""
	if len(harnessName) != 0 {
		name = harnessName[0]
	}
	switch {
	case strings.Contains(text, "rate limit"), strings.Contains(text, "rate-limit"), strings.Contains(text, "too many requests"), strings.Contains(text, "status 429"), strings.Contains(text, "http 429"):
		return NewRateLimitError(name)
	case strings.Contains(text, "authentication expired"), strings.Contains(text, "auth expired"), strings.Contains(text, "token expired"), strings.Contains(text, "unauthorized"), strings.Contains(text, "not authenticated"), strings.Contains(text, "login required"), strings.Contains(text, "status 401"), strings.Contains(text, "http 401"):
		return NewAuthenticationExpiredError(name)
	case strings.Contains(text, "unexpected exit"), strings.Contains(text, "process exited unexpectedly"), strings.Contains(text, "harness exited"):
		return NewUnexpectedExitError(name)
	default:
		return err
	}
}

// IsUnexpectedExit reports whether err represents an unexpected harness exit.
func IsUnexpectedExit(err error) bool {
	return errors.Is(err, ErrUnexpectedExit)
}

// IsRateLimited reports whether err represents a temporary harness capacity
// limit.
func IsRateLimited(err error) bool {
	return errors.Is(err, ErrRateLimited)
}

// IsAuthenticationExpired reports whether err represents expired harness
// authentication.
func IsAuthenticationExpired(err error) bool {
	return errors.Is(err, ErrAuthenticationExpired)
}

// safeHarnessName keeps adapter labels bounded and free of control characters.
func safeHarnessName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "\x00\r\n") || len(name) > 64 {
		return ""
	}
	return name
}

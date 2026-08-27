package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestFailureErrorsAreTypedAndRedacted verifies coordinator-visible harness
// failures expose recovery categories without copying adapter output.
func TestFailureErrorsAreTypedAndRedacted(t *testing.T) {
	secret := "token=super-secret"
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "unexpected exit", err: NewUnexpectedExitError(NameCodex), want: ErrUnexpectedExit},
		{name: "rate limit", err: NewRateLimitError(NameClaude), want: ErrRateLimited},
		{name: "authentication", err: NewAuthenticationExpiredError(NameCodex), want: ErrAuthenticationExpired},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !errors.Is(test.err, test.want) {
				t.Fatalf("errors.Is(%v, %v) = false", test.err, test.want)
			}
			if strings.Contains(test.err.Error(), secret) {
				t.Fatalf("typed error leaked adapter detail: %q", test.err)
			}
			var typed *FailureError
			if !errors.As(test.err, &typed) {
				t.Fatalf("error %T is not a FailureError", test.err)
			}
		})
	}
}

// TestClassifyErrorRedactsKnownAdapterFailures verifies known transport text
// becomes a stable typed category while unrelated errors remain unchanged.
func TestClassifyErrorRedactsKnownAdapterFailures(t *testing.T) {
	classified := ClassifyError(errors.New("HTTP 401: token=super-secret"), NameCodex)
	if !IsAuthenticationExpired(classified) {
		t.Fatalf("classified error = %v, want expired authentication", classified)
	}
	if strings.Contains(classified.Error(), "super-secret") {
		t.Fatalf("classified error leaked credential: %v", classified)
	}

	original := errors.New("worker failed for an unrelated reason")
	if got := ClassifyError(original, NameClaude); !errors.Is(got, original) {
		t.Fatalf("unrelated error = %v, want original error", got)
	}
}

// TestClassifyErrorRedactsWrappedTypedFailures verifies an adapter cannot
// reintroduce credential-bearing context around an already typed failure.
func TestClassifyErrorRedactsWrappedTypedFailures(t *testing.T) {
	classified := ClassifyError(fmt.Errorf("provider token=super-secret: %w", NewAuthenticationExpiredError(NameCodex)))
	if !IsAuthenticationExpired(classified) {
		t.Fatalf("classified error = %v, want expired authentication", classified)
	}
	if strings.Contains(classified.Error(), "super-secret") || strings.Contains(classified.Error(), "token=") {
		t.Fatalf("classified error leaked wrapped adapter detail: %v", classified)
	}
}

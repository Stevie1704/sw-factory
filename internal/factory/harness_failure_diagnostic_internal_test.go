package factory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHarnessFailureDiagnosticKeepsBothEndsWithinItsBound verifies a
// transcript larger than the bound is written private, within the bound, and
// with its first and last lines, where a startup banner and a late failure
// event appear.
func TestHarnessFailureDiagnosticKeepsBothEndsWithinItsBound(t *testing.T) {
	root := t.TempDir()
	transcript := "first line\n" + strings.Repeat("x", 4*maxHarnessFailureDiagnosticBytes) + "\nlast line"
	path := writeHarnessFailureDiagnostic(root, "launch", errors.New("harness exited unexpectedly"), transcript, time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	if path != filepath.Join(root, harnessFailureDiagnosticName) {
		t.Fatalf("diagnostic path = %q, want %s in the invocation root", path, harnessFailureDiagnosticName)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() > maxHarnessFailureDiagnosticBytes+1024 {
		t.Fatalf("diagnostic = mode %v size %d, want private and at most %d bytes of output", info.Mode().Perm(), info.Size(), maxHarnessFailureDiagnosticBytes)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first line", "last line", strings.TrimSpace(harnessDiagnosticTruncationMarker)} {
		if !strings.Contains(string(body), want) {
			t.Errorf("diagnostic does not keep %q", want)
		}
	}
}

// TestHarnessFailureDiagnosticMasksGitHubTokens verifies a token a harness
// printed never reaches the local diagnostic file, whatever its GitHub token
// shape, while the surrounding output stays readable.
func TestHarnessFailureDiagnosticMasksGitHubTokens(t *testing.T) {
	tokens := []string{
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
		"gho_0123456789abcdefghijABCDEFGHIJ0123",
		"ghu_0123456789abcdefghijABCDEFGHIJ0123",
		"ghp_0123456789abcdefghijABCDEFGHIJ0123",
		"ghs_0123456789abcdefghijABCDEFGHIJ0123",
	}
	transcript := "Error: token " + strings.Join(tokens, " and token=") + " was rejected"
	path := writeHarnessFailureDiagnostic(t.TempDir(), "launch", errors.New("harness authentication expired"), transcript, time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range tokens {
		if strings.Contains(string(body), token) || strings.Contains(string(body), token[len(token)-12:]) {
			t.Errorf("diagnostic keeps token %s", token[:4])
		}
	}
	if !strings.Contains(string(body), "Error: token ") || !strings.Contains(string(body), " was rejected") {
		t.Fatalf("diagnostic = %q, want the surrounding output kept", body)
	}
}

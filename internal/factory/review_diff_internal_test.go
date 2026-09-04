package factory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestCaptureReviewDiffFromWorktreeIgnoresAnInheritedGitProjection verifies the
// capture reads the claimed worktree even when the coordinator process carries
// a repository override. Blanking the override instead of removing it made Git
// read "" as the repository and stopped every review round.
func TestCaptureReviewDiffFromWorktreeIgnoresAnInheritedGitProjection(t *testing.T) {
	worktree, base, checkpoint := reviewDiffRepository(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "projected.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())

	diff, err := captureReviewDiffFromWorktree(t.Context(), worktree, base, checkpoint)
	if err != nil {
		t.Fatalf("captureReviewDiffFromWorktree() error = %v", err)
	}
	if !strings.Contains(diff, "+checkpoint") {
		t.Fatalf("captureReviewDiffFromWorktree() diff = %q, want the checkpoint change", diff)
	}
}

// TestCaptureReviewDiffFromWorktreeNamesWhyGitRefused verifies a refused
// capture reports Git's own reason instead of a bare exit status, so the
// published lifecycle reason is actionable.
func TestCaptureReviewDiffFromWorktreeNamesWhyGitRefused(t *testing.T) {
	worktree, base, _ := reviewDiffRepository(t)
	missing := "0000000000000000000000000000000000000000"

	_, err := captureReviewDiffFromWorktree(t.Context(), worktree, base, missing)
	if err == nil {
		t.Fatal("captureReviewDiffFromWorktree() error = nil, want the unknown-checkpoint refusal")
	}
	if !strings.Contains(err.Error(), "fatal:") {
		t.Fatalf("captureReviewDiffFromWorktree() error = %q, want Git's own reason", err)
	}
}

// TestReviewDiffCommandErrorHoldsTheBoundAcrossNewlineNormalisation verifies
// the published detail never exceeds maxReviewDiffErrorBytes. Git's usage text
// is thousands of bytes over dozens of lines, and each newline becomes the
// two-byte "; " separator, so truncating before normalising doubled the bound.
func TestReviewDiffCommandErrorHoldsTheBoundAcrossNewlineNormalisation(t *testing.T) {
	t.Parallel()

	command := exec.CommandContext(t.Context(), "git", "diff", "--no-such-option")
	command.Dir = t.TempDir()
	command.Env = environmentWithoutGitProjection()
	_, raw := command.Output()
	if raw == nil {
		t.Fatal("git diff --no-such-option succeeded, want the usage refusal")
	}
	var exit *exec.ExitError
	if !errors.As(raw, &exit) {
		t.Fatalf("git diff --no-such-option error = %v, want an exit error carrying stderr", raw)
	}
	if len(exit.Stderr) <= maxReviewDiffErrorBytes {
		t.Fatalf("git stderr is %d bytes, too short to exercise the bound", len(exit.Stderr))
	}

	detail := strings.TrimPrefix(reviewDiffCommandError(raw).Error(), raw.Error()+": ")
	if len(detail) > maxReviewDiffErrorBytes {
		t.Fatalf("detail = %d bytes, want at most %d", len(detail), maxReviewDiffErrorBytes)
	}
	if strings.Contains(detail, "\n") {
		t.Fatalf("detail = %q, want newlines normalised", detail)
	}
	if !utf8.ValidString(detail) {
		t.Fatalf("detail = %q, want valid UTF-8 after truncation", detail)
	}
}

// TestReviewDiffCommandErrorDropsARuneSplitByTheBound verifies truncation ends
// on a rune boundary when the stderr text is not ASCII.
func TestReviewDiffCommandErrorDropsARuneSplitByTheBound(t *testing.T) {
	t.Parallel()

	stderr := strings.Repeat("…", maxReviewDiffErrorBytes)
	detail := strings.TrimPrefix(reviewDiffCommandError(&reviewDiffExitError{stderr: []byte(stderr)}).Error(), "exit status 1: ")
	if len(detail) > maxReviewDiffErrorBytes {
		t.Fatalf("detail = %d bytes, want at most %d", len(detail), maxReviewDiffErrorBytes)
	}
	if !utf8.ValidString(detail) {
		t.Fatalf("detail = %q, want valid UTF-8 after truncation", detail)
	}
}

// reviewDiffExitError supplies exact stderr bytes without depending on a real
// process, which cannot be made to emit a chosen encoding reliably.
type reviewDiffExitError struct {
	stderr []byte
}

// Error reports the fixed exit status the bound test strips.
func (*reviewDiffExitError) Error() string { return "exit status 1" }

// Unwrap exposes the exec error shape reviewDiffCommandError inspects.
func (e *reviewDiffExitError) Unwrap() error { return &exec.ExitError{Stderr: e.stderr} }

// reviewDiffRepository creates a two-commit repository and returns its path,
// base commit, and checkpoint commit.
func reviewDiffRepository(t *testing.T) (string, string, string) {
	t.Helper()

	worktree := t.TempDir()
	runReviewDiffGit(t, worktree, "init", "-b", "main")
	runReviewDiffGit(t, worktree, "config", "user.email", "factory@example.test")
	runReviewDiffGit(t, worktree, "config", "user.name", "Factory Test")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReviewDiffGit(t, worktree, "add", "README.md")
	runReviewDiffGit(t, worktree, "commit", "-m", "base")
	base := strings.TrimSpace(runReviewDiffGit(t, worktree, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("base\ncheckpoint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReviewDiffGit(t, worktree, "commit", "-am", "checkpoint")
	return worktree, base, strings.TrimSpace(runReviewDiffGit(t, worktree, "rev-parse", "HEAD"))
}

// runReviewDiffGit runs one fixture Git command in a repository the test owns.
func runReviewDiffGit(t *testing.T, directory string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	command.Env = environmentWithoutGitProjection()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

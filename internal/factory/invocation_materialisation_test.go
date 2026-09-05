package factory

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestStreamReviewDiffFromWorktreeSummarisesBinaryChanges verifies the review
// artifact stays readable and does not contain a Git binary patch payload.
func TestStreamReviewDiffFromWorktreeSummarisesBinaryChanges(t *testing.T) {
	worktree := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(context.Background(), "git", append([]string{"-C", worktree}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init")
	runGit("config", "user.email", "factory@example.test")
	runGit("config", "user.name", "Factory Test")
	if err := os.WriteFile(worktree+"/payload.bin", []byte{0x00, 0x01, 0x02, 0x03}, 0o600); err != nil {
		t.Fatalf("write base binary: %v", err)
	}
	runGit("add", "payload.bin")
	runGit("commit", "-m", "base binary")
	base := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(worktree+"/payload.bin", []byte{0x00, 0x01, 0xff, 0xfe}, 0o600); err != nil {
		t.Fatalf("write checkpoint binary: %v", err)
	}
	runGit("add", "payload.bin")
	runGit("commit", "-m", "checkpoint binary")
	checkpoint := runGit("rev-parse", "HEAD")

	destination, err := os.CreateTemp(worktree, ".review-diff-test-*")
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	destinationPath := destination.Name()
	defer os.Remove(destinationPath)
	if err := streamReviewDiffFromWorktree(context.Background(), worktree, base, checkpoint, destination); err != nil {
		t.Fatalf("streamReviewDiffFromWorktree() error = %v", err)
	}
	if err := destination.Close(); err != nil {
		t.Fatalf("close destination: %v", err)
	}
	content, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !strings.Contains(string(content), "Binary files") {
		t.Fatalf("binary review diff = %q, want short binary summary", content)
	}
	if strings.Contains(string(content), "GIT binary patch") {
		t.Fatalf("binary review diff contains an unusable Git binary patch: %q", content)
	}
}

// TestMaterialiseReviewDiffRemovesAnUnpublishedPartial verifies a source error
// cannot leave a partially captured artifact or temporary file behind.
func TestMaterialiseReviewDiffRemovesAnUnpublishedPartial(t *testing.T) {
	directory := t.TempDir()
	service := &Service{deps: Dependencies{Worktree: partialReviewDiffSource{}}}
	run := store.Run{
		Worktree:          directory,
		BaseCheckpointSHA: strings.Repeat("a", 40),
		CheckpointSHA:     strings.Repeat("b", 40),
	}
	if _, err := service.materialiseReviewDiff(context.Background(), run, store.Invocation{InvocationDirectory: directory}); err == nil {
		t.Fatal("materialiseReviewDiff() error = nil, want source failure")
	}
	if _, err := os.Lstat(filepath.Join(directory, reviewDiffFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published review artifact stat error = %v, want no artifact", err)
	}
	temporary, err := filepath.Glob(filepath.Join(directory, ".review-diff-*.tmp"))
	if err != nil {
		t.Fatalf("glob temporary review artifacts: %v", err)
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary review artifacts = %#v, want none", temporary)
	}
}

// partialReviewDiffSource writes evidence before reporting a capture failure.
type partialReviewDiffSource struct{}

// Create satisfies the worktree-manager half of the factory dependency seam.
func (partialReviewDiffSource) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, nil
}

// Remove satisfies the worktree-manager half of the factory dependency seam.
func (partialReviewDiffSource) Remove(context.Context, string, gitadapter.Workspace) error {
	return nil
}

// StreamDiff simulates a source that fails after producing partial stdout.
func (partialReviewDiffSource) StreamDiff(_ context.Context, _, _, _ string, destination io.Writer) error {
	_, _ = io.WriteString(destination, "partial diff")
	return errors.New("partial source failure")
}

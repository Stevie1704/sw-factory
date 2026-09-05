package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const reviewDiffFileName = "review.diff"

// reviewDiffMetadata is the bounded identity persisted in a new review packet.
// The diff bytes themselves remain in the invocation-owned artifact.
type reviewDiffMetadata struct {
	path   string
	bytes  int64
	sha256 string
}

// reviewDiffStreamer is the optional exact-checkpoint source seam. It writes
// directly to the supplied destination so the coordinator never needs to hold
// the complete review diff in memory.
type reviewDiffStreamer interface {
	StreamDiff(context.Context, string, string, string, io.Writer) error
}

// reviewDiffHashWriter publishes bytes to a file while calculating their
// content identity and size in the same streaming pass.
type reviewDiffHashWriter struct {
	destination io.Writer
	digest      hash.Hash
	bytes       int64
}

// Write forwards a chunk to the temporary artifact before accounting for the
// bytes that were actually accepted by the destination.
func (w *reviewDiffHashWriter) Write(value []byte) (int, error) {
	written, err := w.destination.Write(value)
	if written > 0 {
		_, _ = w.digest.Write(value[:written])
		w.bytes += int64(written)
	}
	return written, err
}

// materialiseReviewDiff captures and atomically publishes the exact review
// artifact before the invocation packet is written.
func (s *Service) materialiseReviewDiff(ctx context.Context, run store.Run, invocation store.Invocation) (reviewDiffMetadata, error) {
	base := reviewDiffBase(run)
	if !github.ValidCommitSHA(base) || !github.ValidCommitSHA(run.CheckpointSHA) {
		return reviewDiffMetadata{}, errors.New("review diff requires valid immutable base and checkpoint SHAs")
	}
	directory := invocation.InvocationDirectory
	if directory == "" {
		return reviewDiffMetadata{}, errors.New("review diff requires an invocation packet directory")
	}
	temporary, err := os.CreateTemp(directory, ".review-diff-*.tmp")
	if err != nil {
		return reviewDiffMetadata{}, fmt.Errorf("create review diff temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return reviewDiffMetadata{}, fmt.Errorf("secure review diff temporary file: %w", err)
	}
	streamed := &reviewDiffHashWriter{destination: temporary, digest: sha256.New()}
	captureErr := s.streamReviewDiff(ctx, run.Worktree, base, run.CheckpointSHA, streamed)
	if captureErr != nil {
		_ = temporary.Close()
		return reviewDiffMetadata{}, fmt.Errorf("capture exact review diff: %w", captureErr)
	}
	// Sync completes the file before the rename makes it visible to recovery or
	// a worker mount.
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if syncErr != nil {
		return reviewDiffMetadata{}, fmt.Errorf("sync review diff temporary file: %w", syncErr)
	}
	if closeErr != nil {
		return reviewDiffMetadata{}, fmt.Errorf("close review diff temporary file: %w", closeErr)
	}
	destination := filepath.Join(directory, reviewDiffFileName)
	if err := os.Rename(temporaryPath, destination); err != nil {
		return reviewDiffMetadata{}, fmt.Errorf("publish review diff: %w", err)
	}
	published = true
	return reviewDiffMetadata{path: prompt.WorkerReviewDiffPath, bytes: streamed.bytes, sha256: hex.EncodeToString(streamed.digest.Sum(nil))}, nil
}

// streamReviewDiff selects the configured read-only source or the host Git
// fallback without widening the worker runtime interface.
func (s *Service) streamReviewDiff(ctx context.Context, worktree, base, checkpoint string, destination io.Writer) error {
	if streamer, ok := s.deps.Worktree.(reviewDiffStreamer); ok {
		return streamer.StreamDiff(ctx, worktree, base, checkpoint, destination)
	}
	return streamReviewDiffFromWorktree(ctx, worktree, base, checkpoint, destination)
}

// streamReviewDiffFromWorktree streams Git stdout directly into the supplied
// destination. No command-output helper may materialise the complete diff.
func streamReviewDiffFromWorktree(ctx context.Context, worktree, base, checkpoint string, destination io.Writer) error {
	command := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--no-ext-diff", "--no-textconv", fmt.Sprintf("--unified=%d", reviewDiffContextLines), base, checkpoint, "--", ".")
	// The factory worker environment may provide a coordinator-level Git
	// projection. Remove those overrides so this command inspects the claimed
	// worktree's own repository metadata.
	command.Env = environmentWithoutGitProjection()
	command.Stdout = destination
	command.Stderr = io.Discard
	return command.Run()
}

// environmentWithoutGitProjection removes inherited repository overrides
// without replacing them with empty values that Git interprets as paths.
func environmentWithoutGitProjection() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if strings.HasPrefix(entry, "GIT_DIR=") || strings.HasPrefix(entry, "GIT_WORK_TREE=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// currentReviewInvocation reports whether the invocation uses the current
// review packet contract. Historical review packets remain readable without
// requiring an artifact they could not have persisted.
func currentReviewInvocation(invocation store.Invocation) bool {
	definition, ok := workflow.DefaultRegistry().Role(invocation.Role)
	return ok && definition.Kind == workflow.RoleKindReview && invocation.PromptVersion == definition.PromptVersion
}

// validatePersistedReviewDiff verifies the review artifact recorded by a new
// invocation packet without loading its contents into coordinator memory.
func validatePersistedReviewDiff(invocation store.Invocation) error {
	packetData, err := os.ReadFile(filepath.Join(invocation.InvocationDirectory, invocationPacketFileName))
	if err != nil {
		return fmt.Errorf("read persisted invocation packet: %w", err)
	}
	packet, err := decodePersistedInvocationPacket(packetData)
	if err != nil {
		return fmt.Errorf("decode persisted invocation packet: %w", err)
	}
	if packet.ReviewContext == nil {
		return errors.New("persisted review packet has no review context")
	}
	reviewContext := packet.ReviewContext
	if reviewContext.DiffPath != prompt.WorkerReviewDiffPath {
		return fmt.Errorf("review artifact path is %q, want %q", reviewContext.DiffPath, prompt.WorkerReviewDiffPath)
	}
	if reviewContext.DiffBytes < 0 {
		return fmt.Errorf("review artifact byte count %d is negative", reviewContext.DiffBytes)
	}
	if len(reviewContext.DiffSHA256) != sha256.Size*2 {
		return fmt.Errorf("review artifact SHA-256 has length %d, want %d", len(reviewContext.DiffSHA256), sha256.Size*2)
	}
	if _, err := hex.DecodeString(reviewContext.DiffSHA256); err != nil {
		return fmt.Errorf("review artifact SHA-256 is invalid: %w", err)
	}
	artifactPath := filepath.Join(invocation.InvocationDirectory, reviewDiffFileName)
	info, err := os.Lstat(artifactPath)
	if err != nil {
		return fmt.Errorf("stat review artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("review artifact is not a regular file (mode %s)", info.Mode())
	}
	if info.Size() != reviewContext.DiffBytes {
		return fmt.Errorf("review artifact size %d does not match recorded size %d", info.Size(), reviewContext.DiffBytes)
	}
	artifact, err := os.Open(artifactPath)
	if err != nil {
		return fmt.Errorf("open review artifact: %w", err)
	}
	digest := sha256.New()
	bytes, copyErr := io.Copy(digest, artifact)
	closeErr := artifact.Close()
	if copyErr != nil {
		return fmt.Errorf("hash review artifact: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close review artifact: %w", closeErr)
	}
	if bytes != reviewContext.DiffBytes {
		return fmt.Errorf("review artifact read %d bytes, want recorded size %d", bytes, reviewContext.DiffBytes)
	}
	observed := hex.EncodeToString(digest.Sum(nil))
	if observed != reviewContext.DiffSHA256 {
		return fmt.Errorf("review artifact SHA-256 %q does not match recorded identity", observed)
	}
	return nil
}

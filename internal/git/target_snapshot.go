package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// FetchTargetSnapshot resolves the exact result of this fetch through a unique
// factory-owned ref. Concurrent session/run fetches cannot replace it through
// shared FETCH_HEAD. The temporary ref is removed with an expected-value check.
// Nil runner selects the bounded ADR0015 host Git runner.
func FetchTargetSnapshot(ctx context.Context, runner CommandRunner, repositoryPath, targetBranch string) (revision string, err error) {
	if strings.TrimSpace(targetBranch) == "" || strings.ContainsRune(targetBranch, 0) {
		return "", errors.New("target branch is required for immutable observation")
	}
	if err := validateRefPart(targetBranch); err != nil {
		return "", fmt.Errorf("target branch: %w", err)
	}
	if strings.TrimSpace(repositoryPath) == "" {
		return "", errors.New("repository path is required for target observation")
	}
	if runner == nil {
		runner = NewCommandRunner()
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return "", fmt.Errorf("allocate private target ref: %w", err)
	}
	privateRef := "refs/factory/visibility/" + hex.EncodeToString(identity[:])
	defer func() {
		expected := revision
		if expected == "" {
			output, readErr := runner.Run(context.WithoutCancel(ctx), repositoryPath, []string{"rev-parse", "--verify", "--quiet", privateRef})
			if readErr != nil {
				return
			}
			expected = strings.TrimSpace(string(output))
		}
		if !validCommitSHA(expected) {
			err = errors.Join(err, errors.New("private target ref could not be safely removed"))
			return
		}
		if _, cleanupErr := runner.Run(context.WithoutCancel(ctx), repositoryPath, []string{"update-ref", "-d", privateRef, expected}); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove private target observation ref: %w", cleanupErr))
		}
	}()
	if _, err := runner.Run(ctx, repositoryPath, []string{"fetch", "--no-tags", "--no-write-fetch-head", DefaultRemoteName, "refs/heads/" + targetBranch + ":" + privateRef}); err != nil {
		return "", fmt.Errorf("fetch target branch %q: %w", targetBranch, err)
	}
	output, err := runner.Run(ctx, repositoryPath, []string{"rev-parse", "--verify", privateRef + "^{commit}"})
	if err != nil {
		return "", fmt.Errorf("resolve private target observation: %w", err)
	}
	revision = strings.TrimSpace(string(output))
	if !validCommitSHA(revision) {
		return "", errors.New("target observation did not resolve a full immutable commit identity")
	}
	return revision, nil
}

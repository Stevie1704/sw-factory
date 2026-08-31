package factory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// captureRepositoryGuidance freezes checked-in repository guidance at the
// workspace's immutable base checkpoint before any role can mutate it.
func captureRepositoryGuidance(ctx context.Context, manager gitadapter.WorktreeManager, workspace gitadapter.Workspace) (string, error) {
	if reader, ok := manager.(gitadapter.RepositoryGuidanceReader); ok {
		documents, err := reader.ReadRepositoryGuidance(ctx, workspace.Worktree, workspace.BaseSHA)
		if err != nil {
			return "", fmt.Errorf("capture repository guidance at base checkpoint: %w", err)
		}
		return formatRepositoryGuidance(documents)
	}
	documents, err := readRepositoryGuidanceFromCheckout(workspace.Worktree)
	if err != nil {
		return "", fmt.Errorf("capture repository guidance from run worktree: %w", err)
	}
	return formatRepositoryGuidance(documents)
}

// formatRepositoryGuidance creates a deterministic, named projection of the
// captured documents for the prompt fence. Empty guidance remains valid.
func formatRepositoryGuidance(documents []gitadapter.GuidanceDocument) (string, error) {
	if len(documents) == 0 {
		return "", nil
	}
	ordered := append([]gitadapter.GuidanceDocument(nil), documents...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].Path < ordered[right].Path
	})
	var builder strings.Builder
	seen := make(map[string]struct{}, len(ordered))
	for _, document := range ordered {
		path := pathpkg.Clean(strings.ReplaceAll(document.Path, "\\", "/"))
		if !repositoryGuidancePath(path) {
			return "", fmt.Errorf("unsupported repository guidance path %q", document.Path)
		}
		if _, exists := seen[path]; exists {
			return "", fmt.Errorf("repository guidance path %q appears more than once", path)
		}
		seen[path] = struct{}{}
		builder.WriteString("### ")
		builder.WriteString(path)
		builder.WriteByte('\n')
		builder.WriteString(document.Content)
		if document.Content == "" || document.Content[len(document.Content)-1] != '\n' {
			builder.WriteByte('\n')
		}
	}
	return strings.TrimRight(builder.String(), "\n"), nil
}

// readRepositoryGuidanceFromCheckout is the compatibility fallback for custom
// worktree adapters that expose only the original WorktreeManager seam. The
// coordinator calls it immediately after creating the fresh base checkout.
func readRepositoryGuidanceFromCheckout(worktreePath string) ([]gitadapter.GuidanceDocument, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, errors.New("worktree path is required")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	documents := make([]gitadapter.GuidanceDocument, 0)
	err := filepath.WalkDir(worktreePath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(worktreePath, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !repositoryGuidancePath(relative) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		documents = append(documents, gitadapter.GuidanceDocument{Path: relative, Content: string(content)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return documents, nil
}

// repositoryGuidancePath identifies the same guidance file set used by the
// Git-backed exact-checkpoint reader.
func repositoryGuidancePath(path string) bool {
	path = pathpkg.Clean(strings.ReplaceAll(path, "\\", "/"))
	if path == "." || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "/") {
		return false
	}
	base := pathpkg.Base(path)
	if base == "AGENTS.md" || base == "CLAUDE.md" || base == "CONTEXT.md" || base == "CONTEXT-MAP.md" {
		return true
	}
	return strings.HasPrefix(path, "docs/agents/")
}

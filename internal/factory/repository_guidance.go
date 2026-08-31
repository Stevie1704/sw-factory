package factory

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// captureRepositoryGuidance freezes checked-in repository guidance at the
// workspace's immutable base checkpoint before any role can mutate it.
func captureRepositoryGuidance(ctx context.Context, manager gitadapter.WorktreeManager, workspace gitadapter.Workspace) (string, error) {
	reader, ok := manager.(gitadapter.RepositoryGuidanceReader)
	if !ok {
		return "", errors.New("exact repository guidance reader is required at claim time")
	}
	documents, err := reader.ReadRepositoryGuidance(ctx, workspace.Worktree, workspace.BaseSHA)
	if err != nil {
		return "", fmt.Errorf("capture repository guidance at base checkpoint: %w", err)
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
		if !gitadapter.IsRepositoryGuidancePath(path) {
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

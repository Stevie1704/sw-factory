package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// RepositoryCraftDocument is the exact repository file captured for one role
// at the immutable claim checkpoint. The content is retained in the
// specification packet; Path and SHA256 make its source and identity explicit.
type RepositoryCraftDocument struct {
	// Path is the repository-relative source path selected by factory.yaml.
	Path string `json:"path"`
	// Content is the exact file content read from the immutable base checkpoint.
	Content string `json:"content"`
	// SHA256 is the lowercase hexadecimal digest of Content without a prefix.
	SHA256 string `json:"sha256"`
}

// CraftDigestMismatchError reports that a frozen role-craft content or recorded
// identity no longer matches its expected SHA-256 digest.
type CraftDigestMismatchError struct {
	// Role identifies the factory role whose craft identity disagrees.
	Role string
	// InvocationID identifies the invocation being rebuilt, when applicable.
	InvocationID string
	// SourcePath identifies the frozen repository source path.
	SourcePath string
	// Expected is the digest recorded by the frozen packet or invocation identity.
	Expected string
	// Observed is the digest computed from the packet or recorded projection.
	Observed string
}

// Error returns a bounded digest discrepancy without exposing repository craft.
func (e *CraftDigestMismatchError) Error() string {
	if e == nil {
		return "repository craft digest mismatch"
	}
	identity := ""
	if e.InvocationID != "" {
		identity = fmt.Sprintf(" for invocation %q", e.InvocationID)
	}
	return fmt.Sprintf("repository craft digest mismatch for role %q%s at %q: expected %q, observed %q", e.Role, identity, e.SourcePath, e.Expected, e.Observed)
}

// CraftSourcePathMismatchError reports that a frozen craft source path differs
// from the path selected by repository configuration or invocation metadata.
type CraftSourcePathMismatchError struct {
	// Role identifies the factory role whose source path disagrees.
	Role string
	// InvocationID identifies the invocation being rebuilt, when applicable.
	InvocationID string
	// Expected is the configured or packet source path.
	Expected string
	// Observed is the conflicting persisted source path.
	Observed string
}

// Error returns a bounded source-path discrepancy without exposing file content.
func (e *CraftSourcePathMismatchError) Error() string {
	if e == nil {
		return "repository craft source path mismatch"
	}
	identity := ""
	if e.InvocationID != "" {
		identity = fmt.Sprintf(" for invocation %q", e.InvocationID)
	}
	return fmt.Sprintf("repository craft source path mismatch for role %q%s: expected %q, observed %q", e.Role, identity, e.Expected, e.Observed)
}

// captureRepositoryCraft freezes every configured role-craft file from the
// workspace's immutable base checkpoint before the worktree can be edited.
func captureRepositoryCraft(ctx context.Context, manager gitadapter.WorktreeManager, workspace gitadapter.Workspace, configured map[string]string) (map[string]RepositoryCraftDocument, error) {
	if len(configured) == 0 {
		return nil, nil
	}
	reader, ok := manager.(gitadapter.RepositoryCraftReader)
	if !ok {
		return nil, errors.New("exact repository craft reader is required at claim time")
	}
	roles := sortedRepositoryCraftRoles(configured)
	frozen := make(map[string]RepositoryCraftDocument, len(roles))
	for _, role := range roles {
		path := configured[role]
		document, err := reader.ReadRepositoryCraft(ctx, workspace.Worktree, workspace.BaseSHA, path)
		if err != nil {
			return nil, fmt.Errorf("capture repository craft for role %q path %q at base checkpoint: %w", role, path, err)
		}
		if document.Path != path {
			return nil, fmt.Errorf("capture repository craft for role %q path %q returned path %q", role, path, document.Path)
		}
		frozen[role] = RepositoryCraftDocument{
			Path:    path,
			Content: document.Content,
			SHA256:  digestRepositoryCraft(document.Content),
		}
	}
	return frozen, nil
}

// sortedRepositoryCraftRoles returns map keys in a deterministic order for
// packet capture, diagnostics, and reproducible fake-adapter observations.
func sortedRepositoryCraftRoles(configured map[string]string) []string {
	roles := make([]string, 0, len(configured))
	for role := range configured {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// digestRepositoryCraft computes the content identity stored beside the prompt
// version for a frozen role-craft document.
func digestRepositoryCraft(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

// validateRepositoryCraftPacket verifies every configured document and rejects
// extra role entries before a prompt can be rebuilt from a persisted packet.
func validateRepositoryCraftPacket(packet SpecificationPacket, invocationID string) error {
	for _, role := range sortedRepositoryCraftRoles(packet.RepositoryConfig.RoleCraft) {
		if _, err := frozenRepositoryCraftForRole(packet, role, invocationID); err != nil {
			return err
		}
	}
	for role, document := range packet.RepositoryCraft {
		if _, configured := packet.RepositoryConfig.RoleCraft[role]; configured {
			continue
		}
		return &CraftSourcePathMismatchError{Role: role, InvocationID: invocationID, Expected: "", Observed: document.Path}
	}
	return nil
}

// frozenRepositoryCraftForRole returns one verified packet document, or nil
// when the frozen repository configuration selected embedded craft.
func frozenRepositoryCraftForRole(packet SpecificationPacket, role, invocationID string) (*RepositoryCraftDocument, error) {
	configuredPath, configured := packet.RepositoryConfig.RoleCraft[role]
	document, captured := packet.RepositoryCraft[role]
	if !configured {
		if captured {
			return nil, &CraftSourcePathMismatchError{Role: role, InvocationID: invocationID, Expected: "", Observed: document.Path}
		}
		return nil, nil
	}
	if !captured {
		return nil, fmt.Errorf("frozen repository craft for role %q is missing configured path %q", role, configuredPath)
	}
	if document.Path != configuredPath {
		return nil, &CraftSourcePathMismatchError{Role: role, InvocationID: invocationID, Expected: configuredPath, Observed: document.Path}
	}
	if err := verifyRepositoryCraftDigest(role, invocationID, document); err != nil {
		return nil, err
	}
	copy := document
	return &copy, nil
}

// verifyRepositoryCraftDigest checks packet content against its recorded
// digest before prompt rendering or invocation recovery proceeds.
func verifyRepositoryCraftDigest(role, invocationID string, document RepositoryCraftDocument) error {
	observed := digestRepositoryCraft(document.Content)
	if document.SHA256 != observed {
		return &CraftDigestMismatchError{
			Role:         role,
			InvocationID: invocationID,
			SourcePath:   document.Path,
			Expected:     document.SHA256,
			Observed:     observed,
		}
	}
	return nil
}

// repositoryCraftContent converts a verified packet document to the prompt
// package's presence-aware replacement input.
func repositoryCraftContent(document *RepositoryCraftDocument) *string {
	if document == nil {
		return nil
	}
	content := document.Content
	return &content
}

// validateInvocationCraftIdentity verifies the packet, invocation packet, and
// operational-store identity all describe the same frozen role-craft content.
func validateInvocationCraftIdentity(packet SpecificationPacket, persisted InvocationPacket, invocation store.Invocation) (*RepositoryCraftDocument, error) {
	if err := validateRepositoryCraftPacket(packet, invocation.ID); err != nil {
		return nil, err
	}
	document, err := frozenRepositoryCraftForRole(packet, invocation.Role, invocation.ID)
	if err != nil {
		return nil, err
	}
	expectedPath, expectedSHA := "", ""
	if document != nil {
		expectedPath = document.Path
		expectedSHA = document.SHA256
	}
	if persisted.PromptCraftSourcePath != expectedPath {
		return nil, &CraftSourcePathMismatchError{Role: invocation.Role, InvocationID: invocation.ID, Expected: expectedPath, Observed: persisted.PromptCraftSourcePath}
	}
	if persisted.PromptCraftSHA256 != expectedSHA {
		return nil, &CraftDigestMismatchError{Role: invocation.Role, InvocationID: invocation.ID, SourcePath: expectedPath, Expected: expectedSHA, Observed: persisted.PromptCraftSHA256}
	}
	if invocation.PromptCraftSourcePath != expectedPath {
		return nil, &CraftSourcePathMismatchError{Role: invocation.Role, InvocationID: invocation.ID, Expected: expectedPath, Observed: invocation.PromptCraftSourcePath}
	}
	if invocation.PromptCraftSHA256 != expectedSHA {
		return nil, &CraftDigestMismatchError{Role: invocation.Role, InvocationID: invocation.ID, SourcePath: expectedPath, Expected: expectedSHA, Observed: invocation.PromptCraftSHA256}
	}
	return document, nil
}

// craftMetadataForInvocation returns the source identity to persist beside a
// prompt version while keeping embedded-craft invocations empty and compatible.
func craftMetadataForInvocation(document *RepositoryCraftDocument) (string, string) {
	if document == nil {
		return "", ""
	}
	return document.Path, document.SHA256
}

// validatePersistedRepositoryCraft verifies the invocation packet's recorded
// craft identity before recovery can rebuild a prompt. Legacy packets without
// repository craft remain compatible and skip this check.
func validatePersistedRepositoryCraft(run store.Run, invocation store.Invocation) error {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return nil
	}
	if len(packet.RepositoryConfig.RoleCraft) == 0 && invocation.PromptCraftSourcePath == "" && invocation.PromptCraftSHA256 == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(invocation.InvocationDirectory, invocationPacketFileName))
	if err != nil {
		return fmt.Errorf("read persisted invocation packet for repository craft verification: %w", err)
	}
	persisted, err := decodePersistedInvocationPacket(data)
	if err != nil {
		return fmt.Errorf("decode persisted invocation packet for repository craft verification: %w", err)
	}
	if persisted.SchemaVersion < 9 {
		return fmt.Errorf("persisted invocation packet schema version %d has no repository craft identity", persisted.SchemaVersion)
	}
	_, err = validateInvocationCraftIdentity(packet, persisted, invocation)
	return err
}

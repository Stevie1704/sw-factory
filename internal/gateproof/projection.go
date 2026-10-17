// Package gateproof proves a candidate repository's setup and declared gates
// inside its digest-pinned worker before the host is registered. It reuses the
// production worker runtime and gate runner and publishes nothing.
package gateproof

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// ExclusionReason names why a candidate file is not in the projection.
type ExclusionReason string

const (
	// ExcludedFactoryArtifact marks a leftover Factory proof directory.
	ExcludedFactoryArtifact ExclusionReason = "factory_artifact"
	// ExcludedCredentialFile marks an untracked local credential file.
	ExcludedCredentialFile ExclusionReason = "credential_file"
	// ExcludedNotRegular marks an entry that is neither a file nor a symbolic
	// link, such as a submodule checkout.
	ExcludedNotRegular ExclusionReason = "not_regular"
	// ExcludedOutsideCheckout marks a path below a symbolic link to a
	// directory, whose content can lie outside the checkout.
	ExcludedOutsideCheckout ExclusionReason = "outside_checkout"
)

// credentialFilePatterns match the base names of untracked files that hold
// local credentials. A tracked file is part of the repository that every run
// checks out, so these patterns apply only to untracked files.
var credentialFilePatterns = []string{
	".env", ".env.*", ".netrc", ".git-credentials", ".npmrc", ".pypirc",
	"*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ecdsa*", "id_ed25519*",
	"auth.json", "credentials.json", ".credentials.json", "claude-credentials.json",
	"copilot-token",
}

// factoryArtifactPrefix starts the name of a top-level proof directory that
// the earlier manual proof recipe created inside the checkout.
const factoryArtifactPrefix = ".factory-proof"

// Candidate identifies the candidate files a proof tested.
type Candidate struct {
	// Fingerprint is the sha256 digest of the projected paths, modes, and
	// contents.
	Fingerprint string `json:"fingerprint"`
	// Files is the number of projected files and symbolic links.
	Files int `json:"files"`
	// Excluded lists the candidate files left out, with the reason.
	Excluded []Exclusion `json:"excluded"`
}

// Exclusion names one candidate file that is not in the projection.
type Exclusion struct {
	// Path is the repository-relative path.
	Path string `json:"path"`
	// Reason tells why the file is left out.
	Reason ExclusionReason `json:"reason"`
}

// Project copies the candidate files of the Git checkout at repository into
// the existing directory destination and returns their identity. The
// candidate is every tracked file in its working-tree state plus every
// untracked file that Git does not ignore, so uncommitted onboarding changes
// are tested. Git internals, ignored files, untracked credential files, and
// Factory proof artifacts stay out. The checkout is only read.
func Project(ctx context.Context, repository, destination string) (Candidate, error) {
	return project(ctx, repository, destination, nil)
}

// Fingerprint returns the identity of the candidate files of the Git
// checkout at repository without copying them, leaving out the omitted
// repository-relative paths. It selects the same files as Project.
func Fingerprint(ctx context.Context, repository string, omitted ...string) (string, error) {
	candidate, err := project(ctx, repository, "", omitted)
	return candidate.Fingerprint, err
}

// project selects, identifies, and, when destination is not empty, copies
// the candidate files.
func project(ctx context.Context, repository, destination string, omitted []string) (Candidate, error) {
	tracked, err := listFiles(ctx, repository, "--cached")
	if err != nil {
		return Candidate{}, err
	}
	untracked, err := listFiles(ctx, repository, "--others", "--exclude-standard")
	if err != nil {
		return Candidate{}, err
	}
	trackedSet := make(map[string]bool, len(tracked))
	for _, relative := range tracked {
		trackedSet[relative] = true
	}
	paths := append(tracked, untracked...)
	sort.Strings(paths)
	candidate := Candidate{Excluded: []Exclusion{}}
	hasher := sha256.New()
	previous := ""
	for _, relative := range paths {
		if relative == previous {
			continue
		}
		previous = relative
		if slices.Contains(omitted, relative) {
			continue
		}
		if reason := exclusionReason(relative, trackedSet[relative]); reason != "" {
			candidate.Excluded = append(candidate.Excluded, Exclusion{Path: relative, Reason: reason})
			continue
		}
		linked, err := symlinkedParent(repository, relative)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Candidate{}, fmt.Errorf("inspect the parents of %q: %w", relative, err)
		}
		if linked {
			candidate.Excluded = append(candidate.Excluded, Exclusion{Path: relative, Reason: ExcludedOutsideCheckout})
			continue
		}
		identity, err := projectEntry(repository, destination, relative)
		if errors.Is(err, fs.ErrNotExist) {
			// A tracked file deleted in the working tree is an intended change.
			continue
		}
		if errors.Is(err, errNotRegular) {
			candidate.Excluded = append(candidate.Excluded, Exclusion{Path: relative, Reason: ExcludedNotRegular})
			continue
		}
		if err != nil {
			return Candidate{}, fmt.Errorf("project %q: %w", relative, err)
		}
		_, _ = fmt.Fprintf(hasher, "%s\x00%s\x00", relative, identity)
		candidate.Files++
	}
	candidate.Fingerprint = "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	return candidate, nil
}

// errNotRegular refuses an entry that is neither a file nor a link.
var errNotRegular = errors.New("not a regular file or symbolic link")

// listFiles returns the repository-relative paths that git ls-files selects.
func listFiles(ctx context.Context, repository string, selection ...string) ([]string, error) {
	output, err := hostGit(ctx, repository, nil, append([]string{"ls-files", "-z"}, selection...)...)
	if err != nil {
		return nil, fmt.Errorf("list the candidate files of %q; it must be a Git checkout: %w", repository, err)
	}
	var paths []string
	for _, entry := range strings.Split(output, "\x00") {
		if entry != "" {
			paths = append(paths, entry)
		}
	}
	return paths, nil
}

// exclusionReason returns why a candidate path stays out, or "".
func exclusionReason(relative string, tracked bool) ExclusionReason {
	first, _, _ := strings.Cut(relative, "/")
	if strings.HasPrefix(first, factoryArtifactPrefix) {
		return ExcludedFactoryArtifact
	}
	if tracked {
		return ""
	}
	base := path.Base(relative)
	for _, pattern := range credentialFilePatterns {
		if matched, _ := path.Match(pattern, base); matched {
			return ExcludedCredentialFile
		}
	}
	return ""
}

// symlinkedParent reports whether a parent directory of relative, inside the
// checkout, is a symbolic link. Reading through such a link can read host
// data outside the checkout.
func symlinkedParent(repository, relative string) (bool, error) {
	for parent := path.Dir(relative); parent != "."; parent = path.Dir(parent) {
		info, err := os.Lstat(filepath.Join(repository, filepath.FromSlash(parent)))
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}

// projectEntry copies one file or symbolic link and returns the identity that
// enters the fingerprint: its kind, executable bit, and content digest. An
// empty destination only identifies the entry.
func projectEntry(repository, destination, relative string) (string, error) {
	source := filepath.Join(repository, filepath.FromSlash(relative))
	target := ""
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if destination != "" {
		target = filepath.Join(destination, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", err
		}
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(source)
		if err != nil {
			return "", err
		}
		if target != "" {
			if err := os.Symlink(link, target); err != nil {
				return "", err
			}
		}
		digest := sha256.Sum256([]byte(link))
		return "link\x00" + hex.EncodeToString(digest[:]), nil
	case info.Mode().IsRegular():
		mode := os.FileMode(0o600)
		kind := "file"
		if info.Mode().Perm()&0o111 != 0 {
			mode, kind = 0o700, "executable"
		}
		digest, err := copyFile(source, target, mode)
		if err != nil {
			return "", err
		}
		return kind + "\x00" + digest, nil
	default:
		return "", errNotRegular
	}
}

// copyFile copies source to a new file at target and returns the hex sha256
// digest of the copied content. An empty target only digests the content.
func copyFile(source, target string, mode os.FileMode) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer func() { _ = input.Close() }()
	hasher := sha256.New()
	if target == "" {
		if _, err := io.Copy(hasher, input); err != nil {
			return "", err
		}
		return hex.EncodeToString(hasher.Sum(nil)), nil
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(io.MultiWriter(output, hasher), input); err != nil {
		_ = output.Close()
		return "", err
	}
	if err := output.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

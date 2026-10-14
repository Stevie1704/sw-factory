package git

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CopyGitMetadata copies permitted regular Git metadata from source to
// destination while omitting local configuration and files that can define
// remotes, commands, or host paths. It rejects symbolic links and other
// non-regular entries.
func CopyGitMetadata(source, destination string) error {
	managedFiles := make(map[string]struct{})
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if skipGitMetadata(relative, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported git metadata entry %q", relative)
		}
		managedFiles[relative] = struct{}{}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return CopyGitMetadataFile(path, target, 0o640)
	}); err != nil {
		return err
	}
	return removeStaleGitMetadataFiles(destination, managedFiles)
}

// removeStaleGitMetadataFiles removes projected regular metadata files that
// are no longer present in the current permitted source set. Directories and
// excluded paths remain outside this reconciliation step.
func removeStaleGitMetadataFiles(destination string, managedFiles map[string]struct{}) error {
	return filepath.WalkDir(destination, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(destination, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if skipGitMetadata(relative, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if _, ok := managedFiles[relative]; ok {
			return nil
		}
		return os.Remove(path)
	})
}

// skipGitMetadata reports whether a relative Git metadata path should be
// excluded from a projection because it can contain configuration, credentials,
// executable hooks, or host-specific indirection.
func skipGitMetadata(relative string, directory bool) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		switch part {
		case "hooks", "modules", "remotes", "branches", "worktrees":
			return true
		}
	}
	if directory {
		return false
	}
	base := filepath.Base(relative)
	return base == "config" || base == "config.worktree" || base == "FETCH_HEAD" || base == "alternates"
}

// CopyGitMetadataFile streams one regular Git metadata file into a temporary
// sibling and atomically replaces the projection entry while preserving the
// specified non-secret permission bits.
func CopyGitMetadataFile(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

// MakeGitProjectionReadable gives the fixed worker identity read and execute
// access through the projection's host group without granting world access or
// write access.
func MakeGitProjectionReadable(projection string) error {
	return filepath.WalkDir(projection, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported git metadata entry %q", path)
		}
		mode := gitProjectionPermissions(info.Mode())
		return os.Chmod(path, mode)
	})
}

// gitProjectionPermissions returns owner-plus-group permissions for one
// projected Git metadata entry. The worker receives the projection's host
// group at Docker start, so no world-readable or world-executable bits are
// needed.
func gitProjectionPermissions(mode fs.FileMode) fs.FileMode {
	permissions := mode.Perm() & 0o700
	if mode.IsDir() {
		return permissions | 0o050
	}
	permissions |= 0o040
	if mode.Perm()&0o111 != 0 {
		permissions |= 0o010
	}
	return permissions
}

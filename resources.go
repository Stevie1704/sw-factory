// Package swfactory carries the Software Factory resources that a release
// ships inside the factory binary: the onboarding procedure and its reference
// documents, the operator onboarding skill, the curated worker skills with
// their provenance, the worked worker definition, and the smoke and
// headless-verification scripts. The
// files stay at their checked-in paths, so the embedded copy is the source and
// the documentation links between them still resolve after an export.
package swfactory

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// The repository factory.yaml is deliberately absent: the smoke script falls
// back to it, and an exported copy would point a target's smoke at Factory's
// own worker image.
//
//go:embed docs/*.md worker/SKILLS.md worker/Dockerfile worker/skills operator/skills
//go:embed scripts/smoke-skills.sh scripts/verify-headless-worker.sh
var resources embed.FS

// ExportResources writes every shipped resource below destination, keeping the
// checked-in layout. Scripts are executable. The modes do not depend on the
// process umask, so the export always matches its resource identity. The export refuses to replace an
// existing file, so resources of one release never mix with another copy.
func ExportResources(destination string) error {
	return fs.WalkDir(resources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if entry.IsDir() {
			return makeDirectory(target)
		}
		data, err := resources.ReadFile(name)
		if err != nil {
			return err
		}
		return writeNewFile(target, data, resourceMode(name))
	})
}

// resourceMode returns the file mode of one exported resource: scripts run,
// every other file is read.
func resourceMode(name string) os.FileMode {
	if path.Dir(name) == "scripts" {
		return 0o755
	}
	return 0o644
}

// makeDirectory creates one exported directory with mode 0755 regardless of
// the process umask.
func makeDirectory(target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return os.Chmod(target, 0o755)
}

// writeNewFile creates one file that must not exist yet. The process umask
// narrows the mode OpenFile applies, so the mode is set again explicitly.
func writeNewFile(target string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("export Factory resource: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("export Factory resource %s: %w", target, err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("export Factory resource %s: %w", target, err)
	}
	return file.Close()
}

// ownershipMarker is the file that marks a directory as a complete export of
// one resource identity. PrepareResources writes it last, so a directory
// without it is foreign or an interrupted export.
const ownershipMarker = ".factory-resources"

// ResourceIdentity returns the SHA-256 of the path, mode, and content of every
// shipped resource as sha256:HEX. Two releases with the same identity ship the
// same resources.
func ResourceIdentity() (string, error) {
	digest := sha256.New()
	err := walkResourceFiles(func(name string, data []byte) error {
		_, err := fmt.Fprintf(digest, "%s\x00%o\x00%d\x00", name, resourceMode(name), len(data))
		if err == nil {
			_, err = digest.Write(data)
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("compute the Factory resource identity: %w", err)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

// PrepareResources makes destination a complete export of the shipped
// resources and returns their identity. It exports into a missing or empty
// directory. It accepts a directory that an earlier preparation of the same
// identity completed, when every resource there is unchanged. It refuses every
// other directory, so it never mixes releases or overwrites unrelated files.
func PrepareResources(destination string) (string, error) {
	identity, err := ResourceIdentity()
	if err != nil {
		return "", err
	}
	marker, err := os.ReadFile(filepath.Join(destination, ownershipMarker))
	switch {
	case err == nil:
		if err := verifyPreparedResources(destination, strings.TrimSpace(string(marker)), identity); err != nil {
			return "", err
		}
		return identity, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("read the Factory resource directory %s: %w", destination, err)
	}
	if err := requireEmptyDirectory(destination); err != nil {
		return "", err
	}
	if err := exportInto(destination, identity); err != nil {
		return "", err
	}
	return identity, nil
}

// verifyPreparedResources accepts an earlier export of the same identity whose
// files still have the content and mode of the shipped resources.
func verifyPreparedResources(destination, prepared, identity string) error {
	if prepared != identity {
		return fmt.Errorf("%s holds Factory resources with identity %s, not %s of this release: choose a new directory, or remove that one", destination, prepared, identity)
	}
	return walkResourceFiles(func(name string, data []byte) error {
		target := filepath.Join(destination, filepath.FromSlash(name))
		exported, err := os.ReadFile(target)
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(target)
		}
		if err != nil || !bytes.Equal(exported, data) || info.Mode().Perm() != resourceMode(name) {
			return fmt.Errorf("%s differs from the Factory release resources at %s: choose a new directory, or remove that one", destination, name)
		}
		return nil
	})
}

// requireEmptyDirectory creates the parent of destination and refuses a
// destination that holds any entry.
func requireEmptyDirectory(destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create the Factory resource directory: %w", err)
	}
	entries, err := os.ReadDir(destination)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the Factory resource directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("%s is not empty and holds no complete Factory resource export: choose a new directory, or remove that one", destination)
	}
	return nil
}

// exportInto exports the resources and their identity marker into a staging
// directory beside the empty destination, then renames it into place. An
// interrupted export leaves the destination empty, so a later preparation
// accepts it.
func exportInto(destination, identity string) error {
	staging, err := os.MkdirTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".partial-")
	if err != nil {
		return fmt.Errorf("create the Factory resource directory: %w", err)
	}
	err = makeDirectory(staging)
	if err == nil {
		err = ExportResources(staging)
	}
	if err == nil {
		err = writeNewFile(filepath.Join(staging, ownershipMarker), []byte(identity+"\n"), 0o644)
	}
	if err == nil {
		err = publishStaged(staging, destination)
	}
	if err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("export the Factory resources to %s: %w", destination, err)
	}
	return nil
}

// publishStaged renames the complete staging directory to destination.
// Rename cannot replace a directory on every platform, so an empty destination
// directory is removed first. Rmdir, unlike os.Remove, never deletes a file,
// and rename of a directory onto a file or a non-empty directory fails. So an
// entry that another process creates at destination meanwhile is never lost.
func publishStaged(staging, destination string) error {
	if err := syscall.Rmdir(destination); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(staging, destination)
}

// walkResourceFiles calls visit with the slash-separated name and content of
// each shipped resource file, in lexical order.
func walkResourceFiles(visit func(name string, data []byte) error) error {
	return fs.WalkDir(resources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := resources.ReadFile(name)
		if err != nil {
			return err
		}
		return visit(name, data)
	})
}

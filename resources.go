// Package swfactory carries the Software Factory resources that a release
// ships inside the factory binary: the onboarding procedure and its reference
// documents, the curated worker skills with their provenance, the worked
// worker definition, and the smoke and headless-verification scripts. The
// files stay at their checked-in paths, so the embedded copy is the source and
// the documentation links between them still resolve after an export.
package swfactory

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// The repository factory.yaml is deliberately absent: the smoke script falls
// back to it, and an exported copy would point a target's smoke at Factory's
// own worker image.
//
//go:embed docs/*.md worker/SKILLS.md worker/Dockerfile worker/skills
//go:embed scripts/smoke-skills.sh scripts/verify-headless-worker.sh
var resources embed.FS

// ExportResources writes every shipped resource below destination, keeping the
// checked-in layout. Scripts are executable. The export refuses to replace an
// existing file, so resources of one release never mix with another copy.
func ExportResources(destination string) error {
	return fs.WalkDir(resources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
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

// writeNewFile creates one file that must not exist yet.
func writeNewFile(target string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("export Factory resource: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("export Factory resource %s: %w", target, err)
	}
	return file.Close()
}

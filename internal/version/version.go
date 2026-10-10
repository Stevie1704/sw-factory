// Package version reports the identity of the running Factory build: its
// release version when it has one, and the source revision when the Go
// toolchain recorded it.
package version

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// release is the release version of a release build. The release build sets
// it at link time with -X; every other build leaves it empty.
var release string

// SchemaVersion is the version of the JSON identity document. Increment it
// when a field changes meaning or is removed.
const SchemaVersion = 1

// Build kinds distinguish a deliberate release from every other build.
const (
	BuildRelease     = "release"
	BuildDevelopment = "development"
)

// Unknown marks a value the build did not record.
const Unknown = "unknown"

// Identity is the documented identity of one Factory build.
type Identity struct {
	// SchemaVersion is the version of this document's shape.
	SchemaVersion int `json:"schema_version"`
	// Version is the release version, or "development" for any other build.
	Version string `json:"version"`
	// Build is "release" or "development".
	Build string `json:"build"`
	// Revision is the source commit, or "unknown" when the build did not record it.
	Revision string `json:"revision"`
	// Modified reports that the build's source tree had uncommitted changes.
	Modified bool `json:"modified"`
}

// pseudoVersion matches the timestamp-and-commit suffix Go adds to a module
// version derived from an untagged commit.
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}`)

// Current returns the identity of the running binary.
func Current() Identity {
	identity := Identity{SchemaVersion: SchemaVersion, Version: BuildDevelopment, Build: BuildDevelopment, Revision: Unknown}
	info, ok := debug.ReadBuildInfo()
	if ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				identity.Revision = setting.Value
			case "vcs.modified":
				identity.Modified = setting.Value == "true"
			}
		}
	}
	switch {
	case release != "":
		identity.Version, identity.Build = release, BuildRelease
	case ok && isTaggedModuleVersion(info.Main.Version):
		// go install module@vX.Y.Z records the tag but no VCS settings.
		identity.Version, identity.Build = info.Main.Version, BuildRelease
	}
	return identity
}

// isTaggedModuleVersion reports whether Go recorded a tagged module version
// rather than a local, pseudo, or modified build.
func isTaggedModuleVersion(moduleVersion string) bool {
	if moduleVersion == "" || moduleVersion == "(devel)" {
		return false
	}
	return !pseudoVersion.MatchString(moduleVersion) && !strings.HasSuffix(moduleVersion, "+dirty")
}

// Text renders the identity as one human-readable line without a newline.
func (i Identity) Text() string {
	revision := "revision " + i.Revision
	if i.Modified {
		revision += ", modified"
	}
	if i.Build == BuildRelease {
		return "factory " + i.Version + " (release, " + revision + ")"
	}
	return "factory development build (" + revision + ")"
}

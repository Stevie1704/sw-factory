// Package guide holds the operator guide that ships inside the factory
// binary. The Markdown files under topics are the canonical source: the CLI
// prints them and the repository documentation links to them, so no second
// copy can drift. The guide explains supported behavior; it is not role
// authority and never replaces the embedded role prompts.
package guide

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed topics/*.md
var topicFS embed.FS

// Overview is the topic that `factory guide` prints without a topic name.
const Overview = "overview"

// OverviewWordLimit is the ceiling on the overview length. Detailed
// procedures belong in topics.
const OverviewWordLimit = 1000

// ErrUnknownTopic reports a topic name the guide does not ship.
var ErrUnknownTopic = errors.New("unknown guide topic")

// Topics returns the sorted names of every detailed topic, without the
// overview.
func Topics() []string {
	entries, err := fs.ReadDir(topicFS, "topics")
	if err != nil {
		// The directory is embedded at build time, so it always exists.
		panic(fmt.Sprintf("read embedded guide topics: %v", err))
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".md")
		if name != Overview {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Topic returns the Markdown body of one topic, or ErrUnknownTopic.
func Topic(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return "", fmt.Errorf("%w %q", ErrUnknownTopic, name)
	}
	data, err := topicFS.ReadFile(path.Join("topics", name+".md"))
	if err != nil {
		return "", fmt.Errorf("%w %q", ErrUnknownTopic, name)
	}
	return string(data), nil
}

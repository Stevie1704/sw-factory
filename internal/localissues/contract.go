// Package localissues owns accepted Markdown content, stable identities, and
// graph validation. It contains no execution approval or mutable run state.
package localissues

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// SchemaVersion is the only supported local Markdown schema.
	SchemaVersion = 1
	// Directory is the repository-relative accepted issue directory.
	Directory = "docs/issues"
	// MaxFileBytes bounds one complete UTF-8 issue, including front matter.
	MaxFileBytes = 1 << 20
	// MaxIssues bounds a complete validated issue graph.
	MaxIssues = 256
	// MaxGraphBytes bounds the bytes of a complete graph.
	MaxGraphBytes = 16 << 20
)

// Kind retains the difference between grouping, implementation, and research.
type Kind string

const (
	KindParent        Kind = "parent"
	KindTask          Kind = "task"
	KindInvestigation Kind = "investigation"
)

var identityPattern = regexp.MustCompile(`^local-[0-9a-f]{32}$`)
var filenamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.md$`)

// Issue is accepted issue content and its revision-addressed source evidence.
// Only the explicitly tagged fields belong in YAML; Path, Bytes, and digests
// describe the source and never grant execution authority.
type Issue struct {
	SchemaVersion int      `yaml:"schema_version"`
	ID            string   `yaml:"id"`
	Title         string   `yaml:"title"`
	Kind          Kind     `yaml:"kind"`
	Parent        string   `yaml:"parent,omitempty"`
	Dependencies  []string `yaml:"dependencies"`
	Correlation   string   `yaml:"publication_correlation"`
	Body          string   `yaml:"-"`
	Path          string   `yaml:"-"`
	Bytes         []byte   `yaml:"-"`
	ContentDigest string   `yaml:"-"`
}

// File is one source entry; Mode is required so symlinks and Git submodules
// cannot be mistaken for accepted Markdown.
type File struct {
	Path string
	Data []byte
	Mode fs.FileMode
}

// Graph is a complete validated graph in stable issue-identity order.
// Digest binds exact content and relationships, independent of mutable paths.
type Graph struct {
	Issues []Issue
	Digest string
}

// ValidID reports whether id is an opaque 128-bit local identity.
func ValidID(id string) bool { return identityPattern.MatchString(id) }

// DefaultPath returns the initial filename of an already allocated identity.
// It does not allocate identities or write files.
func DefaultPath(id string) (string, error) {
	if !ValidID(id) {
		return "", errors.New("invalid local issue identity")
	}
	return Directory + "/" + id + ".md", nil
}

// ValidatePath permits only a plain direct Markdown child of docs/issues.
// A rename never changes the identity stored in the front matter.
func ValidatePath(name string) error {
	if path.Dir(name) != Directory || path.Clean(name) != name || strings.Contains(name, "\\") || len(path.Base(name)) > 128 || !filenamePattern.MatchString(path.Base(name)) {
		return errors.New("local issue path must be a plain Markdown file directly under docs/issues")
	}
	return nil
}

// Digest computes the exact byte identity shared by preview and target checks.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Parse decodes one bounded regular-file payload without changing any state.
// Unknown fields, YAML aliases, duplicate keys, malformed bodies, and unsupported
// schemas are rejected; content identity always hashes the original bytes.
func Parse(name string, data []byte) (Issue, error) {
	if err := ValidatePath(name); err != nil {
		return Issue{}, err
	}
	if len(data) == 0 || len(data) > MaxFileBytes || !utf8.Valid(data) || bytes.ContainsAny(data, "\x00\r") {
		return Issue{}, errors.New("local issue must be bounded UTF-8 with LF line endings")
	}
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return Issue{}, errors.New("local issue YAML front matter is required")
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end < 0 {
		return Issue{}, errors.New("local issue front matter is not terminated")
	}
	header := data[4 : 4+end]
	var node yaml.Node
	if err := yaml.Unmarshal(header, &node); err != nil {
		return Issue{}, errors.New("invalid local issue YAML")
	}
	if err := validateFrontMatter(&node); err != nil {
		return Issue{}, err
	}
	var issue Issue
	decoder := yaml.NewDecoder(bytes.NewReader(header))
	decoder.KnownFields(true)
	if err := decoder.Decode(&issue); err != nil {
		return Issue{}, errors.New("unknown or malformed local issue front matter")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Issue{}, errors.New("local issue front matter must contain one document")
	}
	issue.Body = strings.TrimPrefix(string(data[4+end+5:]), "\n")
	if err := validateIssue(issue); err != nil {
		return Issue{}, err
	}
	issue.Path, issue.Bytes, issue.ContentDigest = name, bytes.Clone(data), Digest(data)
	return issue, nil
}

// Render creates the canonical approved bytes, with sorted dependency IDs and
// fixed front-matter field order. It never alters an existing accepted source.
func Render(issue Issue) ([]byte, error) {
	if err := validateIssue(issue); err != nil {
		return nil, err
	}
	issue.Dependencies = slices.Clone(issue.Dependencies)
	slices.Sort(issue.Dependencies)
	if issue.Dependencies == nil {
		issue.Dependencies = []string{}
	}
	var buffer bytes.Buffer
	buffer.WriteString("---\n")
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(issue); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	buffer.WriteString("---\n\n")
	buffer.WriteString(issue.Body)
	data := buffer.Bytes()
	if len(data) > MaxFileBytes {
		return nil, errors.New("local issue exceeds file byte limit")
	}
	return data, nil
}

// Validate checks every entry and the complete parent/dependency graph before
// returning any issue. A partial or invalid graph is never usable for claim.
func Validate(files []File) (Graph, error) {
	if len(files) > MaxIssues {
		return Graph{}, errors.New("local issue graph exceeds issue limit")
	}
	issues := make([]Issue, 0, len(files))
	ids, correlations, names := map[string]bool{}, map[string]bool{}, map[string]bool{}
	total := 0
	for _, file := range files {
		if !file.Mode.IsRegular() {
			return Graph{}, errors.New("local issue source must be a regular file")
		}
		if names[strings.ToLower(file.Path)] {
			return Graph{}, errors.New("duplicate local issue path")
		}
		names[strings.ToLower(file.Path)] = true
		total += len(file.Data)
		if total > MaxGraphBytes {
			return Graph{}, errors.New("local issue graph exceeds byte limit")
		}
		issue, err := Parse(file.Path, file.Data)
		if err != nil {
			return Graph{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		if ids[issue.ID] {
			return Graph{}, errors.New("duplicate local issue identity")
		}
		ids[issue.ID] = true
		if correlations[issue.Correlation] {
			return Graph{}, errors.New("duplicate local issue publication correlation")
		}
		correlations[issue.Correlation] = true
		issues = append(issues, issue)
	}
	slices.SortFunc(issues, func(a, b Issue) int { return strings.Compare(a.ID, b.ID) })
	byID := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = issue
	}
	edges := make(map[string][]string, len(issues))
	for _, issue := range issues {
		if issue.Parent != "" {
			parent, ok := byID[issue.Parent]
			if !ok || parent.Kind != KindParent {
				return Graph{}, errors.New("local issue parent must reference an existing parent issue")
			}
			edges[issue.ID] = append(edges[issue.ID], issue.Parent)
		}
		for _, dependency := range issue.Dependencies {
			if !ids[dependency] {
				return Graph{}, errors.New("dangling local issue dependency")
			}
			edges[issue.ID] = append(edges[issue.ID], dependency)
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return errors.New("local issue relationships contain a cycle")
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, next := range edges[id] {
			if err := visit(next); err != nil {
				return err
			}
		}
		visiting[id], visited[id] = false, true
		return nil
	}
	var identity bytes.Buffer
	for _, issue := range issues {
		if err := visit(issue.ID); err != nil {
			return Graph{}, err
		}
		fmt.Fprintf(&identity, "%s\x00%s\n", issue.ID, issue.ContentDigest)
	}
	return Graph{Issues: issues, Digest: Digest(identity.Bytes())}, nil
}

// ReadDirectory is a read-only filesystem preflight for previews/imports. Runtime
// claims instead supply committed Git entries to Validate. Symlinked ancestors,
// nested directories, and non-regular entries are explicit errors.
func ReadDirectory(root string) (Graph, error) {
	directory := root
	for _, component := range []string{"docs", "issues"} {
		directory = filepath.Join(directory, component)
		info, err := os.Lstat(directory)
		if errors.Is(err, fs.ErrNotExist) {
			return Validate(nil)
		}
		if err != nil {
			return Graph{}, err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return Graph{}, errors.New("local issue directory must not be a symlink or non-directory")
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return Graph{}, err
	}
	if len(entries) > MaxIssues {
		return Graph{}, errors.New("local issue directory exceeds issue limit")
	}
	files := make([]File, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return Graph{}, err
		}
		if !info.Mode().IsRegular() {
			return Graph{}, errors.New("local issue source must be a regular file")
		}
		if info.Size() > MaxFileBytes {
			return Graph{}, errors.New("local issue exceeds file byte limit")
		}
		name := Directory + "/" + entry.Name()
		if err := ValidatePath(name); err != nil {
			return Graph{}, err
		}
		opened, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			return Graph{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(opened, MaxFileBytes+1))
		closeErr := opened.Close()
		if readErr != nil {
			return Graph{}, readErr
		}
		if closeErr != nil {
			return Graph{}, closeErr
		}
		files = append(files, File{Path: name, Data: data, Mode: info.Mode()})
	}
	return Validate(files)
}

// validateIssue checks the source-only schema; no field can claim an operator
// identity, append a command, or grant execution authority.
func validateIssue(issue Issue) error {
	if issue.SchemaVersion != SchemaVersion {
		return errors.New("unsupported local issue schema_version")
	}
	if !ValidID(issue.ID) {
		return errors.New("invalid local issue identity")
	}
	if strings.TrimSpace(issue.Title) != issue.Title || !utf8.ValidString(issue.Title) || len(issue.Title) == 0 || len(issue.Title) > 256 || strings.IndexFunc(issue.Title, unicode.IsControl) >= 0 {
		return errors.New("invalid local issue title")
	}
	if issue.Kind != KindParent && issue.Kind != KindTask && issue.Kind != KindInvestigation {
		return errors.New("unsupported local issue kind")
	}
	if issue.Parent != "" && (!ValidID(issue.Parent) || issue.Parent == issue.ID) {
		return errors.New("invalid local issue parent")
	}
	seen := map[string]bool{}
	for _, id := range issue.Dependencies {
		if !ValidID(id) || id == issue.ID || seen[id] {
			return errors.New("invalid or duplicate local issue dependency")
		}
		seen[id] = true
	}
	if strings.TrimSpace(issue.Correlation) != issue.Correlation || !utf8.ValidString(issue.Correlation) || len(issue.Correlation) == 0 || len(issue.Correlation) > 512 || strings.IndexFunc(issue.Correlation, unicode.IsControl) >= 0 {
		return errors.New("invalid local issue publication correlation")
	}
	if !utf8.ValidString(issue.Body) || strings.ContainsAny(issue.Body, "\x00\r") || !strings.HasSuffix(issue.Body, "\n") {
		return errors.New("local issue body must be UTF-8 with LF lines and a final newline")
	}
	rest := issue.Body
	for _, heading := range []string{"## Problem\n", "## Scope and non-goals\n", "## Acceptance criteria\n"} {
		index := strings.Index(rest, heading)
		if index < 0 || index > 0 && rest[index-1] != '\n' {
			return errors.New("local issue body requires problem, scope/non-goals, and acceptance criteria sections")
		}
		if strings.TrimSpace(rest[:index]) == "" && heading != "## Problem\n" {
			return errors.New("local issue body sections must be nonempty")
		}
		rest = rest[index+len(heading):]
	}
	if strings.TrimSpace(rest) == "" {
		return errors.New("local issue acceptance criteria must be nonempty")
	}
	return nil
}

// validateYAMLNode rejects aliases and merge keys before typed strict decoding.
func validateYAMLNode(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" || node.Tag == "!!merge" {
		return errors.New("local issue YAML aliases, anchors, and merge keys are unsupported")
	}
	for _, child := range node.Content {
		if err := validateYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

// validateFrontMatter requires every schema field and its exact YAML type.
func validateFrontMatter(document *yaml.Node) error {
	if err := validateYAMLNode(document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("local issue front matter must be a mapping")
	}
	node := document.Content[0]
	required := map[string]bool{"schema_version": false, "id": false, "title": false, "kind": false, "dependencies": false, "publication_correlation": false}
	allowed := map[string]string{"schema_version": "!!int", "id": "!!str", "title": "!!str", "kind": "!!str", "parent": "!!str", "dependencies": "!!seq", "publication_correlation": "!!str"}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		tag, ok := allowed[key.Value]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || !ok || seen[key.Value] || value.Tag != tag {
			return errors.New("unknown, duplicate, or malformed local issue front matter field")
		}
		seen[key.Value] = true
		if _, ok := required[key.Value]; ok {
			required[key.Value] = true
		}
		if key.Value == "dependencies" {
			for _, item := range value.Content {
				if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
					return errors.New("local issue dependencies must be issue identities")
				}
			}
		}
	}
	for _, present := range required {
		if !present {
			return errors.New("local issue front matter is missing a required field")
		}
	}
	return nil
}

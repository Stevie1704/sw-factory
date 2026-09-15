// Package reviewunits partitions one exact-checkpoint review diff into stable
// workload units without changing the checkpoint or review-round boundary.
package reviewunits

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// SchemaVersion identifies the durable manifest shape.
	SchemaVersion = 1
	// PolicyVersion identifies the deterministic partition algorithm.
	PolicyVersion = "review-units-v1"
	// DefaultMaxUnitBytes is the default self-contained diff workload bound.
	DefaultMaxUnitBytes = 64 << 10
	// DefaultMaxUnits is the normal repository-declared review fan-out.
	DefaultMaxUnits = 4
	// DefaultContextLines is the maximum source-line context overlap.
	DefaultContextLines = 10
)

// Policy freezes the inputs that affect manifest construction. The effective
// values are copied into Manifest so a restart cannot silently repartition.
type Policy struct {
	SchemaVersion int    `json:"schema_version"`
	PolicyVersion string `json:"policy_version"`
	MaxUnitBytes  int    `json:"max_unit_bytes"`
	MaxUnits      int    `json:"max_units"`
	ContextLines  int    `json:"context_lines"`
}

// Segment identifies one byte interval copied from the canonical diff into a
// unit artifact. Segments are ordered and non-overlapping within one unit;
// adjacent units may repeat bounded context through their own segments.
type Segment struct {
	StartByte int `json:"start_byte"`
	EndByte   int `json:"end_byte"`
}

// Range identifies one source-side changed or context line. Primary ranges
// occur in exactly one unit; context ranges may occur in more than one unit.
type Range struct {
	Path      string `json:"path"`
	Hunk      int    `json:"hunk"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Unit is one ordered manifest assignment. Its byte segments derive the
// self-contained `/invocation/review-unit.diff` artifact from the exact full
// review diff; no unit is an independent checkpoint or review result.
type Unit struct {
	ID            string    `json:"unit_id"`
	Ordinal       int       `json:"ordinal"`
	WorkloadBytes int       `json:"workload_bytes"`
	DiffSHA256    string    `json:"diff_sha256"`
	Segments      []Segment `json:"segments"`
	PrimaryRanges []Range   `json:"primary_ranges"`
	ContextRanges []Range   `json:"context_ranges,omitempty"`
	PrimaryFiles  []string  `json:"primary_files,omitempty"`
	ChangedLines  int       `json:"changed_lines"`
}

// Manifest is the immutable ordered assignment for one review round.
type Manifest struct {
	SchemaVersion  int    `json:"schema_version"`
	PolicyVersion  string `json:"policy_version"`
	RunID          string `json:"run_id"`
	BaseSHA        string `json:"base_sha"`
	CheckpointSHA  string `json:"checkpoint_sha"`
	DiffBytes      int    `json:"diff_bytes"`
	DiffSHA256     string `json:"diff_sha256"`
	MaxUnitBytes   int    `json:"max_unit_bytes"`
	MaxUnits       int    `json:"max_units"`
	ContextLines   int    `json:"context_lines"`
	ChangedLines   int    `json:"changed_lines"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Units          []Unit `json:"units"`
}

// diffLine is one preserved line in a unified-diff hunk.
type diffLine struct {
	start   int
	end     int
	text    string
	oldLine int
	newLine int
}

// diffHunk is a parsed hunk with source-side line identities.
type diffHunk struct {
	headerStart int
	headerEnd   int
	body        []diffLine
}

// diffFile is one complete `diff --git` section.
type diffFile struct {
	start int
	end   int
	path  string
	hunks []diffHunk
}

type fragment struct {
	segments      []Segment
	primary       []Range
	context       []Range
	primaryFiles  []string
	changedLines  int
	workloadBytes int
}

var hunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// Build parses an exact Git unified diff and returns its deterministic ordered
// review-unit manifest. It preserves file and hunk order and never reorders or
// drops primary changed lines.
func Build(diff []byte, runID, baseSHA, checkpointSHA string, policy Policy) (Manifest, error) {
	policy = normalizePolicy(policy)
	if strings.TrimSpace(runID) == "" {
		return Manifest{}, errors.New("review unit manifest run id is required")
	}
	if strings.TrimSpace(baseSHA) == "" || strings.TrimSpace(checkpointSHA) == "" {
		return Manifest{}, errors.New("review unit manifest base and checkpoint SHAs are required")
	}
	if len(diff) == 0 {
		manifest := emptyManifest(runID, baseSHA, checkpointSHA, policy)
		manifest.Units = []Unit{{ID: unitID(1), Ordinal: 1, WorkloadBytes: 0, DiffSHA256: sha256Hex(nil), Segments: []Segment{}}}
		return finalizeManifest(manifest)
	}

	files, err := parseFiles(diff)
	if err != nil {
		return Manifest{}, err
	}
	fragments := make([]fragment, 0, len(files))
	for _, file := range files {
		fileFragments, fragmentErr := fragmentsForFile(diff, file, policy)
		if fragmentErr != nil {
			return Manifest{}, fragmentErr
		}
		fragments = append(fragments, fileFragments...)
	}
	if len(fragments) == 0 {
		return Manifest{}, errors.New("review diff contains no partitionable file sections")
	}

	manifest := emptyManifest(runID, baseSHA, checkpointSHA, policy)
	manifest.DiffBytes = len(diff)
	manifest.DiffSHA256 = sha256Hex(diff)
	var current *Unit
	for _, part := range fragments {
		if part.workloadBytes > policy.MaxUnitBytes {
			return Manifest{}, fmt.Errorf("review fragment workload %d exceeds unit limit %d", part.workloadBytes, policy.MaxUnitBytes)
		}
		merged := []Segment(nil)
		if current != nil {
			merged = unionSegments(append(append([]Segment(nil), current.Segments...), part.segments...))
		}
		if current == nil || segmentWorkload(merged) > policy.MaxUnitBytes {
			if current != nil {
				manifest.Units = append(manifest.Units, *current)
			}
			current = &Unit{Ordinal: len(manifest.Units) + 1}
			merged = unionSegments(part.segments)
		}
		current.WorkloadBytes = segmentWorkload(merged)
		current.Segments = merged
		current.PrimaryRanges = append(current.PrimaryRanges, part.primary...)
		current.ContextRanges = append(current.ContextRanges, part.context...)
		current.ChangedLines += part.changedLines
		for _, path := range part.primaryFiles {
			if !contains(current.PrimaryFiles, path) {
				current.PrimaryFiles = append(current.PrimaryFiles, path)
			}
		}
	}
	if current != nil {
		manifest.Units = append(manifest.Units, *current)
	}
	for index := range manifest.Units {
		unit := &manifest.Units[index]
		unit.ID = unitID(index + 1)
		unit.Ordinal = index + 1
		evidence, evidenceErr := UnitDiff(diff, *unit)
		if evidenceErr != nil {
			return Manifest{}, evidenceErr
		}
		unit.DiffSHA256 = sha256Hex(evidence)
		manifest.ChangedLines += unit.ChangedLines
	}
	return finalizeManifest(manifest)
}

// normalizePolicy applies the documented defaults while preserving explicit
// positive repository and host values.
func normalizePolicy(policy Policy) Policy {
	if policy.SchemaVersion == 0 {
		policy.SchemaVersion = SchemaVersion
	}
	if policy.PolicyVersion == "" {
		policy.PolicyVersion = PolicyVersion
	}
	if policy.MaxUnitBytes <= 0 {
		policy.MaxUnitBytes = DefaultMaxUnitBytes
	}
	if policy.MaxUnits <= 0 {
		policy.MaxUnits = DefaultMaxUnits
	}
	if policy.ContextLines <= 0 || policy.ContextLines > DefaultContextLines {
		policy.ContextLines = DefaultContextLines
	}
	return policy
}

// emptyManifest creates the common immutable manifest fields for an empty or
// parsed exact-checkpoint diff.
func emptyManifest(runID, baseSHA, checkpointSHA string, policy Policy) Manifest {
	return Manifest{
		SchemaVersion: policy.SchemaVersion,
		PolicyVersion: policy.PolicyVersion,
		RunID:         runID,
		BaseSHA:       baseSHA,
		CheckpointSHA: checkpointSHA,
		DiffBytes:     0,
		DiffSHA256:    sha256Hex(nil),
		MaxUnitBytes:  policy.MaxUnitBytes,
		MaxUnits:      policy.MaxUnits,
		ContextLines:  policy.ContextLines,
		Units:         []Unit{},
	}
}

// finalizeManifest computes the stable manifest digest and checks its primary
// changed-line ownership invariant before returning it.
func finalizeManifest(manifest Manifest) (Manifest, error) {
	manifestData := manifest
	manifestData.ManifestSHA256 = ""
	encoded, err := json.Marshal(manifestData)
	if err != nil {
		return Manifest{}, fmt.Errorf("encode review unit manifest: %w", err)
	}
	manifest.ManifestSHA256 = sha256Hex(encoded)
	if err := VerifyPrimaryCoverage(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// UnitDiff derives one unit's self-contained evidence from the canonical diff.
// It validates every segment before copying so a persisted manifest cannot
// escape the canonical artifact's byte bounds.
func UnitDiff(diff []byte, unit Unit) ([]byte, error) {
	if unit.WorkloadBytes < 0 {
		return nil, errors.New("review unit workload must not be negative")
	}
	evidence := make([]byte, 0, unit.WorkloadBytes)
	previousEnd := -1
	for index, segment := range unit.Segments {
		if segment.StartByte < 0 || segment.EndByte < segment.StartByte || segment.EndByte > len(diff) {
			return nil, fmt.Errorf("review unit segment %d is outside canonical diff", index)
		}
		if previousEnd > segment.StartByte {
			return nil, fmt.Errorf("review unit segment %d overlaps a prior primary segment", index)
		}
		evidence = append(evidence, diff[segment.StartByte:segment.EndByte]...)
		previousEnd = segment.EndByte
	}
	if len(evidence) != unit.WorkloadBytes {
		return nil, fmt.Errorf("review unit evidence is %d bytes, recorded workload is %d", len(evidence), unit.WorkloadBytes)
	}
	if unit.DiffSHA256 != "" && sha256Hex(evidence) != unit.DiffSHA256 {
		return nil, errors.New("review unit evidence SHA-256 does not match manifest")
	}
	return evidence, nil
}

// VerifyPrimaryCoverage checks that manifest primary assignments are unique and
// ordered. Context overlap is intentionally excluded from this invariant.
func VerifyPrimaryCoverage(manifest Manifest) error {
	seen := make(map[string]struct{})
	lastOrdinal := 0
	for index, unit := range manifest.Units {
		if unit.Ordinal != index+1 || unit.Ordinal <= lastOrdinal {
			return fmt.Errorf("review unit ordinal %d is not ordered", unit.Ordinal)
		}
		lastOrdinal = unit.Ordinal
		if unit.WorkloadBytes < 0 {
			return fmt.Errorf("review unit %d workload is negative", unit.Ordinal)
		}
		changedLines := 0
		for _, primary := range unit.PrimaryRanges {
			if primary.StartLine <= 0 || primary.EndLine < primary.StartLine || primary.Path == "" || primary.Side == "" {
				return fmt.Errorf("review unit %d has an invalid primary range", unit.Ordinal)
			}
			changedLines += primary.EndLine - primary.StartLine + 1
			for line := primary.StartLine; line <= primary.EndLine; line++ {
				key := rangeLineKey(primary, line)
				if _, exists := seen[key]; exists {
					return fmt.Errorf("review primary line %s is assigned more than once", key)
				}
				seen[key] = struct{}{}
			}
		}
		if unit.ChangedLines != changedLines {
			return fmt.Errorf("review unit %d changed-line count is %d, primary ranges cover %d", unit.Ordinal, unit.ChangedLines, changedLines)
		}
	}
	return nil
}

// parseFiles splits a unified diff into ordered file sections without changing
// any source bytes.
func parseFiles(diff []byte) ([]diffFile, error) {
	lines := splitLines(diff)
	starts := make([]int, 0)
	for index, line := range lines {
		if strings.HasPrefix(line.text, "diff --git ") {
			starts = append(starts, index)
		}
	}
	if len(starts) == 0 {
		return []diffFile{{start: 0, end: len(diff), path: "synthetic", hunks: parseHunks(lines, 0, len(lines))}}, nil
	}
	files := make([]diffFile, 0, len(starts))
	for index, startIndex := range starts {
		endIndex := len(lines)
		if index+1 < len(starts) {
			endIndex = starts[index+1]
		}
		file := diffFile{start: lines[startIndex].start, end: lines[endIndex-1].end, path: diffPath(lines[startIndex].text)}
		if file.path == "" {
			file.path = fmt.Sprintf("file-%d", index+1)
		}
		file.hunks = parseHunks(lines, startIndex, endIndex)
		files = append(files, file)
	}
	return files, nil
}

// splitLines records byte bounds for every complete line in a diff artifact.
func splitLines(diff []byte) []diffLine {
	lines := make([]diffLine, 0)
	start := 0
	for start < len(diff) {
		end := start
		for end < len(diff) && diff[end] != '\n' {
			end++
		}
		if end < len(diff) {
			end++
		}
		lines = append(lines, diffLine{start: start, end: end, text: string(diff[start:end])})
		start = end
	}
	return lines
}

// diffPath extracts the repository path from a Git file-section header.
func diffPath(header string) string {
	header = strings.TrimSuffix(header, "\n")
	if marker := strings.LastIndex(header, " b/"); marker >= 0 {
		return strings.TrimPrefix(header[marker+3:], "b/")
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "diff --git "))
}

// parseHunks parses ordered hunk headers and assigns old/new line identities
// to their preserved body lines.
func parseHunks(lines []diffLine, start, end int) []diffHunk {
	indices := make([]int, 0)
	for index := start; index < end; index++ {
		if strings.HasPrefix(lines[index].text, "@@ ") {
			indices = append(indices, index)
		}
	}
	hunks := make([]diffHunk, 0, len(indices))
	for index, headerIndex := range indices {
		bodyEnd := end
		if index+1 < len(indices) {
			bodyEnd = indices[index+1]
		}
		match := hunkHeaderPattern.FindStringSubmatch(strings.TrimSuffix(lines[headerIndex].text, "\n"))
		if len(match) == 0 {
			continue
		}
		oldLine := parsePositiveInt(match[1])
		newLine := parsePositiveInt(match[3])
		body := make([]diffLine, 0, bodyEnd-headerIndex-1)
		for lineIndex := headerIndex + 1; lineIndex < bodyEnd; lineIndex++ {
			line := lines[lineIndex]
			line.oldLine = oldLine
			line.newLine = newLine
			if len(line.text) == 0 {
				body = append(body, line)
				continue
			}
			switch line.text[0] {
			case ' ':
				oldLine++
				newLine++
			case '-':
				oldLine++
			case '+':
				newLine++
			}
			body = append(body, line)
		}
		hunks = append(hunks, diffHunk{headerStart: lines[headerIndex].start, headerEnd: lines[headerIndex].end, body: body})
	}
	return hunks
}

// parsePositiveInt decodes a unified-diff line number and normalizes a zero
// start used by an empty-side hunk to the first representable source line.
func parsePositiveInt(value string) int {
	result := 0
	for _, character := range value {
		result = result*10 + int(character-'0')
	}
	if result < 1 {
		return 1
	}
	return result
}

// fragmentsForFile keeps a complete file together when possible, otherwise
// splitting only at hunk or legal line-window boundaries.
func fragmentsForFile(diff []byte, file diffFile, policy Policy) ([]fragment, error) {
	if file.start < 0 || file.end > len(diff) || file.start >= file.end {
		return nil, fmt.Errorf("review diff file %q has invalid byte bounds", file.path)
	}
	whole := fragment{segments: []Segment{{StartByte: file.start, EndByte: file.end}}, primaryFiles: []string{file.path}}
	whole.primary, whole.context = rangesForAllHunks(file.path, file.hunks, policy.ContextLines)
	whole.changedLines = len(whole.primary)
	whole.workloadBytes = file.end - file.start
	if whole.workloadBytes <= policy.MaxUnitBytes || len(file.hunks) == 0 {
		if whole.workloadBytes > policy.MaxUnitBytes {
			return nil, fmt.Errorf("review file %q has no legal hunk boundary below %d bytes", file.path, policy.MaxUnitBytes)
		}
		return []fragment{whole}, nil
	}

	fragments := make([]fragment, 0, len(file.hunks))
	fileHeaderEnd := file.hunks[0].headerStart
	fileHeader := Segment{StartByte: file.start, EndByte: fileHeaderEnd}
	for hunkIndex, hunk := range file.hunks {
		prefix := []Segment{fileHeader, {StartByte: hunk.headerStart, EndByte: hunk.headerEnd}}
		bodyStart := hunk.headerEnd
		if len(hunk.body) > 0 {
			bodyStart = hunk.body[0].start
		}
		bodyEnd := bodyStart
		if len(hunk.body) > 0 {
			bodyEnd = hunk.body[len(hunk.body)-1].end
		}
		baseBytes := fileHeaderEnd - file.start + hunk.headerEnd - hunk.headerStart
		if len(hunk.body) == 0 {
			if baseBytes > policy.MaxUnitBytes {
				return nil, fmt.Errorf("review file %q hunk %d header exceeds %d bytes", file.path, hunkIndex+1, policy.MaxUnitBytes)
			}
			fragments = append(fragments, fragment{segments: prefix, primaryFiles: []string{file.path}, workloadBytes: baseBytes})
			continue
		}
		windows, err := hunkWindows(file.path, hunkIndex+1, hunk, prefix, baseBytes, policy)
		if err != nil {
			return nil, err
		}
		if hunkIndex == len(file.hunks)-1 && bodyEnd < file.end {
			// Git normally ends a file section at the hunk body. Preserve any
			// trailing marker or metadata with the final legal fragment. If it
			// cannot fit there, retain it as a separate header-bearing fragment
			// rather than silently dropping bytes from the review evidence.
			trailing := Segment{StartByte: bodyEnd, EndByte: file.end}
			trailingBytes := trailing.EndByte - trailing.StartByte
			last := &windows[len(windows)-1]
			if last.workloadBytes+trailingBytes <= policy.MaxUnitBytes {
				last.segments = append(last.segments, trailing)
				last.workloadBytes += trailingBytes
			} else if baseBytes+trailingBytes <= policy.MaxUnitBytes {
				windows = append(windows, fragment{segments: append([]Segment(nil), prefix...), primaryFiles: []string{file.path}, workloadBytes: baseBytes + trailingBytes})
				windows[len(windows)-1].segments = append(windows[len(windows)-1].segments, trailing)
			} else {
				return nil, fmt.Errorf("review file %q trailing metadata exceeds %d bytes", file.path, policy.MaxUnitBytes)
			}
		}
		fragments = append(fragments, windows...)
	}
	return fragments, nil
}

// hunkWindows creates maximal consecutive primary line windows with bounded
// neighboring context while preserving complete diff lines.
func hunkWindows(path string, hunkNumber int, hunk diffHunk, prefix []Segment, baseBytes int, policy Policy) ([]fragment, error) {
	windows := make([]fragment, 0)
	cursor := 0
	for cursor < len(hunk.body) {
		if baseBytes+hunk.body[cursor].end-hunk.body[cursor].start > policy.MaxUnitBytes {
			return nil, fmt.Errorf("review file %q hunk %d has no legal line fragment below %d bytes", path, hunkNumber, policy.MaxUnitBytes)
		}
		end := cursor + 1
		for end < len(hunk.body) && baseBytes+hunk.body[end].end-hunk.body[cursor].start <= policy.MaxUnitBytes {
			end++
		}
		// Include up to the configured neighbouring context lines when the
		// evidence still fits. Primary assignment remains cursor:end, so this
		// overlap can never duplicate ownership.
		contextStart := cursor
		contextEnd := end
		for contextStart > 0 && end-contextStart < end-cursor+policy.ContextLines {
			candidate := contextStart - 1
			if baseBytes+hunk.body[contextEnd-1].end-hunk.body[candidate].start > policy.MaxUnitBytes {
				break
			}
			contextStart = candidate
		}
		for contextEnd < len(hunk.body) && contextEnd-end < policy.ContextLines {
			candidate := contextEnd + 1
			if baseBytes+hunk.body[candidate-1].end-hunk.body[contextStart].start > policy.MaxUnitBytes {
				break
			}
			contextEnd = candidate
		}
		part := fragment{
			segments:      append(append([]Segment(nil), prefix...), Segment{StartByte: hunk.body[contextStart].start, EndByte: hunk.body[contextEnd-1].end}),
			workloadBytes: baseBytes + hunk.body[contextEnd-1].end - hunk.body[contextStart].start,
			primaryFiles:  []string{path},
		}
		for index := cursor; index < end; index++ {
			line := hunk.body[index]
			if rangeValue, ok := changedRange(path, hunkNumber, line); ok {
				part.primary = append(part.primary, rangeValue)
				part.changedLines++
			}
		}
		for index := contextStart; index < contextEnd; index++ {
			line := hunk.body[index]
			if rangeValue, ok := contextRange(path, hunkNumber, line, hunk.body, index, policy.ContextLines); ok {
				part.context = append(part.context, rangeValue)
			}
		}
		windows = append(windows, part)
		cursor = end
	}
	return windows, nil
}

// rangesForAllHunks collects primary and nearby context ranges for a complete
// file-sized fragment.
func rangesForAllHunks(path string, hunks []diffHunk, contextLines int) ([]Range, []Range) {
	var primary, context []Range
	for hunkIndex, hunk := range hunks {
		for index, line := range hunk.body {
			if value, ok := changedRange(path, hunkIndex+1, line); ok {
				primary = append(primary, value)
			}
			if value, ok := contextRange(path, hunkIndex+1, line, hunk.body, index, contextLines); ok {
				context = append(context, value)
			}
		}
	}
	return primary, context
}

// changedRange maps one removed or added diff line to its primary source range.
func changedRange(path string, hunk int, line diffLine) (Range, bool) {
	if len(line.text) == 0 {
		return Range{}, false
	}
	switch line.text[0] {
	case '-':
		return Range{Path: path, Hunk: hunk, Side: "old", StartLine: line.oldLine, EndLine: line.oldLine}, true
	case '+':
		return Range{Path: path, Hunk: hunk, Side: "new", StartLine: line.newLine, EndLine: line.newLine}, true
	default:
		return Range{}, false
	}
}

// contextRange maps one nearby unchanged diff line to a bounded judgment-only
// context range when it is adjacent to a change.
func contextRange(path string, hunk int, line diffLine, body []diffLine, index, width int) (Range, bool) {
	if len(line.text) == 0 || line.text[0] != ' ' || width <= 0 {
		return Range{}, false
	}
	nearChange := false
	for offset := 1; offset <= width; offset++ {
		for _, candidate := range []int{index - offset, index + offset} {
			if candidate >= 0 && candidate < len(body) {
				if value, ok := changedRange(path, hunk, body[candidate]); ok {
					_ = value
					nearChange = true
				}
			}
		}
	}
	if !nearChange {
		return Range{}, false
	}
	return Range{Path: path, Hunk: hunk, Side: "both", StartLine: line.newLine, EndLine: line.newLine}, true
}

// rangeLineKey creates the stable uniqueness key for one primary source line.
func rangeLineKey(value Range, line int) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%d", value.Path, value.Hunk, value.Side, line)
}

// sha256Hex returns the lowercase SHA-256 digest used by manifest identities.
func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// contains reports whether wanted is already present in values.
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// unionSegments returns the ordered byte coverage of fragments packed into
// one unit. Repeated file headers and overlapping context are emitted once so
// same-file hunks can still pack greedily without duplicate evidence.
func unionSegments(values []Segment) []Segment {
	if len(values) == 0 {
		return []Segment{}
	}
	ordered := append([]Segment(nil), values...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].StartByte == ordered[right].StartByte {
			return ordered[left].EndByte < ordered[right].EndByte
		}
		return ordered[left].StartByte < ordered[right].StartByte
	})
	merged := make([]Segment, 0, len(ordered))
	for _, segment := range ordered {
		if len(merged) == 0 || segment.StartByte > merged[len(merged)-1].EndByte {
			merged = append(merged, segment)
			continue
		}
		if segment.EndByte > merged[len(merged)-1].EndByte {
			merged[len(merged)-1].EndByte = segment.EndByte
		}
	}
	return merged
}

// segmentWorkload returns the exact byte workload represented by segments.
func segmentWorkload(values []Segment) int {
	workload := 0
	for _, segment := range values {
		workload += segment.EndByte - segment.StartByte
	}
	return workload
}

// unitID returns the stable ordinal identity shared by both review axes.
func unitID(ordinal int) string {
	return fmt.Sprintf("unit-%03d", ordinal)
}

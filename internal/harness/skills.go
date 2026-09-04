package harness

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// SkillEvidenceFile is the repository-relative path of the recorded worker
// skill smoke evidence. Startup reads the recorded result instead of making a
// model call, which would be paid and nondeterministic.
const SkillEvidenceFile = "worker/skill-smoke.json"

// MandatorySkills returns the worker skills the embedded role prompts require
// by name, in deterministic order. Every shipped harness must advertise all of
// them, because a repository may assign any role to any supported harness.
func MandatorySkills() []string {
	return []string{"implement", "specification-review", "standards-review"}
}

// SkillSmokeRecord is one recorded proof that a real worker invocation of one
// harness loaded and used the role-mandated skills. It is keyed by the
// immutable worker image digest and the harness version observed in it.
type SkillSmokeRecord struct {
	// ImageDigest is the worker image digest the smoke ran against.
	ImageDigest string `json:"image_digest"`
	// Harness is the harness that was invoked.
	Harness string `json:"harness"`
	// HarnessVersion is the version line that harness reported in the image.
	HarnessVersion string `json:"harness_version"`
	// Skills are the role-mandated skills the invocation proved usable.
	Skills []string `json:"skills"`
	// VerifiedAt is the RFC 3339 instant the smoke was recorded.
	VerifiedAt string `json:"verified_at"`
}

// SkillSmokeEvidence is the checked-in set of recorded smoke results.
type SkillSmokeEvidence struct {
	// SchemaVersion identifies the evidence file format.
	SchemaVersion int `json:"schema_version"`
	// Records contains one result per worker digest and harness.
	Records []SkillSmokeRecord `json:"records"`
}

// LoadSkillSmokeEvidence reads the recorded smoke evidence from one file.
func LoadSkillSmokeEvidence(path string) (SkillSmokeEvidence, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return SkillSmokeEvidence{}, errors.New("the worker skill smoke evidence file is unavailable")
	}
	var evidence SkillSmokeEvidence
	if err := json.Unmarshal(body, &evidence); err != nil {
		return SkillSmokeEvidence{}, errors.New("the worker skill smoke evidence file is not readable")
	}
	return evidence, nil
}

// Covers reports whether the evidence contains a record proving one harness
// build in one worker image loaded every required skill.
func (e SkillSmokeEvidence) Covers(digest, harness, version string, required []string) bool {
	for _, record := range e.Records {
		if !strings.EqualFold(record.ImageDigest, digest) || record.Harness != harness {
			continue
		}
		if record.HarnessVersion != version || !coversSkills(record.Skills, required) {
			continue
		}
		return true
	}
	return false
}

// coversSkills reports whether a recorded skill set contains every required
// skill.
func coversSkills(recorded, required []string) bool {
	present := make(map[string]struct{}, len(recorded))
	for _, skill := range recorded {
		present[skill] = struct{}{}
	}
	for _, skill := range required {
		if _, ok := present[skill]; !ok {
			return false
		}
	}
	return true
}

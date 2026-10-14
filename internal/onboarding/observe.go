package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gateproof"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// ImageChecker reports whether an exact worker image exists locally without
// pulling it.
type ImageChecker interface {
	CheckImage(context.Context, worker.ImageReference) error
}

// Observer observes the current inputs of one plan. It only reads: it never
// writes the checkout, the host configuration, or the plan.
type Observer struct {
	// Images checks the pinned worker image.
	Images ImageChecker
	// Release names the running Factory build.
	Release Release
	// VerifyResources returns the identity of a prepared resource directory.
	VerifyResources func(directory string) (string, error)
}

// Observe reads the inputs and the cheap, locally observed steps of a plan.
func (o Observer) Observe(ctx context.Context, plan Plan) Observation {
	observation := Observation{Inputs: map[Input]string{}, Release: o.Release}
	observation.Inputs[InputRelease] = digestOf("release", o.Release.Version, o.Release.Revision, o.Release.ResourceIdentity)
	observation.Resources = o.resources(plan.Resources)
	policy := o.policy(plan.Repository, &observation)
	if policy != nil {
		o.image(ctx, plan.Repository, *policy, &observation)
		observation.Smoke, observation.Inputs[InputSkillEvidence] = smoke(plan.Repository, *policy)
	}
	if fingerprint, err := gateproof.Fingerprint(ctx, plan.Repository, harness.SkillEvidenceFile); err == nil {
		observation.Inputs[InputGateInputs] = fingerprint
	}
	observation.Registration, observation.Inputs[InputRegistration] = registration(plan.HostConfig, plan.Repository)
	return observation
}

// resources verifies the prepared resource directory of the plan.
func (o Observer) resources(directory string) Check {
	if directory == "" {
		return Check{Problem: "the plan names no prepared resource directory"}
	}
	identity, err := o.VerifyResources(directory)
	if err != nil {
		return Check{Problem: err.Error()}
	}
	if identity != o.Release.ResourceIdentity {
		return Check{Problem: fmt.Sprintf("%s holds the resources %s of another release, not %s", directory, identity, o.Release.ResourceIdentity)}
	}
	return Check{Done: true}
}

// policy reads and validates factory.yaml and records its identity. It
// returns the policy only when it is valid.
func (o Observer) policy(repository string, observation *Observation) *config.RepositoryConfig {
	path := config.DefaultRepositoryConfigPath(repository)
	body, err := os.ReadFile(path)
	if err != nil {
		observation.Configuration = Check{Problem: "factory.yaml is not readable in the repository"}
		return nil
	}
	observation.Inputs[InputPolicy] = digestOf("policy", string(body))
	diagnosis := config.DiagnoseRepositoryFile(path)
	if !diagnosis.Valid {
		problem := "factory.yaml is invalid"
		if len(diagnosis.Diagnostics) != 0 {
			first := diagnosis.Diagnostics[0]
			problem += ": " + strings.TrimSpace(first.Field+" "+first.Message)
		}
		observation.Configuration = Check{Problem: problem}
		return nil
	}
	policy, err := config.LoadRepository(path)
	if err != nil {
		observation.Configuration = Check{Problem: "factory.yaml is invalid"}
		return nil
	}
	observation.Configuration = Check{Done: true}
	observation.Inputs[InputWorkerImage] = policy.WorkerBuild.Image + "@" + policy.WorkerBuild.Digest
	return &policy
}

// image checks the pinned image and identifies its build context: the
// directory of the worker definition, without the smoke evidence, which
// the image does not contain.
func (o Observer) image(ctx context.Context, repository string, policy config.RepositoryConfig, observation *Observation) {
	context := filepath.Dir(filepath.Join(repository, filepath.FromSlash(policy.WorkerBuild.Definition)))
	if identity, err := directoryDigest(context, filepath.Join(repository, filepath.FromSlash(harness.SkillEvidenceFile))); err == nil {
		observation.Inputs[InputWorkerDefinition] = identity
	}
	reference := worker.ImageReference{Name: policy.WorkerBuild.Image, Digest: policy.WorkerBuild.Digest}
	if err := o.Images.CheckImage(ctx, reference); err != nil {
		observation.Image = Check{Problem: "the pinned worker image is not available locally, or Docker is not reachable: " + err.Error()}
		return
	}
	observation.Image = Check{Done: true}
}

// directoryDigest identifies the relative paths, modes, and contents of the
// regular files and links below root, except the omitted file.
func directoryDigest(root, omitted string) (string, error) {
	hasher := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path == omitted {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var content []byte
		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			content = []byte(link)
			if err != nil {
				return err
			}
		} else if content, err = os.ReadFile(path); err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		_, err = fmt.Fprintf(hasher, "%s\x00%o\x00%s\x00", filepath.ToSlash(relative), info.Mode().Perm()&0o111, hex.EncodeToString(sum[:]))
		return err
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// smoke reports the recorded skill smoke coverage of each harness the policy
// can select, and the identity of the records of the pinned digest. It uses
// the same selection rule and required skills as the doctor.
func smoke(repository string, policy config.RepositoryConfig) ([]HarnessSmoke, string) {
	evidence, err := harness.LoadSkillSmokeEvidence(filepath.Join(repository, filepath.FromSlash(harness.SkillEvidenceFile)))
	if err != nil {
		evidence = harness.SkillSmokeEvidence{}
	}
	var pinned []harness.SkillSmokeRecord
	for _, record := range evidence.Records {
		if strings.EqualFold(record.ImageDigest, policy.WorkerBuild.Digest) {
			pinned = append(pinned, record)
		}
	}
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].Harness < pinned[j].Harness })
	encoded, _ := json.Marshal(pinned)
	var coverage []HarnessSmoke
	for _, name := range []config.Harness{config.HarnessClaude, config.HarnessCodex} {
		if !harness.RequiresSkillSmoke(&policy, string(name)) {
			continue
		}
		coverage = append(coverage, harnessCoverage(pinned, string(name), harness.RequiredSkills(&policy, string(name))))
	}
	return coverage, digestOf("skill_evidence", string(encoded))
}

// harnessCoverage compares the recorded skills of one harness with the
// required ones.
func harnessCoverage(records []harness.SkillSmokeRecord, name string, required []string) HarnessSmoke {
	coverage := HarnessSmoke{Harness: name, Required: required, Verified: []string{}, Missing: []string{}}
	var recorded []string
	for _, record := range records {
		if record.Harness == name {
			coverage.HarnessVersion = record.HarnessVersion
			recorded = record.Skills
		}
	}
	for _, skill := range required {
		if slices.Contains(recorded, skill) {
			coverage.Verified = append(coverage.Verified, skill)
		} else {
			coverage.Missing = append(coverage.Missing, skill)
		}
	}
	return coverage
}

// registration checks that the host configuration registers the repository
// and returns the identity of that registration.
func registration(path, repository string) (Check, string) {
	host, err := config.LoadHost(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Check{Problem: "no host configuration exists at " + path}, ""
	}
	if err != nil {
		return Check{Problem: "the host configuration at " + path + " is not usable: " + err.Error()}, ""
	}
	if len(host.Repositories) == 0 {
		return Check{Problem: "the host configuration at " + path + " registers no repository"}, ""
	}
	registered := host.Repositories[0]
	if !samePath(registered.Path, repository) {
		return Check{Problem: "the host configuration at " + path + " registers another repository; ask the operator for a separate configuration path"}, ""
	}
	encoded, err := json.Marshal(registered)
	if err != nil {
		return Check{Problem: err.Error()}, ""
	}
	return Check{Done: true}, digestOf("registration", path, string(encoded))
}

// samePath reports whether two paths name the same directory after symbolic
// links are resolved.
func samePath(first, second string) bool {
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return filepath.Clean(path)
	}
	return resolve(first) == resolve(second)
}

// digestOf returns the sha256 identity of a domain and its parts.
func digestOf(domain string, parts ...string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Refresh records the first observation of the pinned worker image with its
// build context. A later observation of the same image keeps that record, so
// an edit of the build context without a rebuild makes the image stale. A
// new pinned image replaces the record.
func (p *Plan) Refresh(observation Observation) {
	if !observation.Image.Done {
		return
	}
	current := observation.Inputs[InputWorkerImage]
	if recorded, ok := p.Evidence[StepImage]; ok && recorded.Inputs[InputWorkerImage] == current {
		return
	}
	p.Record(StepImage, Evidence{Outcome: OutcomePassed, Inputs: observation.Subset(StepImage)})
}

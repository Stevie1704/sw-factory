package worker

import (
	"context"
	"errors"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// ReleaseLabel is the image label that names the Factory release whose worker
// base image a worker image was built from. Repository worker images inherit
// it from the base image.
const ReleaseLabel = "io.github.stevie1704.sw-factory.release"

// ReleaseChecker is the worker diagnosis seam that reads the Factory release
// a pinned worker image was built for.
type ReleaseChecker interface {
	ImageRelease(context.Context, ImageReference) (string, error)
}

// Release diagnosis conditions. Codes are stable; wording may change.
var (
	releaseAdapterMissing = doctor.AdapterMissing("worker.release.adapter_missing")
	releaseMismatch       = doctor.Condition{Code: "worker.release.mismatch", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	releaseUnreadable     = doctor.Condition{Code: "worker.release.unreadable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectInspection, Topic: "setup"}
)

// maxReleaseLabelLength bounds the label value a diagnosis repeats, because
// the label is image content the repository controls.
const maxReleaseLabelLength = 128

// releaseCheckName is the diagnosis name of the release compatibility check.
const releaseCheckName = "worker release"

// ReleaseCheck returns the check that a release coordinator runs only with a
// worker image built from the base image of the same release, so a new
// coordinator never silently combines with older worker helpers and skills. A
// development coordinator, whose release is empty, accepts any worker image.
func ReleaseCheck(readiness *Readiness, checker ReleaseChecker, coordinatorRelease string) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if coordinatorRelease == "" {
			result := doctor.Success("worker.release.unchecked_development", releaseCheckName)
			result.Detail = "a development build accepts any worker base image"
			return result
		}
		if checker == nil {
			return doctor.Failure(releaseAdapterMissing, releaseCheckName, "the worker release diagnosis adapter is unavailable", "configure the Docker worker runtime")
		}
		if readiness.Image(ctx) != nil {
			return doctor.Skipped("worker.release.not_checked", releaseCheckName, ImageUnavailableReason)
		}
		workerRelease, err := checker.ImageRelease(ctx, readiness.image)
		if err != nil {
			return ProbeFailure(releaseUnreadable, releaseCheckName, err, "the release label of the pinned worker image cannot be read", "verify the coordinator account can inspect the configured worker image")
		}
		facts := []doctor.Fact{{Name: "coordinator_release", Value: coordinatorRelease}, {Name: "worker_release", Value: workerRelease}}
		if workerRelease != coordinatorRelease {
			return doctor.Failure(releaseMismatch, releaseCheckName,
				"the pinned worker image was not built from the worker base image of factory "+coordinatorRelease,
				"rebuild the worker image from the worker_base_image that factory version --json names, then record the new digest in factory.yaml and a new skill smoke").WithFacts(facts...)
		}
		return doctor.Success("worker.release.ready", releaseCheckName).WithFacts(facts...)
	}
}

// ImageRelease returns the Factory release label of the exact pinned worker
// image without pulling it, or an empty string when the image has none.
func (r *DockerRuntime) ImageRelease(ctx context.Context, image ImageReference) (string, error) {
	if err := validateImageReference(image); err != nil {
		return "", err
	}
	result, err := r.runDocker(ctx, []string{"image", "inspect", "--format", `{{index .Config.Labels "` + ReleaseLabel + `"}}`, imageReference(image.Name, image.Digest)})
	if err != nil {
		return "", redactCause(err, "worker image release label inspection failed")
	}
	release := strings.TrimSpace(result.Stdout)
	if release == "<no value>" {
		return "", nil
	}
	if strings.ContainsAny(release, "\n\r\t ") || len(release) > maxReleaseLabelLength {
		return "", errors.New("worker image release label is malformed")
	}
	return release, nil
}

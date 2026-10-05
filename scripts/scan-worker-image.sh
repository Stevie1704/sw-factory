#!/bin/sh

# Scan one local worker image: its OS, npm, harness, and Go packages with the
# pinned grype image, and the Go helpers and toolchain it ships with the pinned
# govulncheck through scan-go-artifacts.sh. The image is never pulled here, so
# the scan covers exactly the artifact that was built or pulled before.
#
# Usage: scan-worker-image.sh [IMAGE_REFERENCE]
# The reference defaults to worker_build.image@worker_build.digest from
# factory.yaml.
# Exit status: 0 clean, 1 actionable findings or an unapproved Go release,
# 2 a scanner could not produce a result. Every setup step exits 2, so an
# infrastructure failure is never reported as findings.
set -eu

DOCKER="${DOCKER:-docker}"
GRYPE_IMAGE=anchore/grype:v0.120.0@sha256:5c88961f4130e830542d441c7ed6c78baa28e799163abac53d2be4923fb5ab7d
REPOSITORY_ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)"
SCAN_REPORT_DIR="${SCAN_REPORT_DIR:-$REPOSITORY_ROOT/.scan-reports}"
SUPPRESSIONS="$REPOSITORY_ROOT/security/vulnerability-suppressions.yaml"
WORKER_IMAGE="$(sed -n 's/^  image: \(.*\)$/\1/p' "$REPOSITORY_ROOT/factory.yaml")"
WORKER_DIGEST="$(sed -n 's/^  digest: \(.*\)$/\1/p' "$REPOSITORY_ROOT/factory.yaml")"
reference="${1:-$WORKER_IMAGE@$WORKER_DIGEST}"

if ! "$DOCKER" image inspect "$reference" >/dev/null 2>&1; then
  echo "worker image $reference is not available locally; build or pull it first" >&2
  exit 2
fi
image_id="$("$DOCKER" image inspect "$reference" --format '{{.Id}}')" || exit 2

# The Docker daemon may run in a VM that shares only the home directory, so
# the scan inputs live below the repository root, as in build-worker.sh.
scan_root="$(mktemp -d "$REPOSITORY_ROOT/.worker-scan.XXXXXX")" || exit 2
container=""
# Remove the exported image, the extracted binaries, and the container.
cleanup_scan_inputs() {
  if [ -n "$container" ]; then
    "$DOCKER" rm "$container" >/dev/null 2>&1 || true
  fi
  rm -rf "$scan_root"
}
trap cleanup_scan_inputs EXIT HUP INT TERM

overall=0
# record_status keeps the most severe exit status.
record_status() {
  if [ "$1" -gt "$overall" ]; then
    overall="$1"
  fi
}

echo "Extracting Go helpers and toolchain from $reference"
mkdir "$scan_root/bin" || exit 2
container="$("$DOCKER" create --pull=never "$reference")" || exit 2
for path in /usr/local/bin/factory-report /usr/local/bin/factory-worker-headless /usr/local/go/bin/go; do
  if ! "$DOCKER" cp -L "$container:$path" "$scan_root/bin/"; then
    echo "could not extract $path from $reference" >&2
    record_status 2
  fi
done
if "$REPOSITORY_ROOT/scripts/scan-go-artifacts.sh" "$scan_root"/bin/*; then
  record_status 0
else
  record_status "$?"
fi

echo "Scanning $reference with $GRYPE_IMAGE"
mkdir -p "$SCAN_REPORT_DIR" || exit 2
if "$DOCKER" save --output "$scan_root/image.tar" "$reference" &&
  "$DOCKER" run --rm --pull=missing \
    --mount "type=bind,src=$scan_root,dst=/scan,readonly" \
    "$GRYPE_IMAGE" docker-archive:/scan/image.tar --output json --quiet >"$scan_root/grype.json" &&
  (cd "$REPOSITORY_ROOT" && go build -o "$scan_root/vulnscan" ./tools/vulnscan); then
  if "$scan_root/vulnscan" -scanner grype -artifact worker-image -identity "$reference ($image_id)" \
    -suppressions "$SUPPRESSIONS" -metadata "$SCAN_REPORT_DIR/scans.jsonl" <"$scan_root/grype.json"; then
    record_status 0
  else
    record_status "$?"
  fi
else
  echo "grype could not scan $reference" >&2
  record_status 2
fi

exit "$overall"

#!/bin/sh

# Publish a locally built and verified worker image, then print the registry
# manifest digest that belongs in factory.yaml. The image is scanned before
# the push, and nothing is pushed unless the scan is clean.
#
# Usage: publish-worker.sh [IMAGE:TAG]
# The reference defaults to the tag that scripts/build-worker.sh builds.
set -eu

DOCKER="${DOCKER:-docker}"
REPOSITORY_ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)"
WORKER_IMAGE="${WORKER_IMAGE:-ghcr.io/stevie1704/sw-factory-worker}"
WORKER_TAG="${WORKER_TAG:-v1}"
reference="${1:-$WORKER_IMAGE:$WORKER_TAG}"
image="${reference%:*}"

if ! "$DOCKER" image inspect "$reference" >/dev/null 2>&1; then
  echo "worker image $reference is not available locally; run make worker-build first" >&2
  exit 1
fi

echo "Scanning $reference before publishing"
"$REPOSITORY_ROOT/scripts/scan-worker-image.sh" "$reference"

echo "Pushing $reference"
"$DOCKER" push "$reference"

# The registry's manifest digest, not the local image ID, identifies a
# published worker.
manifest_digest="$("$DOCKER" buildx imagetools inspect "$reference" --format '{{json .Manifest}}' | jq -r '.digest')"
case "$manifest_digest" in
  sha256:????????????????????????????????????????????????????????????????) ;;
  *)
    echo "registry returned an invalid manifest digest for $reference: $manifest_digest" >&2
    exit 1
    ;;
esac

echo "Verifying the published digest resolves to the pushed image"
"$DOCKER" pull "$image@$manifest_digest" >/dev/null
test "$("$DOCKER" image inspect "$image@$manifest_digest" --format '{{.Id}}')" = \
  "$("$DOCKER" image inspect "$reference" --format '{{.Id}}')"

cat <<EOF

Worker image published.
worker_build:
  image: $image
  digest: $manifest_digest
  definition: worker/Dockerfile

Record skill smoke evidence for this digest with ./scripts/smoke-skills.sh.
EOF

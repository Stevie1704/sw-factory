#!/bin/sh

# Build and locally verify the versioned worker image chain, then print the
# immutable digest snippet that belongs in the repository factory.yaml.
set -eu

DOCKER="${DOCKER:-docker}"
WORKER_BASE_IMAGE="${WORKER_BASE_IMAGE:-ghcr.io/stevie1704/sw-factory-base:v1}"
WORKER_IMAGE="${WORKER_IMAGE:-ghcr.io/stevie1704/sw-factory-worker}"
WORKER_TAG="${WORKER_TAG:-v1}"
GO_VERSION="${GO_VERSION:-1.25.0}"
CLAUDE_VERSION="${CLAUDE_VERSION:-2.1.232}"
CODEX_VERSION="${CODEX_VERSION:-0.148.0}"
WORKER_PLATFORM="${WORKER_PLATFORM:-}"
WORKER_REFERENCE="${WORKER_IMAGE}:${WORKER_TAG}"

platform_args=""
if [ -n "$WORKER_PLATFORM" ]; then
  platform_args="--platform=$WORKER_PLATFORM"
fi

echo "Building factory base image $WORKER_BASE_IMAGE"
# shellcheck disable=SC2086
"$DOCKER" build $platform_args \
  --build-arg "GO_VERSION=$GO_VERSION" \
  --build-arg "CLAUDE_VERSION=$CLAUDE_VERSION" \
  --build-arg "CODEX_VERSION=$CODEX_VERSION" \
  --build-arg "FACTORY_BASE_VERSION=1" \
  --file worker/base.Dockerfile \
  --tag "$WORKER_BASE_IMAGE" \
  .

echo "Building repository worker image $WORKER_REFERENCE"
# shellcheck disable=SC2086
"$DOCKER" build $platform_args --pull=false \
  --build-arg "FACTORY_BASE_IMAGE=$WORKER_BASE_IMAGE" \
  --build-arg "GO_VERSION=$GO_VERSION" \
  --file worker/Dockerfile \
  --tag "$WORKER_REFERENCE" \
  worker

image_id="$($DOCKER image inspect "$WORKER_REFERENCE" --format '{{.Id}}')"
case "$image_id" in
  sha256:????????????????????????????????????????????????????????????????) ;;
  *)
    echo "worker image inspection returned an invalid image id: $image_id" >&2
    exit 1
    ;;
esac

# Note: This is Docker's local content-addressable image ID (config digest),
# not a registry manifest digest. See docs/configuration.md and
# docs/worker-runtime.md for the documented limitation. A registry publish
# workflow should replace this with the registry's manifest digest.
local_image_id="$image_id"
local_reference="$WORKER_IMAGE@$local_image_id"

echo "Verifying local image reference $local_reference"
"$DOCKER" image inspect "$local_reference" >/dev/null
"$DOCKER" run --rm --pull=never \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  "$local_reference" /bin/sh -c '
  test "$(id -u)" = 10001
  test "$HOME" = /home/factory
  test -d /work && test -d /git && test -d /cache && test -d /invocation && test -d /results
  test ! -w /run/factory-auth
  test -x /usr/local/bin/factory-report
  command -v codex >/dev/null
  command -v claude >/dev/null
  command -v git >/dev/null
  command -v go >/dev/null
  command -v gofmt >/dev/null
  test "$PATH" = /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
'

repository_root="$(pwd -P)"
temporary_root="$(mktemp -d "$repository_root/.worker-build.XXXXXX")"
worker_checkout="$temporary_root/checkout"
git_projection="$temporary_root/git"
worker_archive="$temporary_root/checkout.tar"
mkdir "$worker_checkout" "$git_projection"

# Remove temporary sanitized worker inputs after the Docker gate run exits.
cleanup_worker_inputs() {
  rm -rf "$temporary_root"
}
trap cleanup_worker_inputs EXIT HUP INT TERM

# Recreate the checkout without Git metadata, harness state, or host secrets.
# The production adapter supplies the same shape through its Git projection.
tar -C "$repository_root" \
  --exclude='./.git' \
  --exclude='./.factory-worktrees' \
  --exclude='./.worker-build.*' \
  --exclude='./.serena' \
  --exclude='./.ua' \
  --exclude='./spike' \
  --exclude='*/.run' \
  --exclude='*/.codex' \
  --exclude='*/.claude' \
  --exclude='./.env' \
  --exclude='./.env.*' \
  --exclude='auth.json' \
  --exclude='*/auth.json' \
  --exclude='*.key' \
  --exclude='*.pem' \
  -cf "$worker_archive" .
tar -C "$worker_checkout" -xf "$worker_archive"
rm -f "$worker_archive"

echo "Running repository setup and gates in $local_reference"
"$DOCKER" run --rm --pull=never \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --mount "type=bind,src=$worker_checkout,dst=/work" \
  --mount "type=bind,src=$git_projection,dst=/git,readonly" \
  --workdir /work \
  "$local_reference" /usr/bin/env -i \
    HOME=/home/factory \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TERM=xterm-256color \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    FACTORY_RUN_ID=worker-build-verification \
    GIT_DIR=/git \
    GIT_WORK_TREE=/work \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_SYSTEM=/dev/null \
    GIT_CONFIG_COUNT=3 \
    GIT_CONFIG_KEY_0=remote.origin.url \
    GIT_CONFIG_VALUE_0=disabled://factory \
    GIT_CONFIG_KEY_1=remote.origin.pushurl \
    GIT_CONFIG_VALUE_1=disabled://factory \
    GIT_CONFIG_KEY_2=credential.helper \
    GIT_CONFIG_VALUE_2= \
    GIT_TERMINAL_PROMPT=0 \
    GIT_ASKPASS=/bin/false \
    SSH_ASKPASS=/bin/false \
    FACTORY_INVOCATION_DIR=/invocation \
    FACTORY_RESULT_DIR=/results \
    FACTORY_REPORT_COMMAND=/usr/local/bin/factory-report \
    /bin/sh -c '
    scripts/worker-go.sh mod download &&
    test -z "$(gofmt -l .)" &&
    scripts/worker-go.sh vet ./... &&
    scripts/worker-go.sh test ./... &&
    scripts/worker-go.sh build -o /tmp/factory ./cmd/factory
  '

cat <<EOF

Worker image built and verified locally.
worker_build:
  image: $WORKER_IMAGE
  digest: $local_image_id
  definition: worker/Dockerfile
EOF

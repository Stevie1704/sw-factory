#!/bin/sh

# Build and locally verify the versioned worker image chain, then print the
# immutable digest snippet that belongs in the repository factory.yaml.
#
# The chain is four worker base image variants from worker/base.Dockerfile,
# which share the minimal runtime layers, and the repository Go project image
# from worker/Dockerfile on the combined variant:
#
#   WORKER_BASE_IMAGE                    combined: Codex, Claude Code, Copilot
#   <WORKER_BASE_IMAGE name>-codex:<tag>    Codex only
#   <WORKER_BASE_IMAGE name>-claude:<tag>   Claude Code only
#   <WORKER_BASE_IMAGE name>-copilot:<tag>  GitHub Copilot CLI only
#   WORKER_IMAGE:WORKER_TAG              combined variant plus skills and Go
set -eu

DOCKER="${DOCKER:-docker}"
WORKER_BASE_IMAGE="${WORKER_BASE_IMAGE:-ghcr.io/stevie1704/sw-factory-base:v1}"
WORKER_IMAGE="${WORKER_IMAGE:-ghcr.io/stevie1704/sw-factory-worker}"
WORKER_TAG="${WORKER_TAG:-v1}"
NPM_VERSION="${NPM_VERSION:-12.2.2}"
CLAUDE_VERSION="${CLAUDE_VERSION:-2.1.325}"
CODEX_VERSION="${CODEX_VERSION:-0.148.0}"
WORKER_PLATFORM="${WORKER_PLATFORM:-}"
# A release build names its version; every other base is a development base.
FACTORY_RELEASE="${FACTORY_RELEASE:-development}"
WORKER_REFERENCE="${WORKER_IMAGE}:${WORKER_TAG}"

# The go.mod toolchain line is the only approved Go release for the shipped
# helpers and the worker toolchain. A different GO_VERSION is refused rather
# than silently producing an unapproved image.
approved_go="$(sed -n 's/^toolchain go//p' go.mod)"
if [ -z "$approved_go" ]; then
  echo "go.mod has no toolchain line naming the approved Go release" >&2
  exit 1
fi
if [ -n "${GO_VERSION:-}" ] && [ "$GO_VERSION" != "$approved_go" ]; then
  echo "GO_VERSION=$GO_VERSION differs from the approved go.mod toolchain go$approved_go." >&2
  echo "Unset GO_VERSION, or change the toolchain line in go.mod and the worker Dockerfile defaults together." >&2
  exit 1
fi
GO_VERSION="$approved_go"

# The compatibility record names the only proved Copilot CLI release. A
# different COPILOT_VERSION is refused, because the image check below compares
# the installed CLI with this value and would otherwise accept any release.
proved_copilot_manifest=docs/spikes/copilot-cli-1.0.98/manifest.json
proved_copilot="$(sed -n 's/^ *"version": "\([^"]*\)".*/\1/p' "$proved_copilot_manifest" | head -n 1)"
if [ -z "$proved_copilot" ]; then
  echo "$proved_copilot_manifest names no Copilot CLI release" >&2
  exit 1
fi
if [ -n "${COPILOT_VERSION:-}" ] && [ "$COPILOT_VERSION" != "$proved_copilot" ]; then
  echo "COPILOT_VERSION=$COPILOT_VERSION differs from the proved Copilot CLI $proved_copilot ($proved_copilot_manifest)." >&2
  echo "Unset COPILOT_VERSION, or prove the new release and update its compatibility record first." >&2
  exit 1
fi
COPILOT_VERSION="$proved_copilot"

# The variant references derive from the combined one, so it must carry a tag.
case "${WORKER_BASE_IMAGE##*/}" in
  *:*) ;;
  *)
    echo "WORKER_BASE_IMAGE=$WORKER_BASE_IMAGE must name a tag, such as ghcr.io/stevie1704/sw-factory-base:v1" >&2
    exit 2
    ;;
esac

platform_args=""
if [ -n "$WORKER_PLATFORM" ]; then
  platform_args="--platform=$WORKER_PLATFORM"
fi

# base_reference prints the image reference of one worker base variant: the
# combined image for all, and IMAGE-HARNESS:TAG for a single harness.
base_reference() {
  if [ "$1" = all ]; then
    printf '%s\n' "$WORKER_BASE_IMAGE"
  else
    printf '%s-%s:%s\n' "${WORKER_BASE_IMAGE%:*}" "$1" "${WORKER_BASE_IMAGE##*:}"
  fi
}

# variant_harnesses prints the harnesses one variant includes, as its label
# names them.
variant_harnesses() {
  if [ "$1" = all ]; then
    echo "claude codex copilot"
  else
    echo "$1"
  fi
}

VARIANTS="all codex claude copilot"
# --pull refreshes the moving Go, Node, and Debian base tags on the first
# build, so a rebuild picks up their published security updates instead of a
# stale local copy. The later variants reuse that build's stages.
pull=--pull
for variant in $VARIANTS; do
  reference="$(base_reference "$variant")"
  echo "Building factory base image $reference (harnesses: $(variant_harnesses "$variant")) with Go $GO_VERSION"
  # shellcheck disable=SC2086
  "$DOCKER" build $platform_args $pull \
    --build-arg "GO_VERSION=$GO_VERSION" \
    --build-arg "NPM_VERSION=$NPM_VERSION" \
    --build-arg "CLAUDE_VERSION=$CLAUDE_VERSION" \
    --build-arg "CODEX_VERSION=$CODEX_VERSION" \
    --build-arg "COPILOT_VERSION=$COPILOT_VERSION" \
    --build-arg "FACTORY_BASE_VERSION=1" \
    --build-arg "FACTORY_RELEASE=$FACTORY_RELEASE" \
    --file worker/base.Dockerfile \
    --target "harness-$variant" \
    --tag "$reference" \
    .
  pull=--pull=false
done

# Each variant must carry exactly its harnesses at their pinned releases, the
# release label, the worker contract, and no Node, npm, Yarn, Go, or gh
# installation. The checks run as the worker user without network.
for variant in $VARIANTS; do
  reference="$(base_reference "$variant")"
  harnesses="$(variant_harnesses "$variant")"
  echo "Verifying factory base image $reference"
  labels="$("$DOCKER" image inspect "$reference" --format '{{index .Config.Labels "io.github.stevie1704.sw-factory.harnesses"}}|{{index .Config.Labels "io.github.stevie1704.sw-factory.release"}}')"
  if [ "$labels" != "$harnesses|$FACTORY_RELEASE" ]; then
    echo "$reference carries the labels '$labels', want harnesses '$harnesses' and release '$FACTORY_RELEASE'" >&2
    exit 1
  fi
  "$DOCKER" run --rm --pull=never --network none \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --env "HARNESSES=$harnesses" \
    --env "CLAUDE_LINE=$CLAUDE_VERSION (Claude Code)" \
    --env "CODEX_LINE=codex-cli $CODEX_VERSION" \
    --env "COPILOT_LINE=GitHub Copilot CLI $COPILOT_VERSION." \
    "$reference" /bin/sh -c '
    set -eu
    test "$(id -u)" = 10001
    test "$HOME" = /home/factory
    test "$PATH" = /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
    test -d /work && test -d /git && test -d /cache && test -d /invocation && test -d /results
    test ! -w /run/factory-auth
    test -x /usr/local/bin/factory-report
    test -x /usr/local/bin/factory-worker-headless
    command -v git >/dev/null
    for absent in node nodejs npm npx yarn yarnpkg corepack go gofmt gh; do
      if command -v "$absent" >/dev/null; then
        echo "the base image carries $absent" >&2
        exit 1
      fi
    done
    test ! -e /usr/local/lib/node_modules && test ! -e /usr/local/go
    if ls -d /opt/yarn-* >/dev/null 2>&1; then
      echo "the base image carries Yarn" >&2
      exit 1
    fi
    installed=""
    for harness in claude codex copilot; do
      case " $HARNESSES " in
        *" $harness "*)
          case "$harness" in
            claude) test "$(claude --version | head -n 1)" = "$CLAUDE_LINE"; installed="$installed claude-code" ;;
            codex) test "$(codex --version | head -n 1)" = "$CODEX_LINE"; installed="$installed codex" ;;
            copilot)
              # The Copilot CLI starts only through its launcher.
              test "$(copilot --version | head -n 1)" = "$COPILOT_LINE"
              test -x /usr/local/bin/factory-copilot
              test -r /usr/local/share/factory-copilot/settings.json
              installed="$installed copilot"
              ;;
          esac
          ;;
        *)
          if command -v "$harness" >/dev/null; then
            echo "the base image carries the excluded harness $harness" >&2
            exit 1
          fi
          if [ "$harness" = copilot ]; then test ! -e /usr/local/bin/factory-copilot; fi
          ;;
      esac
    done
    test "$(ls /opt/factory/harnesses | sort | tr "\n" " ")" = "$(printf "%s\n" $installed | sort | tr "\n" " ")"
  '
done

echo "Building repository Go worker image $WORKER_REFERENCE"
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
  --env "APPROVED_GO=go$GO_VERSION" \
  --env "APPROVED_COPILOT=$COPILOT_VERSION" \
  "$local_reference" /bin/sh -c '
  set -eu
  test "$(id -u)" = 10001
  test "$HOME" = /home/factory
  test -d /work && test -d /git && test -d /cache && test -d /invocation && test -d /results
  test ! -w /run/factory-auth
  test -x /usr/local/bin/factory-report
  test -x /usr/local/bin/factory-worker-headless
  command -v codex >/dev/null
  command -v claude >/dev/null
  # The Copilot CLI is the proved release, starts only through its launcher,
  # and has no gh login to fall back to.
  test -x /usr/local/bin/factory-copilot
  test -r /usr/local/share/factory-copilot/settings.json
  test "$(copilot --version | head -n 1)" = "GitHub Copilot CLI $APPROVED_COPILOT."
  if command -v gh >/dev/null; then exit 1; fi
  command -v git >/dev/null
  command -v go >/dev/null
  command -v gofmt >/dev/null
  test "$PATH" = /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
  # Build metadata of every shipped helper and the worker toolchain must name
  # the approved Go release.
  test "$(go env GOVERSION)" = "$APPROVED_GO"
  for helper in /usr/local/bin/factory-report /usr/local/bin/factory-worker-headless /usr/local/go/bin/go; do
    test "$(go version "$helper")" = "$helper: $APPROVED_GO"
  done
'

repository_root="$(pwd -P)"
temporary_root="$(mktemp -d "$repository_root/.worker-build.XXXXXX")"
worker_checkout="$temporary_root/checkout"
git_projection="$temporary_root/git"
worker_archive="$temporary_root/checkout.tar"
mkdir "$worker_checkout" "$git_projection"

# Remove temporary sanitized worker inputs and the temporary names of the
# variant verification images after the build exits.
cleanup_worker_inputs() {
  for variant in codex claude copilot; do
    "$DOCKER" image rm "$WORKER_IMAGE-verify-$variant:$WORKER_TAG" >/dev/null 2>&1 || true
  done
  rm -rf "$temporary_root"
}
trap cleanup_worker_inputs EXIT HUP INT TERM

# Recreate the checkout without Git metadata, harness state, or host secrets.
# The production adapter supplies the same shape through its Git projection.
tar -C "$repository_root" \
  --exclude='./.git' \
  --exclude='./.factory-worktrees' \
  --exclude='./.worker-build.*' \
  --exclude='./.worker-scan.*' \
  --exclude='./.scan-reports' \
  --exclude='./.headless-verify.*' \
  --exclude='./.serena' \
  --exclude='./.ua' \
  --exclude='./spike' \
  --exclude='*/.run' \
  --exclude='*/.codex' \
  --exclude='*/.claude' \
  --exclude='*/.copilot' \
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

echo "Running real headless worker lifecycle verification"
scripts/verify-headless-worker.sh "$local_reference"

# Each single-harness variant gets the same verification as the repository
# image, on the skill layer of worker/Dockerfile without the Go toolchain. The
# skill image is a disposable verification artifact and is removed again.
for variant in codex claude copilot; do
  reference="$(base_reference "$variant")"
  echo "Running real headless worker lifecycle verification for $reference"
  # shellcheck disable=SC2086
  "$DOCKER" build $platform_args --pull=false --quiet \
    --build-arg "FACTORY_BASE_IMAGE=$reference" \
    --build-arg "GO_VERSION=$GO_VERSION" \
    --file worker/Dockerfile \
    --target worker-skills \
    --iidfile "$temporary_root/skills-$variant.iid" \
    worker >/dev/null
  # The real-worker checks address an image as name@digest, so the skill image
  # gets a temporary name.
  skills_id="$(cat "$temporary_root/skills-$variant.iid")"
  skills_name="$WORKER_IMAGE-verify-$variant"
  "$DOCKER" tag "$skills_id" "$skills_name:$WORKER_TAG"
  scripts/verify-headless-worker.sh "$skills_name@$skills_id"
done

base_images=""
for variant in $VARIANTS; do
  reference="$(base_reference "$variant")"
  base_images="$base_images
  $variant: $reference ($("$DOCKER" image inspect "$reference" --format '{{.Id}}'))"
done

cat <<EOF

Worker base images built and verified locally:$base_images

Worker image built and verified locally.
worker_build:
  image: $WORKER_IMAGE
  digest: $local_image_id
  definition: worker/Dockerfile
EOF

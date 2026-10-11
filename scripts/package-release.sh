#!/bin/sh

# Package one Factory release: a factory binary per supported host platform,
# each linked with the release version and the digest-pinned worker base image,
# in its own archive, plus release.json and checksums.txt. The binary embeds
# every onboarding resource, so the archive needs no Factory checkout or Go.
#
# Usage: VERSION=vX.Y.Z WORKER_BASE_IMAGE=image@sha256:DIGEST package-release.sh
# PLATFORMS narrows the host platforms, for example to the build host in
# tests. DIST_DIR (default dist) receives the archives; the unpacked staging
# directories stay in DIST_DIR/build for the binary scan.
set -eu

GO="${GO:-go}"
REPOSITORY_ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)"
# The initial support matrix. docs/installation.md documents it, and
# release_test.go checks that release.json carries it.
SUPPORTED_HOST_PLATFORMS="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"
SUPPORTED_WORKER_PLATFORMS="linux/amd64 linux/arm64"
PLATFORMS="${PLATFORMS:-$SUPPORTED_HOST_PLATFORMS}"
DIST_DIR="${DIST_DIR:-$REPOSITORY_ROOT/dist}"
VERSION="${VERSION:-}"
WORKER_BASE_IMAGE="${WORKER_BASE_IMAGE:-}"

if ! printf '%s\n' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "VERSION must be a release version such as v1.2.3, got '$VERSION'" >&2
  exit 2
fi
if ! printf '%s\n' "$WORKER_BASE_IMAGE" | grep -Eq '^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$'; then
  echo "WORKER_BASE_IMAGE must be pinned as image@sha256:DIGEST, got '$WORKER_BASE_IMAGE'" >&2
  exit 2
fi
for platform in $PLATFORMS; do
  case " $SUPPORTED_HOST_PLATFORMS " in
    *" $platform "*) ;;
    *)
      echo "$platform is not a supported host platform; supported: $SUPPORTED_HOST_PLATFORMS" >&2
      exit 2
      ;;
  esac
done

revision="$(git -C "$REPOSITORY_ROOT" rev-parse HEAD)"
ldflags="-s -w -X 'github.com/Stevie1704/sw-factory/internal/version.release=$VERSION' -X 'github.com/Stevie1704/sw-factory/internal/version.workerBaseImage=$WORKER_BASE_IMAGE'"

# sha256_line prints one checksums.txt line for a file in the current directory.
sha256_line() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1"
  else
    shasum -a 256 "$1"
  fi
}

# json_list prints the words of its argument as a JSON array of strings.
json_list() {
  printf '%s\n' "$1" | awk '{ printf "["; for (i = 1; i <= NF; i++) printf "%s\"%s\"", (i > 1 ? ", " : ""), $i; printf "]" }'
}

rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR/build"

archives=""
for platform in $PLATFORMS; do
  os="${platform%/*}"
  arch="${platform#*/}"
  name="factory_${VERSION}_${os}_${arch}"
  stage="$DIST_DIR/build/$name"
  mkdir -p "$stage/bin"
  echo "Building $name"
  (cd "$REPOSITORY_ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    "$GO" build -trimpath -ldflags "$ldflags" -o "$stage/bin/factory" ./cmd/factory)
  cp "$REPOSITORY_ROOT/docs/installation.md" "$stage/README.md"
  tar -C "$DIST_DIR/build" -czf "$DIST_DIR/$name.tar.gz" "$name"
  archives="$archives $name.tar.gz"
done

cat > "$DIST_DIR/release.json" <<EOF
{
  "schema_version": 1,
  "version": "$VERSION",
  "revision": "$revision",
  "worker_base_image": "$WORKER_BASE_IMAGE",
  "host_platforms": $(json_list "$SUPPORTED_HOST_PLATFORMS"),
  "worker_platforms": $(json_list "$SUPPORTED_WORKER_PLATFORMS")
}
EOF

(
  cd "$DIST_DIR"
  for file in $archives release.json; do
    sha256_line "$file"
  done
) > "$DIST_DIR/checksums.txt"

echo "Release $VERSION packaged in $DIST_DIR"

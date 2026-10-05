#!/bin/sh

# Scan built Go artifacts with the pinned govulncheck and evaluate each report
# against security/vulnerability-suppressions.yaml.
#
# Binary mode reads the Go release from each artifact's build metadata, so the
# newer toolchain that runs the scanner cannot hide an older deployed release;
# every artifact must also carry the approved go.mod toolchain. --source also
# scans the module source against that same approved release.
#
# Usage: scan-go-artifacts.sh [--source] [BINARY...]
# Exit status: 0 clean, 1 actionable findings or an unapproved Go release,
# 2 a scanner could not produce a result. Every setup step exits 2, so an
# infrastructure failure is never reported as findings.
set -eu

GOVULNCHECK_VERSION=v1.8.0
REPOSITORY_ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)"
SCAN_REPORT_DIR="${SCAN_REPORT_DIR:-$REPOSITORY_ROOT/.scan-reports}"
SUPPRESSIONS="$REPOSITORY_ROOT/security/vulnerability-suppressions.yaml"

approved_go="$(sed -n 's/^toolchain //p' "$REPOSITORY_ROOT/go.mod")"
if [ -z "$approved_go" ]; then
  echo "go.mod has no toolchain line naming the approved Go release" >&2
  exit 2
fi
# The scanner, the evaluator, and the source scan run on the approved release.
export GOTOOLCHAIN="$approved_go"

scan_source=false
if [ "${1:-}" = "--source" ]; then
  scan_source=true
  shift
fi

tools="$(mktemp -d)" || exit 2
# Remove the scanner build and raw reports when the scan exits.
cleanup_scan_tools() {
  rm -rf "$tools"
}
trap cleanup_scan_tools EXIT HUP INT TERM

mkdir -p "$SCAN_REPORT_DIR" || exit 2
metadata="$SCAN_REPORT_DIR/scans.jsonl"

if ! GOBIN="$tools" go install "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" ||
  ! (cd "$REPOSITORY_ROOT" && go build -o "$tools/vulnscan" ./tools/vulnscan); then
  echo "scanner setup failed; no artifact was scanned" >&2
  exit 2
fi

overall=0
# record_status keeps the most severe exit status: a scanner failure (2)
# outranks findings (1), which outrank a clean scan (0).
record_status() {
  if [ "$1" -gt "$overall" ]; then
    overall="$1"
  fi
}

# sha256_of prints the SHA-256 digest of one file.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# evaluate_report applies the suppression policy to one govulncheck report.
evaluate_report() {
  if "$tools/vulnscan" -scanner govulncheck -artifact "$1" -identity "$2" \
    -expect-go "$approved_go" -suppressions "$SUPPRESSIONS" -metadata "$metadata" <"$3"; then
    record_status 0
  else
    record_status "$?"
  fi
}

if [ "$scan_source" = true ]; then
  revision="$(git -C "$REPOSITORY_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
  if [ -n "$(git -C "$REPOSITORY_ROOT" status --porcelain 2>/dev/null)" ]; then
    revision="$revision-dirty"
  fi
  if (cd "$REPOSITORY_ROOT" && "$tools/govulncheck" -format json ./...) >"$tools/source.json"; then
    evaluate_report module-source "git:$revision" "$tools/source.json"
  else
    echo "govulncheck could not scan the module source" >&2
    record_status 2
  fi
fi

for binary in "$@"; do
  name="$(basename "$binary")"
  if [ ! -f "$binary" ]; then
    echo "govulncheck could not scan $binary: no such file" >&2
    record_status 2
    continue
  fi
  if "$tools/govulncheck" -mode binary -format json "$binary" >"$tools/$name.json"; then
    evaluate_report "$name" "sha256:$(sha256_of "$binary")" "$tools/$name.json"
  else
    echo "govulncheck could not scan $binary" >&2
    record_status 2
  fi
done

echo "Scan metadata appended to $metadata"
exit "$overall"

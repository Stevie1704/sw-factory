# Build toolchain and vulnerability scanning

This document owns three contracts: the approved Go toolchain for every
shipped binary, the vulnerability scans of the artifacts that are actually
deployed, and the procedure to rebuild, scan, publish, and repin the Worker
image. [Configuration](configuration.md#worker-image-build-and-digest-pinning)
owns the Worker image layout and the digest format.

## Approved Go toolchain

The `toolchain` line in [go.mod](../go.mod) names the one approved Go release.
It was set to `go1.27.2` on 2026-10-09. Then, `go1.27.2` and `go1.26.9` were
the two supported patched releases. The `go` line remains the language
minimum. It does not select a build toolchain.

Every build path selects the approved release, or it stops with a diagnosis:

| Build path | How it selects the release | What refuses another release |
| --- | --- | --- |
| `make build`, `make install`, `make test`, `make test-race` | The Makefile exports `GOTOOLCHAIN` as the go.mod toolchain line. Go downloads that release when the Go on `PATH` is different. This also applies when you set `GO=/path/to/go`. | `make toolchain-check` compares `go env GOVERSION` with go.mod and stops. Each of these targets runs it first. |
| CI | `actions/setup-go` reads `go-version-file: go.mod`. The Make targets then select and check the release again. | `make toolchain-check` |
| Worker base image (`factory-report`, `factory-worker-headless`) | `scripts/build-worker.sh` reads the release from go.mod and passes it as `GO_VERSION`. | The build script refuses a `GO_VERSION` variable that differs from go.mod. After the build, it compares the build metadata of each helper with go.mod. |
| Worker toolchain (`/usr/local/go`) | The same `GO_VERSION` build argument | The same in-image check runs `go env GOVERSION` and `go version /usr/local/go/bin/go`. |
| Plain `docker build` | The `GO_VERSION` defaults in both Dockerfiles | `worker/toolchain_contract_test.go` fails when a default differs from go.mod. |

To approve a new release, change the toolchain line with
`go mod edit -toolchain=go1.X.Y`. Then change `ARG GO_VERSION` in
`worker/base.Dockerfile` and `worker/Dockerfile`, run `make check`, and follow
the Worker procedure below.

Do not set `GOTOOLCHAIN` on the make command line. Do not block
`golang.org/toolchain` in `GOPROXY`. If you do, Go cannot run the approved
release, and `make toolchain-check` stops the build.

## Scanners

All scanners are pinned to exact versions:

| Scanner | Pin | Scans |
| --- | --- | --- |
| govulncheck | `golang.org/x/vuln/cmd/govulncheck@v1.8.0`, verified by the Go checksum database | Module source, and the build metadata and symbols of each Go binary |
| grype | `anchore/grype:v0.120.0` by its image index digest | OS packages, npm packages (including both harnesses and npm), and Go modules and toolchains in the final Worker image |

The pins are in `scripts/scan-go-artifacts.sh` and
`scripts/scan-worker-image.sh`. Change them in a reviewed commit only.

| Command | Scans |
| --- | --- |
| `make vuln-check` | The module source, scanned against the approved release, and `bin/factory`, `bin/factory-report`, and `bin/factory-worker-headless` |
| `make worker-scan` | The pinned `factory.yaml` Worker image. Set `WORKER_REFERENCE=image@digest` to scan a different local image. |

`make worker-scan` never pulls. It scans the image that is already available
locally. It copies `factory-report`, `factory-worker-headless`, and
`/usr/local/go/bin/go` out of the image and scans them with govulncheck. Then
it scans the complete image with grype.

The binary scans read the Go release from the build metadata of each artifact.
The scanner itself runs on the approved release, but this does not change
the result. A newer scanner toolchain therefore cannot hide an older deployed
release. Each scanned Go artifact, and the source scan, must report the
approved release. If not, the result is `unapproved_go`.

### Exit status

The command `go run ./tools/vulnscan` evaluates each report. The scan scripts
return the most severe status:

| Status | Meaning |
| --- | --- |
| `0` | Each artifact is clean. |
| `1` | An artifact has actionable findings, or it was built with a Go release that is not approved. |
| `2` | A scanner could not produce a result. Examples: a failed download, missing or malformed output, a missing artifact, or an invalid suppression file. This status is never a clean result. |

### Actionable findings

| Scanner | Actionable when | Informational otherwise |
| --- | --- | --- |
| govulncheck | A vulnerable symbol is present in the binary, or is reachable from the source. | Matches only at module or package level |
| grype | Severity is High or Critical, and the ecosystem has released a fix (`fix.state: fixed`). | Lower severities, and `not-fixed`, `wont-fix`, or unknown fix states |

An actionable finding blocks CI and `make worker-publish`. Resolve it in this
order:

1. Upgrade the affected component. For Go, approve a newer release. For the
   image, rebuild it: `make worker-build` pulls fresh base images, and you
   can raise a pinned package version.
2. If no upgrade is possible yet, or the finding does not apply, add a
   suppression.

### Suppressions

[security/vulnerability-suppressions.yaml](../security/vulnerability-suppressions.yaml)
is the only place to accept a finding. Each entry names one advisory for one
package of one artifact. Each entry also needs a reason, an owner, and a
`review_by` date. The loader refuses an incomplete entry, an advisory that is
not a GO, CVE, or GHSA identifier, and a package with a wildcard. Thus you
cannot suppress a full ecosystem or all packages of an artifact.

After its `review_by` date, a suppression stops working. The finding then
becomes actionable again, and the report shows the expired entry and its
owner. If a suppression no longer matches a finding, the report shows it as
unused. Remove unused entries.

### Scan metadata

Each evaluation appends one JSON line to `.scan-reports/scans.jsonl`. CI also
adds these lines to the job summary. A record contains no source content. It
contains this data:

- the artifact name and identity: the SHA-256 of a binary, the image
  reference and image ID, or the Git revision for a source scan
- the Go release from the build metadata
- the scanner name, version, and mode, and the vulnerability database and its
  build time
- the result, the actionable and suppressed advisories with their packages
  and versions, and the number of informational findings

Together with the pinned scanner versions, these records let you repeat a
scan against the same artifact and database.

## Rebuild, scan, publish, and repin the Worker

1. `make worker-build`. This builds the base and the Worker with the approved
   release, and pulls fresh base images. Then it checks the toolchain build
   metadata in the image. It runs the repository gates inside the image, and
   it runs the offline headless lifecycle check for both harness adapters and
   the `factory-report` and `factory-worker-headless` protocol.
2. `make worker-publish`. This scans the local `v1` tag and stops on any
   status other than `0`. Then it pushes the tag, reads the registry manifest
   digest, pulls `image@digest`, and makes sure that it is the same image. It
   prints the `worker_build` block.
3. Copy the printed `digest` into `factory.yaml`.
4. `./scripts/smoke-skills.sh`. This runs real, paid harness calls in the new
   digest and records evidence for each harness in
   `worker/skill-smoke.json`. Do not copy a record of an earlier digest, and
   do not write a record by hand. Records of earlier digests stay in the
   file, but they do not apply to the new digest.
5. `factory doctor --config <host config>`. Startup diagnosis must pass
   against the new digest.
6. Commit `factory.yaml` and `worker/skill-smoke.json` together. CI then
   pulls the published digest, verifies it, and scans it again.

A local-only image uses Docker's local image ID as its digest. See
[Configuration](configuration.md#worker-image-build-and-digest-pinning). If
you publish the image, always pin the registry manifest digest.

## Active Runs

A Run freezes the Worker `image@digest` when it is claimed. Each later worker
start, resume, and repair uses that frozen digest. Repinning `factory.yaml`
changes only Runs that are claimed after the commit. A Run that is already
active keeps its old image, and it also keeps the harness versions and Go
release of that image. Do not remove an old image from the local Docker host
or from the registry while an active Run still uses it. Docker never pulls a
Worker image for a Run.

Only a new Run, claimed after you repin, uses the patched image.
`/factory retry` reopens a cancelled or failed Run with its frozen old image.

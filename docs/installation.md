# Install a Software Factory release

A release is the supported way to operate Software Factory. You do not need
Go or a Software Factory checkout. A source build is the development path; see
[Build from source](#build-from-source).

## Support matrix

| Component | Supported |
| --- | --- |
| Host (`factory`) | `darwin/arm64`, `darwin/amd64`, `linux/arm64`, `linux/amd64` |
| Worker (Docker server) | `linux/arm64`, `linux/amd64` |

Windows and all other platforms are not supported. The release does not
publish an archive for them, and the worker base image has no manifest for
them. `docker version --format '{{.Server.Os}}/{{.Server.Arch}}'` shows the
worker platform of a Docker daemon.

The Docker daemon must use the containerd image store. Factory refers to a
local worker image by its image ID, and only this store finds an image by it.
Docker Engine 29 and colima use it by default. `docker info --format
'{{.DriverStatus}}'` must show `io.containerd.snapshotter.v1`.

## Release artifacts

Each release `vX.Y.Z` publishes these artifacts on its GitHub release page:

| Artifact | Content |
| --- | --- |
| `factory_vX.Y.Z_<os>_<arch>.tar.gz` | `bin/factory` and this file as `README.md` |
| `checksums.txt` | The SHA-256 of each archive and of `release.json` |
| `release.json` | The version, the source revision, the worker base images, and the support matrix |

Each release publishes four worker base images for both worker platforms:

| Image | Harnesses | `factory version --json` field |
| --- | --- | --- |
| `ghcr.io/stevie1704/sw-factory-base:vX.Y.Z` | Codex, Claude Code, and GitHub Copilot CLI | `worker_base_image` |
| `ghcr.io/stevie1704/sw-factory-base-codex:vX.Y.Z` | Codex | `worker_base_images.codex` |
| `ghcr.io/stevie1704/sw-factory-base-claude:vX.Y.Z` | Claude Code | `worker_base_images.claude` |
| `ghcr.io/stevie1704/sw-factory-base-copilot:vX.Y.Z` | GitHub Copilot CLI | `worker_base_images.copilot` |

A release refers to each image only by digest, and `factory version --json`
prints those digests. Each image carries its pinned harness releases as native
executables, the Copilot launcher `factory-copilot` when it includes Copilot,
and the worker-only helpers `factory-report` and `factory-worker-headless`. No
image carries Node, npm, Yarn, or Go; a repository worker image adds the
toolchain that its gates need. [Configuration](https://github.com/Stevie1704/sw-factory/blob/main/docs/configuration.md#worker-base-image-variants)
describes how a repository selects an image. Copilot support is not
released yet: a repository can select it only after an operator records a live
Copilot skill smoke ([GitHub Copilot
CLI](https://github.com/Stevie1704/sw-factory/blob/main/docs/configuration.md#github-copilot-cli)). Because of this, the host
needs only `factory`.

The `factory` binary contains every resource that onboarding needs: the
repository initialization procedure and its reference documents, the operator
onboarding skill, the curated worker skills and their provenance, the worked worker definition, and the
smoke and headless verification scripts. They always match the binary, also
after you move it.

## Install

1. Download the archive for your host and `checksums.txt` into one directory.
2. Verify the archive:

   ```sh
   shasum -a 256 --ignore-missing -c checksums.txt   # macOS
   sha256sum --ignore-missing -c checksums.txt       # Linux
   ```

3. Extract it to any directory, also one with spaces in its path, and put
   its `bin` directory on `PATH`:

   ```sh
   tar -xzf factory_vX.Y.Z_<os>_<arch>.tar.gz
   export PATH="$PWD/factory_vX.Y.Z_<os>_<arch>/bin:$PATH"
   factory version
   ```

On macOS, a browser download can be quarantined. Run
`xattr -d com.apple.quarantine bin/factory` if Gatekeeper refuses the
binary.

## Prepare a repository

Run these commands from the Git checkout of the repository to prepare:

```sh
factory guide          # orientation, works offline
factory guide setup    # host prerequisites and the onboarding flow
factory onboard prepare --output "$HOME/.cache/sw-factory/onboarding-vX.Y.Z" --json
```

`factory guide` works offline and needs no host registration. `factory
onboard prepare` also works offline and needs no host registration. It writes
the release resources to the named directory and prints a manifest with the
exact `factory` executable, the worker base image, and the procedure path. The
agent that already works in the repository reads that procedure and follows
it. Image pulls, GitHub authentication, and paid skill smoke need network
access. Apply `factory guide authorization` before executing the procedure's
effect inventory; interactive login remains a human step.

The prepared directory belongs to you. It stays after the agent session, so
remove it when the onboarding is complete. Prepare refuses a directory that is
not empty, unless an earlier preparation of the same release completed it, and
it never overwrites a file. Use a new directory after an upgrade.

The resources include the optional `factory-onboarding` skill. Factory never
installs it. [Repository
initialization](https://github.com/Stevie1704/sw-factory/blob/main/docs/repository-initialization.md#with-the-agent-that-already-runs)
names its location for Claude Code and Codex, and the plain-instructions use
for other agents.

`factory onboard` without `prepare` starts a new interactive Claude Code
session (or Codex with `--harness codex`) for the same procedure. It prepares
the resources in a new directory below the user cache directory and removes
that directory when the session ends.

## Upgrade

Upgrade only an idle installation: finish or cancel every active run with
the old binary first. Then download and verify the new archive, but do not
put it on `PATH` yet. Its `factory` binary is the target. Run:

```sh
<target factory> guide maintenance
<target factory> upgrade plan --config <host config> --onboarding-plan <plan file> --json
```

The plan only reads. It names the blockers, the conditions of the migration
gate, the ordered operations with their effects, the final verification, and
the newest maintenance checkpoint of this installation and of the live store
schema when you pass `--checkpoints <directory>`. The checkpoint step is optional; the
`factory guide maintenance` topic describes it. Do the first pending operation, then plan again; after an
interruption, the plan tells which components are old, new, or incomplete.
`factory upgrade migrate-store --config <path> --json` is the only supported
store migration entry point. Every other command, `factory start` included,
refuses an older store schema and names it. The coordinator runs in the foreground; stop it with Ctrl-C or
`factory stop`, and start the target with `factory start` at the end.

A release coordinator accepts only a worker image built from the worker base
image of its own release. Until the rebuild, `factory doctor` reports
`worker.release.mismatch`. The plan reuses the onboarding plan for the
rebuild, the digest pin, the skill smoke, and the doctor. It repeats a paid
smoke only for a skill without a record for the new digest.

Recovery limits: the only supported restore is `factory checkpoint restore`
of a maintenance checkpoint of the same host, registration, and release; it
does not restore across an upgrade. The automatic
`<store>.bak-<timestamp>` snapshot and its `.json` provenance file that the
migration writes are provenance, not a restore point; never copy it over the store. After the migration, the old
binary refuses the store, so it is not a rollback. A database copy does not
rewind branches, pull requests, comments, labels, workers, or harness
sessions. [ADR 0032](https://github.com/Stevie1704/sw-factory/blob/main/docs/adr/0032-quiescent-maintenance-checkpoint-and-restore-boundary.md)
sets the checkpoint and restore boundary.

The worker base images of a release with base image variants carry no Node,
npm, or Yarn. An earlier base image was built on `node:22-bookworm-slim`. A
repository whose setup or gates call `node`, `npm`, or `npx` must therefore add
Node to its worker definition before the rebuild, as the Node example in
[Repository
initialization](https://github.com/Stevie1704/sw-factory/blob/main/docs/repository-initialization.md#step-2-write-the-worker-image-definition)
shows. Otherwise its gates fail in the rebuilt image. A repository whose roles
select only one harness can also move to that harness's variant
([Configuration](https://github.com/Stevie1704/sw-factory/blob/main/docs/configuration.md#worker-base-image-variants)).

## Build from source

The development path builds from a checkout with the approved Go toolchain:

```sh
make install        # factory, factory-report, and factory-worker-headless
make worker-build   # the local development worker base images and the worker
```

A development build names no worker base image. Give `factory onboard prepare`
or `factory onboard` the base images that `make worker-build` built, pinned by
their local image IDs:

```sh
base=ghcr.io/stevie1704/sw-factory-base
id() { docker image inspect "$1" --format '{{.Id}}'; }
factory onboard prepare --output "<directory>" --json \
  --worker-base-image "$base@$(id $base:v1)" \
  --worker-base-images "claude=$base-claude@$(id $base-claude:v1),codex=$base-codex@$(id $base-codex:v1),copilot=$base-copilot@$(id $base-copilot:v1)"
```

A development build does not check the release label of the worker image.

`make release VERSION=vX.Y.Z WORKER_BASE_IMAGE=<image>@sha256:<digest>
WORKER_BASE_IMAGES=claude=<image>@sha256:<digest>,codex=...,copilot=...`
packages the archives, `release.json`, and `checksums.txt` into `dist`. Then
it scans the packaged binaries. The [release
workflow](https://github.com/Stevie1704/sw-factory/blob/main/.github/workflows/release.yml)
runs the complete release. [Build toolchain and vulnerability
scanning](https://github.com/Stevie1704/sw-factory/blob/main/docs/supply-chain.md#release-artifacts)
describes its gates. This file is also the `README.md` of each archive, so its
links are absolute.

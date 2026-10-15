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
| `release.json` | The version, the source revision, the worker base image, and the support matrix |

The worker base image is `ghcr.io/stevie1704/sw-factory-base:vX.Y.Z`. A release
refers to it only by digest. `factory version --json` prints that digest as
`worker_base_image`. The image is published for both worker platforms. It
carries the pinned Codex and Claude Code releases and the worker-only helpers
`factory-report` and `factory-worker-headless`. Because of this, the host
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

Install the new archive in place of the old one. A release coordinator
accepts only a worker image built from the worker base image of its own
release. After an upgrade, `factory doctor` reports `worker.release.mismatch`
until you rebuild the repository worker image from the new base image,
record its digest in `factory.yaml`, and record a new skill smoke. Prepare
the new release's resources with `factory onboard prepare`, or run `factory
onboard`, to do these steps with an agent.

## Build from source

The development path builds from a checkout with the approved Go toolchain:

```sh
make install        # factory, factory-report, and factory-worker-headless
make worker-build   # the local development worker base image and the worker
```

A development build names no worker base image. Give `factory onboard prepare`
or `factory onboard` the base image that `make worker-build` built, pinned by
its local image ID:

```sh
factory onboard prepare --output "<directory>" --json --worker-base-image \
  "ghcr.io/stevie1704/sw-factory-base@$(docker image inspect ghcr.io/stevie1704/sw-factory-base:v1 --format '{{.Id}}')"
```

A development build does not check the release label of the worker image.

`make release VERSION=vX.Y.Z WORKER_BASE_IMAGE=<image>@sha256:<digest>`
packages the archives, `release.json`, and `checksums.txt` into `dist`. Then
it scans the packaged binaries. The [release
workflow](https://github.com/Stevie1704/sw-factory/blob/main/.github/workflows/release.yml)
runs the complete release. [Build toolchain and vulnerability
scanning](https://github.com/Stevie1704/sw-factory/blob/main/docs/supply-chain.md#release-artifacts)
describes its gates. This file is also the `README.md` of each archive, so its
links are absolute.

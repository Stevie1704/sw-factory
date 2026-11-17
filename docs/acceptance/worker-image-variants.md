# Worker image size record

This record holds the measured sizes of the worker images before and after the
worker base image variants ([ADR 0031](../adr/0031-native-harness-base-image-variants.md),
issue #309). [Configuration](../configuration.md#worker-base-image-variants)
owns the variant contract.

## Method

All values come from one host and one platform, `linux/arm64`, on Docker
29.5.2 with the containerd image store, on 2026-11-17. `docker image ls --tree`
reports two values per image:

| Value | Meaning |
| --- | --- |
| Content size | The compressed layer blobs, as a registry stores and a pull transfers them |
| Disk usage | Content size plus the unpacked layers in the snapshotter |
| Unpacked layers | Disk usage minus content size |

The values are per image. Images share layers: the four variants share the
runtime layers, and the combined variant shares each harness layer with its
single-harness variant. The sum over several images is therefore larger than
the disk space they use together.

## Before

The images that `make worker-build` built from revision `6b09a62`:

| Image | Content size | Unpacked layers | Disk usage |
| --- | --- | --- | --- |
| `sw-factory-base:v1` (Node runtime, all harnesses) | 530 MB | 1.23 GB | 1.76 GB |
| `sw-factory-worker:v1` (base plus Go) | 598 MB | 1.52 GB | 2.12 GB |

## After

The images that `scripts/build-worker.sh` built from this change, with the
same harness releases (Codex `0.148.0`, Claude Code `2.1.325`, Copilot CLI
`1.0.98`) and Go `1.27.4`:

| Image | Content size | Unpacked layers | Disk usage |
| --- | --- | --- | --- |
| `sw-factory-base` (combined) | 474 MB | 1.05 GB | 1.52 GB |
| `sw-factory-base-codex` | 182 MB | 499 MB | 681 MB |
| `sw-factory-base-claude` | 239 MB | 592 MB | 831 MB |
| `sw-factory-base-copilot` | 187 MB | 410 MB | 597 MB |
| `sw-factory-worker` (Go project image on the combined base) | 542 MB | 1.34 GB | 1.88 GB |
| Go project image on `sw-factory-base-codex` | 250 MB | 790 MB | 1.04 GB |

`docker image ls --tree` rounds to three significant digits, so each unpacked
value can differ by up to 10 MB.

## Savings

| Comparison | Content size | Unpacked layers | Disk usage |
| --- | --- | --- | --- |
| Combined base against the old base | −56 MB (−11 %) | −0.18 GB (−15 %) | −0.24 GB (−14 %) |
| Codex base against the old base | −348 MB (−66 %) | −0.73 GB (−59 %) | −1.08 GB (−61 %) |
| Claude Code base against the old base | −291 MB (−55 %) | −0.64 GB (−52 %) | −0.93 GB (−53 %) |
| Copilot base against the old base | −343 MB (−65 %) | −0.82 GB (−67 %) | −1.16 GB (−66 %) |
| Go worker on the combined base against the old worker | −56 MB (−9 %) | −0.18 GB (−12 %) | −0.24 GB (−11 %) |
| Go worker on the Codex base against the old worker | −348 MB (−58 %) | −0.73 GB (−48 %) | −1.08 GB (−51 %) |

The combined image saves only the external Node runtime, npm, Yarn, and their
files. The harness executables stay the same size: Claude Code takes 365 MB
unpacked, Codex 271 MB, and the Copilot CLI 182 MB. Most of the saving comes
from a single-harness variant, which drops the other two harnesses. The Go
toolchain (291 MB unpacked) stays in the Go project image of this repository;
a repository that does not need Go builds its project image without it.

The shared runtime (`debian:bookworm-slim`, the worker packages, and the
helpers) takes about 227 MB unpacked. Git pulls in Perl, which accounts for
about 56 MB of it.

## Open acceptance items

These items need a registry push or paid harness credentials, so they follow
the merge:

- The release workflow has not published the variants yet. The first
  `vX.Y.Z` tag after this change publishes them and links their digests.
- `factory.yaml` still pins the earlier `sw-factory-worker` digest. Repin it to
  the published Go worker on the combined base with the procedure in [Build
  toolchain and vulnerability scanning](../supply-chain.md#rebuild-scan-publish-and-repin-the-worker).
- The offline checks above ran each real harness executable in each variant:
  Codex started and resumed a native thread, Claude Code loaded every curated
  skill and refused worktree hooks, and the Copilot CLI behind
  `factory-copilot` used only the projected token, the managed settings, and
  the curated skills. A paid skill smoke with real credentials
  (`scripts/smoke-skills.sh`) for each harness of the repinned image is still
  open.

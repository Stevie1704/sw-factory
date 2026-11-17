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

## Publication and repin

The release workflow published the four variants with release `v0.1.0-rc.4`
on 2026-11-17. It built, verified, and scanned each variant on `linux/amd64`
and `linux/arm64` before the push. `release.json`, `factory version --json`,
and the onboarding manifest of that release link these indexes:

| Variant | Published index |
| --- | --- |
| Combined (`worker_base_image`) | `ghcr.io/stevie1704/sw-factory-base@sha256:748984887a051149daff189411f7bb6a41d76b401b77da6567df7ed436a5e2b6` |
| Codex | `ghcr.io/stevie1704/sw-factory-base-codex@sha256:762dcba0d80eee6568065e1d05b8c80a68bbdba4e10f219c19e39df24a0702a4` |
| Claude Code | `ghcr.io/stevie1704/sw-factory-base-claude@sha256:59b9f57f70f4fee1e387abcdaa7df22cf96d9c4d9a761cbd7da2e2e09221af88` |
| Copilot | `ghcr.io/stevie1704/sw-factory-base-copilot@sha256:f22c15517fdc1c6d2ee41f2553fab669720d5eac99cfeb61d1ff622250f9a61e` |

`factory.yaml` pins the Go worker on the combined base,
`ghcr.io/stevie1704/sw-factory-worker@sha256:e69ce3cf2b7611d558f95bd9f4c3448a3e2c763dbeb861d87cc1010cbae98f2f`.
It was rebuilt, scanned, published, and repinned with the procedure in [Build
toolchain and vulnerability scanning](../supply-chain.md#rebuild-scan-publish-and-repin-the-worker):

- The grype and govulncheck scans of the worker are clean.
- `scripts/smoke-skills.sh` made real, paid Codex calls in that digest. Codex
  loaded every mandatory skill, and `worker/skill-smoke.json` records the
  result. Codex is the only harness that the roles of this repository can
  select.
- `factory doctor` is ready against that digest. The Codex executable, skill
  contract, and authentication checks pass.

The offline checks ran each real harness executable in each variant. Codex
started and resumed a native thread. Claude Code loaded every curated skill
and refused worktree hooks. The Copilot CLI behind `factory-copilot` used
only the projected token, the managed settings, and the curated skills.

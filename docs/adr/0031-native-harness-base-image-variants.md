---
status: accepted
---

# Native harness base image variants without language toolchains

The worker base image was built on `node:22-bookworm-slim` and installed all three harnesses with npm. The repository worker added a full Go toolchain. Every repository therefore pulled Node, npm, Yarn, and every harness, and Software Factory's own 2.12 GB worker carried them beside Go. All three harness packages ship a native executable per platform, so the external Node runtime served only the npm launchers.

The base definition now builds four variants from one shared runtime stage on `debian:bookworm-slim`: the combined image and one image per harness. npm runs only in build stages. They copy the complete platform package with the native executable, and the manifest and license file of each published package, to `/opt/factory/harnesses/` and link the command into `/usr/local/bin`. The final images carry no Node, npm, Yarn, or Go. A harnesses label names the harnesses of each variant. The combined image keeps the name `sw-factory-base` and the `worker_base_image` identity, so existing pins and the release check keep working. The single-harness variants are separate image names, `sw-factory-base-<harness>`. Each release links their digests as `worker_base_images` into the `factory` binary, `release.json`, and the onboarding manifest, so onboarding can select a variant offline. A project toolchain belongs to the repository's project image; `worker/Dockerfile` is the Go project image of Software Factory. Startup diagnosis probes only the harnesses a repository can select, so a single-harness image passes for a repository that selects only that harness, and a selected harness that the image lacks fails with an action that names its variant.

## Considered options

| Option | Result |
| --- | --- |
| Keep Node and delete npm caches | Rejected. The external runtime and npm stay in every image, and deleting files from an inherited layer does not shrink it. |
| Alpine (musl) runtime | Rejected. It would shrink the shared runtime further, but it changes the C library of the proved Copilot build, and the official toolchain images that repositories copy into their project image are built for glibc. |
| Distroless runtime | Rejected. Setup, gates, the launcher, and the headless helper need a shell, Git, and jq. |
| Variants as tag suffixes of one image | Rejected. The tags would mix release, variant, and platform, and the combined image would lose its stable name. |
| Variant digests only in `release.json` | Rejected. Onboarding would need the network to select a variant, or would always fall back to the combined image. |
| Probe every shipped harness at startup | Rejected. A single-harness image would always fail the probe of the absent harnesses. |

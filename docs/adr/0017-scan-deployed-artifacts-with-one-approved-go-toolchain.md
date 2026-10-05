---
status: accepted
---

# Scan deployed artifacts, built with one approved Go toolchain

The go.mod `go` line was only a language minimum. Local builds used the Go on `PATH`, CI used the `go` line, and the Worker build script defaulted to its own release. Thus the shipped coordinator was built with `go1.25.5`, and the Worker helpers and toolchain with `go1.25.0`. A source scan that switched to a newer toolchain did not find the vulnerable standard library symbols in these binaries.

The go.mod `toolchain` line therefore names the one approved patched release. Each build path selects that release, or it stops with a diagnosis. The Makefile exports `GOTOOLCHAIN`, CI reads go.mod, and the Worker build reads go.mod and refuses a different `GO_VERSION`. The Dockerfile defaults are checked against go.mod by a test. The release gate scans the built artifacts, not the source alone. govulncheck reads the Go release from the build metadata of each binary. Pinned grype scans the final Worker image. A report must show the approved release, or the result is a finding. A scanner that cannot produce a result returns a separate status, and that status never counts as clean. Suppressions apply to one advisory in one package of one artifact. Each needs a reason, an owner, and a review date.

## Considered options

Raising the `go` line to the patched release was rejected. It changes language and `GODEBUG` defaults for a supply-chain reason, and with `GOTOOLCHAIN=local` it still lets a newer, unreviewed release build. A source scan alone was rejected because it reports the scanning toolchain, not the deployed one. A severity-only image gate was rejected: most Debian findings have no fix, and a gate that always fails is soon ignored. The gate therefore counts only High and Critical findings that have a released fix.

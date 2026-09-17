---
status: accepted
---

# Repository configuration declares cache names; host configuration owns cache paths

A cache host path in checked-in `factory.yaml` let a repository commit point a writable bind mount at any host directory and made the file unusable by a second operator. Repository configuration therefore declares only the cache name and its container-side purpose, and the host registration maps each declared name to a directory that must resolve inside the operator's `cache_root`. Host configuration refuses a mapped directory outside the cache root when it loads. Startup diagnosis also blocks a declared cache with no host mapping, and a host mapping that no repository cache claims. Worker launch, gates, and check repair fail closed on an unmapped name. No layer defaults a path.

## Considered options

A repository-relative cache path was rejected because a cache must outlive the run worktree. Deriving the host directory from `cache_root` and the name alone was rejected because it removes the operator's explicit consent to a writable mount: a repository commit could then create one by adding a name.

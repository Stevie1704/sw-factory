---
status: accepted
---

# Repository configuration declares cache names; host configuration owns cache paths

A cache host path in checked-in `factory.yaml` let a repository commit point a writable bind mount at any host directory and made the file unusable by a second operator. Repository configuration therefore declares only the cache name and its container-side purpose, and the host registration maps each declared name to a directory that must resolve inside the operator's `cache_root`. A declared cache with no host mapping, a host mapping no repository cache claims, and a mapped directory outside the cache root are all blocking startup findings; worker launch, gates, and check repair fail closed on the same disagreement rather than defaulting a path.

## Considered options

A repository-relative cache path was rejected because a cache must outlive the run worktree. Deriving the host directory from `cache_root` and the name alone was rejected because it removes the operator's explicit consent to a writable mount: a repository commit could then create one by adding a name.

# Configuration and local operation

Issue #3 establishes two configuration documents and one local operational store.

The host configuration is created with `factory init`. Its path is selected by `--config`, then `FACTORY_CONFIG`, then the operating system's user configuration directory at `factory/config.yaml`. The repository registration is intentionally limited to one repository in version one.

```sh
factory init --config /Users/me/.config/factory/config.yaml
factory register \
  --config /Users/me/.config/factory/config.yaml \
  --repository /Users/me/src/project \
  --github-owner example \
  --github-repository project \
  --authorized-user alice \
  --operational-data /Users/me/.local/share/factory/factory.db
factory status --config /Users/me/.config/factory/config.yaml
```

`factory register` creates the SQLite store before it writes the registration. It does not contact GitHub, create labels, or write into the registered repository.

## Host configuration

The generated host file contains the repository path, GitHub identity, authorized maintainers, polling settings, cmux settings, the checked-in repository configuration path, and the operational-data path.

```yaml
schema_version: 1
repositories:
  - path: /Users/me/src/project
    github:
      owner: example
      repository: project
    authorized_users:
      - alice
    polling:
      interval: 30s
      backoff: 5m
    cmux:
      socket_path: ''
      control_workspace: factory-control
    operational_data_path: /Users/me/.local/share/factory/factory.db
    repository_config_path: /Users/me/src/project/factory.yaml
```

All paths persisted in a repository registration are absolute. The coordinator does not infer macOS-specific paths in its domain or deep modules; only the command's default host-config resolver uses the host operating system's standard user configuration directory.

## Checked-in repository configuration

The default checked-in file is `factory.yaml` at the repository root. It is parsed with strict field checking and validated before a run can be claimed. Unknown schema versions fail closed.

```yaml
schema_version: 1
target_branch: main
setup: go mod download
gates:
  - name: format
    command: gofmt -l .
    timeout: 30s
    blocking: true
    environment_policy: clean
  - name: test
    command: go test ./...
    timeout: 2m
    blocking: true
    depends_on: [format]
    environment_policy: clean
role_harness_defaults:
  test: codex
  implementation: codex
model_options:
  test: [gpt-5]
  implementation: [gpt-5]
timeouts:
  setup: 5m
  agent: 30m
  gate: 5m
  review: 10m
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: required
  allow_human_exemption: true
  allow_technical_exemption: true
allowed_overrides: [model, reasoning_effort]
caches:
  - name: go-build
    path: /tmp/factory-cache
    read_only: false
worker_build:
  image: ghcr.io/example/factory-worker
  digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  definition: worker/Dockerfile
base_synchronization:
  mode: before_ready
  branch: main
```

The validator checks the schema version, target branch, setup, ordered unique gates and earlier dependencies, matching role harness/model policies, positive durations, positive retry limits, test policy, supported unique overrides (`model`, `reasoning_effort`, or `harness`), caches, worker image, and base-synchronization mode. An empty `allowed_overrides` list is valid and means that issue-level overrides are disabled. Validation errors are typed and identify the offending field, including `schema_version` for an unsupported newer schema.

Issue #3 establishes and validates this repository-declared gate contract. Running setup and gates with baseline health, dependency skipping, timeouts, and checkpoint-keyed results is the gate-runner work in issue #9; this binary does not execute arbitrary repository commands yet. The commands declared by this repository's `factory.yaml` are run as part of the repository verification suite.

## Operational SQLite store

The operational store contains only current workflow state needed by later tickets. Registration and status both reject paths that resolve inside the repository checkout, including symlink aliases; its directory is private (`0700`) and the SQLite file is private (`0600`). A fresh store is initialized directly; an older supported schema is copied to a timestamped `.bak-*` file before its explicit migration runs. A newer or unversioned database refuses to open. There is no silent guessing or destructive migration.

Issue #25 will add separate content-free local evaluation summaries. Those summaries remain local and are not outbound telemetry.

The high-level `Factory` seam injects configuration, repository checking, and operational-store adapters. Foundation tests use a real temporary SQLite store and fake the external repository/configuration boundary; GitHub, Git/worktree, worker, terminal, harness, and clock adapters are introduced by the later workflow tickets that first need them.

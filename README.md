# Software Factory

Software Factory is a local, supervised coordinator for taking an authorized
GitHub issue through an isolated, AI-assisted change process and into a draft
pull request. It combines a frozen issue and repository policy with a
host-owned Git workspace, a pinned Docker worker, visible Codex or Claude Code
sessions, deterministic gates, structured reports, and restart-safe local
state.

The command-line coordinator is <code>factory</code>. It is intentionally an
operator tool, not a hosted service: GitHub remains the source of issue and
pull-request events, the coordinator runs on the operator's machine, and every
meaningful workflow boundary is visible and recoverable.

This README is the starting point for using the implementation in this
repository. The deeper contracts live in
[docs/configuration.md](docs/configuration.md),
[docs/agent-runtime.md](docs/agent-runtime.md), and
[docs/worker-runtime.md](docs/worker-runtime.md).

## Contents

- [What factory does](#what-factory-does)
- [Core concepts](#core-concepts)
- [Run lifecycle](#run-lifecycle)
  - [Workflow routes](#workflow-routes)
- [Prerequisites](#prerequisites)
- [Build and install](#build-and-install)
- [Configure a host](#configure-a-host)
- [Configure a repository](#configure-a-repository)
- [Run an issue from start to draft PR](#run-an-issue-from-start-to-draft-pr)
- [Structured agent reports](#structured-agent-reports)
- [GitHub commands and human intervention](#github-commands-and-human-intervention)
- [Recovery and authentication](#recovery-and-authentication)
- [Persistent polling](#persistent-polling)
  - [Unattended claim-to-draft-PR progression](#unattended-claim-to-draft-pr-progression)
  - [Unattended review, repair, and readiness](#unattended-review-repair-and-readiness)
  - [Human review as a repair trigger](#human-review-as-a-repair-trigger)
  - [Terminal outcomes and queue continuation](#terminal-outcomes-and-queue-continuation)
  - [States that intentionally pause progression](#states-that-intentionally-pause-progression)
- [Evaluation and cleanup](#evaluation-and-cleanup)
- [Security and data boundaries](#security-and-data-boundaries)
- [Command reference](#command-reference)
- [Development](#development)
- [Troubleshooting](#troubleshooting)
- [Further reading](#further-reading)

## What factory does

For one registered repository, factory coordinates the following boundary:

~~~text
authorized GitHub issue
        |
        | agent-ready label + explicit claim
        v
frozen specification packet
        |
        | isolated branch/worktree + baseline gates
        v
required: test -> implementation -> checkpoint gates -> draft PR -> review
             |          |                 |                  |          |
             |          |                 |                  +--> bounded repair -> gates/review
             |          |                 |                  |          |
             |          |                 |                  |          +--> ready
             |          |                 |                  +--> human review/merge
             |          |                 +--> bounded check repair loop
             |          +--> clarification, retry, or failure
             +--> verified red-test handoff or authorized exemption

advisory: implementation-owned red/green/refactor -> checkpoint gates -> ...
~~~

The coordinator owns workflow state, GitHub projections, worktrees, worker
identity, terminal surfaces, report validation, checkpoint commits, gates,
pushes, and draft pull requests. A harness is a visible proposal-maker. It
does not own workflow transitions, Git history, GitHub mutations, or the final
interpretation of terminal output.

Factory does not merge pull requests, silently alter repository policy, pull a
mutable worker image at run time, or delete remote branches during cleanup.

## Core concepts

The project uses a small vocabulary consistently across the CLI, the
operational store, GitHub comments, and the documentation.

| Term                     | Meaning                                                                                                                                                        |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Run**                  | One supervised execution for one issue. It owns the frozen packet, branch, worktree, invocations, checkpoints, gates, and pull request.                        |
| **Specification packet** | The immutable snapshot of the issue, resolved repository policy, and packet version used by an invocation. A clarification or refresh creates a new version.   |
| **Invocation**           | One visible role-agent execution against a run. It has a role, stage, harness, model, prompt version, worker identity, terminal surface, and result directory. |
| **Worker**               | The pinned Docker execution boundary. It contains the repository worktree and approved tools, but no host GitHub credentials or Git remote.                    |
| **Surface**              | A visible cmux terminal workspace or role surface used by an invocation.                                                                                       |
| **Checkpoint**           | An immutable commit used as a stage boundary. Test and implementation checkpoints are separate.                                                                |
| **Gate**                 | A deterministic repository command, such as formatting, vetting, testing, or building, run in the policy-defined environment.                                  |
| **Operational store**    | A private SQLite database that records run state, identities, effects, reports, gate results, and content-free evaluation summaries.                           |
| **Baseline**             | The pre-edit setup and gate result for the frozen packet. It proves what the repository looked like before agent edits.                                        |
| **Test objection cycle** | A bounded implementation-to-test dispute: implementation supplies a test claim and evidence, the original test session accepts or rejects it, and an accepted revision must pass independent red verification. Automation is pilot-gated and bounded by the repository's `retry_limits.test_revision` value. |
| **Recovery diagnosis**   | A read-only comparison of durable state against Git, GitHub, the worktree, worker, terminal, harness, and operational store.                                   |
| **Reconciliation**       | A deliberate restart pass that replays an exact pending effect or pauses for human inspection when external state is ambiguous.                                |

There is only one active non-terminal run per registered repository. Stage and
status are separate values. In required mode, <code>test/active</code> means the
independent test stage is running; in advisory mode a healthy baseline enters
<code>implementation/active</code> directly. In either mode,
<code>implementation/waiting_for_human</code> means implementation is paused for
an operator.

## Run lifecycle

The factory-owned workflow registry defines the roles and transitions. A
repository may select the harness and model policy for those roles, but it
cannot add arbitrary roles or redefine the transition graph in
<code>factory.yaml</code>.

| Stage                             | Role or owner                            | What happens                                                                                                                                                |
| --------------------------------- | ---------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| <code>claim</code>                | Coordinator                              | The issue, repository policy, target branch, run branch, worktree, and status projection are frozen.                                                        |
| <code>preflight</code>            | Coordinator                              | Setup and baseline gates run before an agent edits anything.                                                                                                |
| <code>test</code>                 | <code>test</code> role                   | The independent agent adds a focused test or test infrastructure change. The coordinator reruns the focused command and accepts only verified red evidence. Required mode and the contract-first routes use this stage. |
| <code>architecture</code>         | <code>architecture</code> role, optional | The agent writes a design document under the permitted architecture path. The <code>design-acceptance</code> route hands that design to the test role; otherwise the design goes to implementation.                     |
| <code>implementation</code>       | <code>implementation</code> role         | The agent edits production code and submits a structured implementation handoff.                                                                            |
| <code>check</code>                | Coordinator                              | The accepted implementation is checkpointed and every configured gate runs against that exact checkpoint.                                                   |
| <code>draft_pr</code>             | Coordinator                              | The host pushes the run branch before the gate statuses, then creates or updates one draft pull request after successful gates.                             |
| <code>review</code>               | <code>spec_review</code> + <code>standards_review</code> | Isolated reviewers concurrently inspect the same exact checkpoint and report role-scoped blocking findings or advisories.                         |
| <code>ready</code>                | Coordinator                              | The final target synchronization and review gates passed; Factory marks the pull request ready for a human to review and merge, but does not merge it.       |

The checked-in policy uses advisory mode: after a healthy baseline, the
implementation role owns the complete red/green/refactor loop. It may add or
revise focused behavioral tests and essential test infrastructure within its
permitted scope. There is no separate test invocation, handoff, checkpoint,
protected-test path, exemption, or test-stage dispute in advisory mode.
The checked-in policy also keeps automated implementation objections disabled;
the measured pilot must record `proceed` before that switch is enabled.

Repositories using required mode, and any run whose issue selected a workflow
route, enter the independent test stage. In required mode that stage can be
skipped only through an explicitly permitted human or technical exemption; a
selected route removes the human skip. The human exemption marker must be present in the frozen issue before
claim:

~~~text
<!-- factory-test-exemption: human | documentation-only change -->
~~~

The test policy must allow that exemption. In required mode, a test handoff never
owns production files. Once accepted, its test checkpoint and protected test
paths are carried into implementation, and an implementation report that changes
a protected test path is rejected. Instead, implementation can submit a
structured objection naming the test, disputed claim, and observable evidence.
Before pilot authorization the objection is preserved for human disposition; an
authorized run can resume the original test session for as many independently
verified revision attempts as the repository's
<code>retry_limits.test_revision</code> value allows.

### Workflow routes

Most changes take the fast path. An authorized issue author may instead select
one factory-owned contract-first route before claim, when independently
authored behavioral evidence is worth the extra invocation:

| Route                              | Sequence                                                                       | Choose it when                                                                                                                       |
| ---------------------------------- | ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| fast (no marker, advisory policy)  | implementation-owned TDD → gates → independent review                          | The default. The change is ordinary product work and one agent can own its own red/green/refactor loop.                              |
| <code>acceptance</code>            | acceptance test → implementation TDD → gates → independent review              | The interface is already stable and you want the behavioral contract written by an agent that cannot also change it.                 |
| <code>design-acceptance</code>     | architecture → acceptance test → implementation TDD → gates → independent review | The change introduces a new interface. Architecture establishes the boundary first, and the accepted design becomes the test contract. |

Select a route with exactly one marker in the issue body, before the issue is
claimed. Like the test-exemption marker, the route marker is matched literally
anywhere in the body, so write it exactly as shown, and do not quote an example
marker in an issue you intend to claim:

~~~text
<!-- factory-route: acceptance -->
~~~

~~~text
<!-- factory-route: design-acceptance -->
~~~

Route selection is explicit, never inferred. The coordinator does not derive a
route from changed files, issue prose, or model judgment. These rules hold:

- The frozen issue grammar accepts at most one route marker. A duplicate,
  unterminated, or undeclared marker rejects the claim with a typed policy code
  (<code>route_duplicate</code>, <code>route_invalid</code>) before any agent
  runs.
- A route that names a role the repository has not configured rejects the claim
  with <code>route_unavailable</code>. The <code>acceptance</code> route needs
  the <code>test</code> role; <code>design-acceptance</code> needs the
  <code>architecture</code> and <code>test</code> roles.
- The selected route is recorded in the specification packet at claim and is
  immutable for the run. A later issue edit, a <code>/factory refresh</code>,
  repository guidance, and agent reports cannot change it.
- An absent marker follows repository policy. Advisory policy takes the fast
  path; required policy keeps its mandatory independent test stage. Required
  policy cannot be downgraded through issue content.
- A selected route runs the independent test stage even under advisory policy,
  and it overrides a pre-authorized human test-exemption marker.
- The test role must prove acceptance through the highest practical observable
  interface and must not prescribe private implementation structure the frozen
  criteria do not require.
- Gates and the configured independent reviews keep their exact-checkpoint
  behavior on every route. Specification and documented-standards reviews use
  separate status contexts and do not share reviewer state.

The frozen route and current stage appear in <code>factory status</code>, in the
<code>factory agent</code> launch output, and in the single editable GitHub
status comment. No projection exposes agent transcripts.

Every stage also has an orthogonal status:

- <code>active</code>: work may proceed;
- <code>waiting_for_human</code>: clarification, review disposition, recovery,
  or another operator decision is required;
- <code>waiting_for_harness</code>: a retryable harness-capacity problem is
  being held;
- <code>failed</code>: the stage or run cannot proceed under the current policy;
- <code>cancelled</code>: the issue or pull request was closed without a merge;
- <code>complete</code>: the tracked pull request was merged.

## Prerequisites

Factory is designed for a macOS operator workflow with cmux, Docker, GitHub,
and a visible agent harness.

You need:

1. Go <code>1.25.x</code> or a compatible Go toolchain for this repository.
2. Git and a checkout of the repository that contains a valid, checked-in
   <code>factory.yaml</code>.
3. Docker with a working daemon. The worker image must be built locally and
   must match the configured image digest; the runtime never silently pulls a
   replacement image.
4. The GitHub CLI, <code>gh</code>, authenticated to an account that can read
   and update the configured repository, issues, labels, comments, commit
   statuses, and pull requests.
5. A running cmux session for visible control, run, checks, and role surfaces.
6. At least one configured harness. The checked-in example uses Codex for all
   roles; Claude Code is also supported by the same harness-neutral runtime.
7. A host authentication source for the selected harness, if that harness
   needs one. Codex commonly uses an <code>auth.json</code> file; Claude Code
   may use a credentials file or macOS Keychain authentication.

The <code>factory doctor</code> command checks these prerequisites together. It
also checks the repository remote and hooks, worker image, harness executables
and capabilities, cmux, authentication sources, and SQLite readiness. Run it
before claiming an issue rather than discovering a host problem after the
issue has been relabeled.

## Build and install

From this repository:

~~~sh
go version
make deps
make build
~~~

<code>make build</code> writes these binaries to <code>bin/</code>:

| Binary                             | Purpose                                                     |
| ---------------------------------- | ----------------------------------------------------------- |
| <code>factory</code>               | Host coordinator CLI.                                       |
| <code>factory-report</code>        | Worker-facing command that writes one structured report.    |
| <code>factory-worker-attach</code> | Internal worker attachment helper used by visible surfaces. |

To install the commands into Go's configured binary directory:

~~~sh
make install
~~~

The normal repository verification command is:

~~~sh
make check
~~~

It runs the formatting check, <code>go vet</code>, the complete Go test suite,
and a build. <code>make all</code> is an alias for the same verification
sequence. Other useful targets are <code>make test-race</code>,
<code>make fmt</code>, <code>make tidy</code>, and <code>make clean</code>.

Use command-specific help; the CLI intentionally keeps help attached to each
subcommand:

~~~sh
factory status --help
factory agent --help
factory-report --help
~~~

### Build the worker image

The worker image pins the base image, Go toolchain, Codex, Claude Code, and the
factory reporting binary. Build and verify it with:

~~~sh
make worker-build
~~~

The script verifies, using <code>--pull=never</code> for the worker run, that
the image:

- runs as UID <code>10001</code> with <code>no-new-privileges</code> and all
  Linux capabilities dropped;
- contains the stable <code>/work</code>, <code>/git</code>,
  <code>/cache</code>, <code>/invocation</code>, and <code>/results</code>
  paths;
- contains Go, Git, <code>gofmt</code>, Codex, Claude Code, and
  <code>factory-report</code>;
- has the expected clean environment and fixed <code>PATH</code>; and
- can run this repository's setup and gates from a sanitized checkout.

The command prints a local Docker content-addressable image ID. Put that value
in <code>worker_build.digest</code> in the repository's
<code>factory.yaml</code> if the build changed the image. This is a local
image config digest, not a registry manifest digest. The configured image and
digest must refer to the image available to the local Docker daemon.

## Configure a host

Factory has two configuration layers. The host configuration is private,
operator-owned state. The repository configuration is checked into the target
repository and is part of the frozen specification packet.

### Host configuration lookup

Every command accepts <code>--config &lt;absolute-path&gt;</code> after the
command name. If it is omitted, factory looks in this order:

1. <code>FACTORY_CONFIG</code>, when set;
2. the operating system user configuration directory under
   <code>factory/config.yaml</code>.

On a typical macOS installation, the second path is:
<code>/Users/&lt;user&gt;/Library/Application Support/factory/config.yaml</code>.

Using an explicit path is usually clearest for scripts and demonstrations.

### Initialize and register one repository

Create a host configuration, then register the repository:

~~~sh
factory init \
  --config /Users/me/.config/factory/config.yaml

factory register \
  --config /Users/me/.config/factory/config.yaml \
  --repository /Users/me/src/project \
  --github-owner example \
  --github-repository project \
  --authorized-user alice \
  --operational-data /Users/me/.local/share/factory/factory.db \
  --repository-config /Users/me/src/project/factory.yaml \
  --cmux-workspace factory-control \
  --codex-auth /Users/me/.codex/auth.json
~~~

<code>--authorized-user</code> can be repeated. Authentication paths are
optional at registration time when the harness will authenticate through a
visible session; provide them when a host source is available and the worker
needs managed credentials. <code>register</code> validates the repository and
initializes the SQLite store. It does not contact GitHub or create labels.

The v1 host configuration supports one repository registration. It stores
absolute paths and rejects an operational database inside the repository
checkout. Keep the operational database outside the checkout so a worker or
repository change cannot accidentally include coordinator state.

A resulting host file looks like this:

~~~yaml
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
      socket_path: ""
      control_workspace: factory-control
    authentication:
      codex_auth_path: /Users/me/.codex/auth.json
      claude_auth_path: /Users/me/.claude/.credentials.json
    operational_data_path: /Users/me/.local/share/factory/factory.db
    repository_config_path: /Users/me/src/project/factory.yaml
~~~

Factory stores the paths to host credentials, not credential contents, in the
host configuration. It reads the selected source only when seeding a
factory-managed worker credential volume. It never writes back to the source
file or mounts a host harness directory into a worker.

Verify the registration before doing anything destructive:

~~~sh
factory status --config /Users/me/.config/factory/config.yaml
factory doctor --config /Users/me/.config/factory/config.yaml
~~~

<code>doctor</code> renders every check, including checks after an earlier
failure. A blocking diagnosis returns exit status <code>1</code> and includes a
problem and an action. It does not print credentials, command transcripts, or
prompt content.

## Configure a repository

The target repository must contain <code>factory.yaml</code>. This file is
strict and versioned. Unknown fields and factory-owned workflow fields fail
closed. Repository policy can select the model, harness, timeouts, gates,
caches, and test exemptions, but it cannot redefine the factory's roles,
stages, prompts, transitions, or registries.

The checked-in <code>factory.yaml</code> is the policy for this Go repository.
Its important current characteristics are:

- target branch <code>main</code>;
- setup through <code>scripts/worker-go.sh mod download</code>;
- blocking gates in dependency order: <code>format</code>,
  <code>vet</code>, <code>test</code>, <code>build</code>;
- <code>clean</code> environment policy for setup and gates;
- advisory <code>test_policy.mode</code>, so <code>implementation</code> owns
  TDD by default (the independent <code>test</code> role remains available when
  policy is switched to required);
- configured <code>test</code>, <code>implementation</code>,
  <code>architecture</code>, <code>spec_review</code>, and
  <code>standards_review</code> role policy;
- human and technical exemption settings retained for required-mode repositories;
- a pinned worker image and digest; and
- no automatic base synchronization
  (<code>base_synchronization.mode: never</code>).

A compact repository policy has this shape:

~~~yaml
schema_version: 1
target_branch: main
setup: scripts/worker-go.sh mod download
setup_files:
  - go.mod
  - go.sum
setup_environment_policy: clean

gates:
  - name: format
    command: test -z "$(gofmt -l .)"
    timeout: 2m
    blocking: true
    environment_policy: clean
  - name: vet
    command: scripts/worker-go.sh vet ./...
    timeout: 5m
    blocking: true
    environment_policy: clean
  - name: test
    command: scripts/worker-go.sh test ./...
    timeout: 10m
    blocking: true
    depends_on: [vet]
    environment_policy: clean
  - name: build
    command: scripts/worker-go.sh build -o /tmp/project ./cmd/project
    timeout: 5m
    blocking: true
    depends_on: [test]
    environment_policy: clean

role_harness_defaults:
  test: codex
  implementation: codex
  architecture: codex
  spec_review: codex
  standards_review: codex

model_options:
  test: [gpt-5.6-luna]
  implementation: [gpt-5.6-luna]
  architecture: [gpt-5.6-luna]
  spec_review: [gpt-5.6-luna]
  standards_review: [gpt-5.6-luna]

timeouts:
  setup: 10m
  agent: 30m
  gate: 10m
  review: 15m

retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2

test_policy:
  mode: required
  allow_human_exemption: true
  allow_technical_exemption: true
  # Enable only after the measured pilot records a proceed decision in #26.
  allow_automated_objections: false

allowed_overrides: [model, reasoning_effort]

caches:
  - name: go-build
    path: /Users/me/.cache/factory/go-build
    read_only: false

worker_build:
  image: ghcr.io/example/project-worker
  digest: sha256:replace-with-the-verified-local-image-digest
  definition: worker/Dockerfile

base_synchronization:
  mode: never

evaluation:
  retention: ""
~~~

Important policy rules:

- <code>setup_files</code> should list manifests or lockfiles that make setup
  reproducible. They are used in setup fingerprinting.
- Gate names must be unique, timeouts positive, and dependencies must point to
  earlier gates. The coordinator retains results for every gate, including
  dependency skips and non-blocking outcomes.
- <code>role_harness_defaults</code> and <code>model_options</code> must refer
  to factory-owned roles. Supported harness names are <code>codex</code> and
  <code>claude</code>.
- Harness, model, and reasoning effort are selected independently. A command
  line value outside the declared options is rejected unless the corresponding
  name appears in <code>allowed_overrides</code>.
- <code>reasoning_effort_options</code> may be declared per role. Codex and
  Claude Code use different native argument names and accepted effort values,
  so declare the values intended for the selected harness.
- <code>test_policy.mode</code> is <code>required</code> or
  <code>advisory</code>. Required mode uses the factory-owned test role and
  independently verifies red evidence before implementation. Advisory mode
  starts implementation after baseline and makes its complete TDD loop part of
  the implementation handoff; it is not an exemption.
- <code>allowed_overrides</code> can contain <code>model</code>,
  <code>reasoning_effort</code>, and <code>harness</code>. Keep this list narrow
  because it widens the frozen run policy at invocation time.
- Cache paths are explicit host paths. A cache is mounted only at its declared
  stable worker path and is not a general host filesystem escape.
- <code>worker_build.digest</code> is mandatory for a pinned worker. Rebuild the
  image and update this value together.
- <code>evaluation.retention</code> is an operator-facing reporting policy. It
  does not cause automatic deletion; evaluation deletion always requires an
  explicit command and confirmation.

See [docs/configuration.md](docs/configuration.md) for the complete schema,
validation rules, gate semantics, role selection, worker mounts, and recovery
model.

## Run an issue from start to draft PR

Routine operation needs one command: <code>factory start</code> claims the
oldest eligible issue, drives it to a draft pull request, runs both independent
reviews and their bounded repair loop, marks the pull request ready, and claims
the next issue after a human merges or closes it - all without a further CLI
step. See [Persistent polling](#persistent-polling).

The one-shot commands below perform the same boundaries one at a time. Use them
for diagnosis or a deliberately manual run; they are the clearest way to see
each boundary.

### 1. Bootstrap the factory labels once

Label creation is explicit and idempotent:

~~~sh
factory bootstrap-labels \
  --config /Users/me/.config/factory/config.yaml
~~~

Factory owns exactly these labels:

~~~text
agent-ready
agent-running
agent-needs-input
agent-failed
agent-cancelled
agent-complete
~~~

<code>agent-ready</code> is the queue trigger. <code>ready-for-agent</code> is
not a factory label and is ignored. Bootstrap preserves ordinary repository
labels. Polling never creates missing labels, so perform this step deliberately
during setup.

### 2. Prepare and claim an issue

Create or select an open issue with the <code>agent-ready</code> label. If the
change needs a contract-first sequence, add exactly one
<code>factory-route</code> marker to the issue body now; the route cannot be
changed after claim. See [Workflow routes](#workflow-routes). Confirm that the
GitHub username issuing commands is registered as an authorized user, then run:

~~~sh
factory issue \
  --config /Users/me/.config/factory/config.yaml \
  42
~~~

The command:

1. reads and freezes the issue and resolved repository configuration;
2. fetches <code>origin/&lt;target_branch&gt;</code> and records its exact SHA;
3. creates <code>factory/&lt;run-id&gt;</code> and a sibling worktree under
   <code>.factory-worktrees/&lt;repository-name&gt;/&lt;run-id&gt;</code>;
4. leaves the ordinary checkout on its existing branch;
5. changes the issue to one factory state label while preserving ordinary
   labels;
6. creates one editable factory status comment; and
7. runs the baseline setup and gates automatically.

The output includes the run ID, branch, worktree, stage, status, test policy,
route, and baseline gate count. Save the <code>run</code> value. With this
repository's advisory policy and no route marker, a healthy run normally becomes
<code>implementation/active</code>. A required-mode repository normally becomes
<code>test/active</code>; an authorized human exemption can move that run
directly to <code>implementation/active</code>. The <code>acceptance</code>
route becomes <code>test/active</code> and the <code>design-acceptance</code>
route becomes <code>architecture/active</code>.

The claim refuses closed issues, pull requests, issues without the exact
<code>agent-ready</code> label, conflicting factory state, a malformed or
unavailable route marker, and a repository that already has a non-terminal run. A failed claim compensates for any workspace it
created. A baseline failure prevents agent startup unless the frozen issue
contains an explicit <code>factory-baseline-target</code> marker permitted by
the baseline policy. The supported target values are <code>setup</code>,
<code>test</code>, and <code>all</code>.

### 3. Start the visible role

Start the role selected by the active stage:

~~~sh
factory agent \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>
~~~

The output reports:

- invocation ID;
- run ID;
- role and stage;
- frozen test policy and workflow route;
- worker-backed workspace ID; and
- opaque cmux surface IDs.

The invocation receives a read-only specification packet at
<code>/invocation</code> and a writable, invocation-specific results directory
at <code>/results</code>. The prompt and terminal content are not printed by
the coordinator.

The packet is the complete frozen claim; the prompt is a projection of it. The
prompt fences the claimed issue, the accepted clarifications, and the frozen
run parameters the role acts on, and points at the mounted packet for the rest,
so repository guidance appears in the prompt once, inside its own untrusted
fence. A
repair or objection revision that resumes an existing harness session receives
a continuation prompt carrying only the changed coordinator-owned context and
the factory-owned rules; a repair that cannot resume a session receives the
complete prompt.

With this repository's advisory policy and no route marker, the first
invocation is the <code>implementation</code> role. On the
<code>acceptance</code> route it is the <code>test</code> role; on the
<code>design-acceptance</code> route it is the <code>architecture</code> role,
whose accepted design is then handed to the test role. For an advisory run with
no route marker, use the visible implementation surface to own the complete
red/green/refactor loop, including a focused behavioral test when practical,
then submit the common implementation report from inside that worker
(see [Structured agent reports](#structured-agent-reports)). Accept it from the
host:

~~~sh
factory agent-report \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --invocation-id <implementation-invocation-id>
~~~

For a required-mode completed test handoff, the coordinator independently reruns
the focused test command. It requires a nonzero result and the expected failure
reason in the command output. Once verified, it creates the test checkpoint,
protects the reported test paths, and automatically starts a fresh
implementation invocation. The accepted command output identifies the test
invocation that was submitted; record the new implementation invocation ID from
the newly created visible surface or its coordinator output before accepting the
implementation report. Do not submit an implementation report using the test
invocation ID.

If the test report requests clarification or cannot be verified, the run is
paused for a human. Factory does not silently turn an unverifiable red test
into an implementation handoff.

### 4. Implement and accept the change

The implementation role edits only the run worktree. It should report the
observable change, acceptance evidence, production paths, focused commands, and
known limitations. A typical report command is:

~~~sh
factory-report \
  --outcome completed \
  --summary 'implementation complete' \
  --change-summary 'implemented the requested behavior' \
  --acceptance 'criterion=focused test passes' \
  --production-file internal/example.go \
  --focused-command 'go test ./internal/...'
~~~

Then ask the host coordinator to validate and accept the report:

~~~sh
factory agent-report \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --invocation-id <implementation-invocation-id>
~~~

The coordinator validates report identity, schema, role, stage, permitted paths,
worktree state, and native session identity. Terminal output is never treated
as a result. An accepted implementation report leaves the run ready for the
host-owned checkpoint and check sequence.

If implementation finds that a protected test encodes the wrong contract, it
can add a structured objection instead of editing that test:

~~~sh
factory-report \
  --outcome completed \
  --summary 'implementation found a disputed test claim' \
  --change-summary 'implemented the observable behavior' \
  --acceptance 'criterion=focused test' \
  --production-file internal/example.go \
  --focused-command 'go test ./internal/...' \
  --test-objection 'internal/example_test.go:TestBehavior|the assertion is too narrow|public behavior passes while the assertion fails'
~~~

The coordinator preserves the objection and waits for a human while
`test_policy.allow_automated_objections` is false. Once the measured pilot has
authorized automation, and the latest authorized maintainer decision comment
on issue #26 says `Decision: proceed`, it resumes the original test session
with the current implementation context. A later `revise and repeat` or `stop`
decision closes that gate. The test role may accept or reject the objection; an
accepted revision must pass a new independent red verification before
implementation resumes, and the cycle stops for human disposition after two
attempts or any verification failure.

An optional architecture detour can be started when the run has no conflicting
active invocation:

~~~sh
factory agent \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --role architecture \
  --stage architecture \
  --permitted-path docs/architecture
~~~

The architecture role is restricted to <code>docs/architecture</code> by its
factory definition and returns the run to implementation after its report is
accepted.

### 5. Create the draft pull request

After the implementation report is accepted and its invocation has finished:

~~~sh
factory draft-pr \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>
~~~

The host coordinator:

1. verifies that the worktree is still at the expected parent checkpoint and,
   for required-mode runs, that protected test paths are unchanged;
2. creates an implementation checkpoint commit with a message containing
   <code>factory: implementation checkpoint &lt;run-id&gt;</code>;
3. pushes <code>factory/&lt;run-id&gt;</code> so GitHub can resolve the checkpoint
   commit named by every gate status;
4. runs every configured gate against that exact checkpoint; and
5. creates or updates one draft pull request targeting the configured base
   branch only if the required gates pass.

Workers never commit, push, access a GitHub API, or receive a GitHub token.
Repeating <code>factory draft-pr</code> after the first PR exists regenerates
only the coordinator-owned section between these markers:

~~~text
<!-- factory-generated:start -->
<!-- factory-generated:end -->
~~~

Human-authored text outside those markers is preserved. The PR body includes
the issue, packet version, checkpoint, gate results, test disposition, review
projection, intervention marker, and control commands.

If a gate fails, the checkpoint remains pushed on the run branch and
<code>draft-pr</code> returns the bounded check-repair decision without creating
or updating the draft pull request. The next implementation invocation reuses
the worker role volume and implementation surface, subject to the frozen repair
budget. After an accepted repair report, run <code>factory draft-pr</code> again.
Infrastructure waits do not spend the check-repair budget; the repository's
<code>retry_limits.check_repair</code> value is the only bound.

### 6. Run the concurrent isolated reviews

After the draft PR is created, the coordinator starts both active review roles:

~~~sh
factory agent \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>
~~~

The automatic roles for <code>draft_pr</code> are <code>spec_review</code> and
<code>standards_review</code>. Their external sessions run concurrently, but
report acceptance is serialized by the coordinator. Each receives the exact
immutable base-to-checkpoint diff and content-free gate metadata in a fresh,
read-only worker surface with private home and temporary storage. Reviewers do
not receive implementation/test handoffs, upstream transcripts, or the other
reviewer's conclusions. A review finding has seven fields:

~~~text
location|claim|evidence|severity|category|resolution|owner
~~~

For example:

~~~sh
factory-report \
  --outcome completed \
  --summary 'review complete' \
  --finding 'internal/example.go:42|claim|observable evidence|blocker|correctness|repair the behavior|implementation'
~~~

Accept the review as usual:

~~~sh
factory agent-report \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --invocation-id <review-invocation-id>
~~~

Correctness, security, specification, and documented-standards blockers are
combined from both reviewers into one implementation repair packet. A finding
that suggests test ownership must use the existing structured test-objection
protocol; implementation never edits protected tests directly. An accepted
repair creates a new checkpoint, reruns every configured gate, and starts both
review roles in fresh sessions against that new SHA. The repository's
<code>retry_limits.review_repair</code> value bounds the budget, and a
materially repeated blocker escalates immediately to a human. Taste and scope
findings remain advisory.

The run moves to <code>ready</code> only after both configured reviews succeed,
the configured target branch has been merged into the factory branch when
<code>base_synchronization.mode: before_ready</code> is enabled, and the final
checkpoint has passed all gates. A target-branch merge that changes the SHA
returns the run to checks and starts a fresh review round. A merge conflict
waits for human disposition; Factory never rebases or force-updates the run
branch. After the final checks the coordinator explicitly marks the pull
request ready for human inspection and merge.

### 7. Observe and finish the run

At any point, inspect local state with:

~~~sh
factory status --config /Users/me/.config/factory/config.yaml
~~~

The issue's factory label and editable status comment are also updated at
workflow transitions. A merged PR completes the run and records its merge
commit. Closing the issue or closing an unmerged PR cancels the run. Merge
detection takes precedence over a closed PR state.

## Structured agent reports

<code>factory-report</code> is the only supported result boundary for a visible
worker. The coordinator injects identity and result-path variables into the
worker:

~~~text
FACTORY_INVOCATION_ID
FACTORY_RUN_ID
FACTORY_HARNESS
FACTORY_ROLE
FACTORY_STAGE
FACTORY_RESULT_DIR
~~~

The agent does not need to construct these values.
<code>factory-report</code> writes one schema-versioned <code>report.json</code>
atomically, with a bounded size, into the invocation result directory. Do not
run it from the host shell; the required identity environment is supplied only
inside the worker surface.

### Common completed handoff

Use these fields for implementation or architecture:

~~~sh
factory-report \
  --outcome completed \
  --summary 'implementation complete' \
  --change-summary 'implemented the requested behavior' \
  --acceptance 'criterion=observable evidence' \
  --production-file internal/example.go \
  --focused-command 'go test ./internal/...'
~~~

Repeat <code>--acceptance</code>, <code>--production-file</code>,
<code>--focused-command</code>, and <code>--known-limitation</code> as needed.
Keep evidence observable and concise. Do not put a transcript, chain of
thought, or credentials in a report.

### Required-mode test handoff

Test reports use test-specific fields and must not report production paths:

~~~sh
factory-report \
  --outcome completed \
  --summary 'focused behavior test is red on base' \
  --acceptance 'criterion=focused red test' \
  --test-file internal/example_test.go \
  --focused-test-command 'go test ./internal -run TestBehavior' \
  --expected-failure-reason 'expected behavior assertion' \
  --observed-failure 'exit_code=1'
~~~

The coordinator reruns <code>--focused-test-command</code> inside the worker.
Passing output, a missing expected failure reason, a command/path dispute, or
any other unverifiable result becomes a <code>test_dispute</code> escalation
and pauses for a human; it does not trigger the implementation objection cycle.

For an implementation objection, repeat `--test-objection` once per disputed
claim. A resumed test role reports its decision with the test response flags:

~~~sh
factory-report \
  --outcome completed \
  --summary 'test objection accepted' \
  --objection-decision accepted \
  --objection-reason 'the public behavior is the stable contract' \
  --acceptance 'criterion=revised focused test' \
  --test-file internal/example_test.go \
  --focused-test-command 'go test ./internal -run TestBehavior' \
  --expected-failure-reason 'expected behavior assertion' \
  --observed-failure 'exit_code=1'
~~~

Use `--objection-decision rejected` with a concise reason when the test claim
stands. Rejection, direct edits to protected paths, or failed red verification
pauses the run for human disposition.

### Clarification and blocked outcomes

An agent can report:

~~~sh
factory-report --outcome needs_clarification \
  --summary 'the issue does not specify the required JSON compatibility' \
  --question 'clarification-1=Should the existing JSON field remain backward compatible?'
~~~

Or it can report an observable blocker:

~~~sh
factory-report --outcome cannot_proceed \
  --summary 'required dependency is unavailable in the pinned worker' \
  --blocker setup
~~~

Clarification pauses stop the worker and publish a question. They do not consume
retry or repair budget. Answers create a new packet version and a fresh
invocation.

### Review handoff

Reviewers use one or more <code>--finding</code> values:

~~~sh
factory-report \
  --outcome completed \
  --summary 'review complete' \
  --finding 'path:line|claim|evidence|advisory|scope|consider narrowing the change|implementation'
~~~

The coordinator validates that review findings target the exact checkpoint and
that the reviewer changed no files. It persists bounded finding metadata and
uses it to update the generated PR review section.

### Usage metadata

The report CLI can accept reliable harness usage values such as
<code>--input-tokens</code>, <code>--output-tokens</code>,
<code>--total-tokens</code>, <code>--cost-micros</code>, and
<code>--cost-currency</code>. These values are optional. They are stored only
as bounded evaluation metadata and are not inferred from transcripts.

## GitHub commands and human intervention

Authorized users can control the current run with exact, single-line comments:

~~~text
/factory status
/factory refresh
/factory answer clarification-1 use the existing JSON format
/factory changes validate permitted paths before the adoption return
/factory revision
/factory retry
/factory cancel
/factory config harness=codex
~~~

Only usernames registered in the host configuration may issue these commands.
`/factory revision` is authorized only on an active ready run. It snapshots
the current open issue into a new packet version, drafts the tracked pull
request, invalidates every prior gate and review result, treats the current
clean checkpoint as the new amendment baseline, preserves the branch and
worktree, and restarts the workflow from that checkpoint. The
command does not rebase, force-push, merge, or delete anything remotely.
The command parser rejects malformed or unauthorized comments without changing
workflow state. Comments are processed once using a persisted watermark.

| Comment                                                         | Effect                                                                                                                                                                             |
| --------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| <code>/factory status</code>                                    | Re-renders the current status projection.                                                                                                                                          |
| <code>/factory refresh</code>                                   | Re-reads the issue into a new packet version, preserves resolved answers, and invalidates downstream work that no longer matches.                                                  |
| <code>/factory answer &lt;question-id&gt; &lt;answer&gt;</code> | Answers one pending question and starts a fresh invocation against the new packet. <code>question=</code>, <code>question-id=</code>, or <code>id=</code> forms are also accepted. |
| <code>/factory changes &lt;instruction&gt;</code>              | Supplies one maintainer instruction for the tracked pull request and resumes implementation from the current checkpoint. The supervision-comment equivalent of a <code>CHANGES_REQUESTED</code> review, and likewise unbudgeted. |
| <code>/factory retry</code>                                     | Reopens the current failed or explicitly cancelled stage when policy permits.                                                                                                      |
| <code>/factory cancel</code>                                    | Stops the worker and cancels the run while retaining artifacts.                                                                                                                    |
| <code>/factory config harness=codex</code>                      | Selects a permitted harness for a later invocation only.                                                                                                                           |

Run the one-shot comment/lifecycle poll when operating without the persistent
coordinator:

~~~sh
factory poll \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>
~~~

This checks issue and pull-request lifecycle first, then processes newly
authorized structured commands. Repeating it is safe.

## Recovery and authentication

Factory persists an effect journal around external mutations. Before advancing
after a restart, it compares the run against the issue, factory label, status
comment, branch, worktree, checkpoint, pull request, worker, terminal, native
session, and operational store. If those sources disagree, factory pauses
instead of guessing.

### Normal recovery

Use the following commands only after reading the current
<code>status</code> and any diagnosis:

~~~sh
factory resume \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>

factory attach \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>

factory auth refresh \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>
~~~

- <code>resume</code> retries harness capacity or performs an explicit
  native-session recovery. It does not change the frozen packet.
- When restart reconciliation has paused a coordinator-owned <code>check</code>
  stage, <code>resume</code> re-enters check evaluation without launching a new
  implementation agent; run <code>factory draft-pr</code> afterward.
- A manually resumed native session sets an attach gate. <code>attach</code>
  restores the worker and visible terminal topology and clears that gate before
  report acceptance or workflow progression can continue.
- <code>auth refresh</code> reseeds only the factory-managed credential volume
  for the selected invocation harness. It never modifies the registered host
  source.
- Rate limits and retryable capacity failures use
  <code>waiting_for_harness</code> and do not spend workflow or check-repair
  budgets.
- An expired or missing credential generally requires an operator refresh and
  enters <code>waiting_for_human</code> rather than repeatedly trying a bad
  source.

### Pending effects and discrepancies

Run a read-only reconciliation pass:

~~~sh
factory reconcile \
  --config /Users/me/.config/factory/config.yaml
~~~

If the output names a pending effect, inspect the external system and local
artifacts before abandoning it. Only after that inspection may an operator
explicitly mark the ambiguous effect abandoned:

~~~sh
factory reconcile \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --abandon-effect <effect-id> \
  --reason 'verified that the GitHub update was not applied'
~~~

The reason is recorded and the run remains visible for human reconciliation.
Do not edit SQLite rows, delete a branch, or rerun a mutating command to work
around a discrepancy. The exact effect identity and external state are needed
to preserve idempotence.

## Persistent polling

<code>factory start</code> is the long-lived queue supervisor and the
unattended driver of the routine path. It performs the full startup diagnosis,
takes a private host lock, renews a visible <code>factory/lease</code> commit
status, backs off transient queue/lease transport failures, suppresses new
claims while a run is active, and claims the oldest eligible open issue by
issue number.

Start and stop it with:

~~~sh
factory start --config /Users/me/.config/factory/config.yaml

# From another shell:
factory stop --config /Users/me/.config/factory/config.yaml
~~~

<code>stop</code> stops polling and leaves the active run, branch, worktree,
worker artifacts, and workflow status intact. It is not a cancellation command.

### Unattended claim-to-draft-PR progression

After every polling observation that claimed or found a run, the coordinator
drives that run as far as the frozen packet allows. It owns every transition;
no agent may choose, skip, or redefine one.

1. A newly claimed run evaluates the frozen baseline against its immutable base
   checkpoint.
2. A healthy baseline enters the stage the frozen route and repository test
   policy select. With advisory policy and no route, that is
   implementation-owned TDD: no test invocation, test handoff, protected test
   path, or test checkpoint is created. The <code>acceptance</code> and
   <code>design-acceptance</code> routes and required policy enter their own
   declared stages instead.
3. The coordinator launches the role the workflow registry declares for that
   stage, then waits while the invocation runs.
4. When the invocation writes its structured report, the coordinator validates
   and accepts it through the same boundary as <code>factory agent-report</code>.
   A structured implementation objection either waits for the measured-pilot
   gate, or starts the bounded native-resume test cycle; an accepted revision
   must pass independent red verification before the implementation stage can
   continue. Rejection, failed verification, or an objection after the second
   attempt pauses for a human.
5. An accepted implementation is checkpointed, every configured gate runs
   against that exact checkpoint, and a failed gate enters the bounded
   check-repair loop.
6. A coherent checkpoint that passes every gate is pushed and receives one
   draft pull request.

Repeated polling and a coordinator restart are safe. Every transition runs
through the same durable effect journal as the one-shot commands, so a second
pass reconciles pending effects instead of creating a second invocation,
checkpoint, push, comment, status, or pull request. The supervisor also
observes a tracked issue or pull request before restart reconciliation when no
effect is pending, which lets an already-merged or closed target enter its
terminal state even after GitHub deletes the run branch.

### Unattended review, repair, and readiness

The draft pull request is not the end of the routine path. When the frozen
packet declares both independent review roles, the coordinator continues
without any stage-driving command.

1. It launches the specification reviewer and the standards reviewer for the
   same immutable checkpoint. Launching is serialized; the two sessions then
   run concurrently in their own workers, homes, and result directories, and
   neither reviewer ever sees the other's findings.
2. When both reviewers have reported for the exact current checkpoint, their
   blocking findings are combined into one implementation repair packet. A
   finding that names the test role still reaches it through the existing
   structured objection protocol; implementation never edits a protected test.
3. The repair produces a new checkpoint, which reruns every configured gate and
   both reviewers in fresh sessions. No gate result and no review result
   survives a code change, because both are keyed by the exact commit.
4. A materially repeated blocker escalates immediately, even with budget left,
   and an exhausted <code>review_repair</code> budget escalates too. Both enter
   the waiting-for-human state instead of looping.
5. When the final checkpoint passes every gate and neither reviewer reports a
   blocker, the coordinator synchronizes the target if configured, removes the
   draft flag, and waits. The factory never approves and never merges.

### Human review as a repair trigger

One or more submitted GitHub reviews with the <code>CHANGES_REQUESTED</code>
decision from users in <code>authorized_users</code> are a progression event.
The coordinator combines all concurrent applicable reviews before one repair
flow begins, consuming them as a single human repair packet holding every
review body and inline comment. It returns the pull request to draft when it was
already ready and resumes implementation from the current checkpoint. All
downstream results for the superseded checkpoint are invalidated, so every gate
and both fresh reviewers must pass again before the pull request can become
ready a second time.

An authorized <code>/factory changes &lt;instruction&gt;</code> comment is the same
progression event expressed as a supervision comment. It exists because GitHub
refuses a <code>CHANGES_REQUESTED</code> decision from a pull request's own
author, so a host whose coordinator account is also its only authorized user
has no submitted review to give. The comment is admitted on the same stages and
statuses as a submitted review, and only while the run has no active
invocation, so it never interrupts a harness session that can still write a
structured result. The instruction becomes one blocking finding owned by
implementation, carrying the same provenance as a review body.

A human-requested repair is not one of the bounded factory repair rounds and
never consumes the <code>review_repair</code> budget. The editable status
comment names the review that triggered it under the review-repair cycle.

These do not start work: an unsubmitted review draft, an ordinary issue or
pull-request comment, a <code>COMMENTED</code> review, an approval, a dismissed
decision, and any review from a user outside <code>authorized_users</code>.

The applied review identity is persisted as a watermark on the run, so repeated
polling and a coordinator restart cannot apply the same review twice.

### Terminal outcomes and queue continuation

The coordinator observes the tracked pull request before it applies any
transition, so a terminal disposition always wins over further work.

| Observation                        | Outcome                                                                         |
| ---------------------------------- | ------------------------------------------------------------------------------- |
| Pull request merged                | The run completes and records the merge commit.                                  |
| Pull request closed without merge  | The run is cancelled; its branch, worktree, worker, and artifacts are retained.  |
| Issue closed                       | The run is cancelled the same way.                                               |

Either terminal outcome releases the one-active-run constraint. The same
<code>factory start</code> process then claims the next oldest eligible issue
without waiting for a full polling interval and without an operator restart.
Retention is unchanged: the terminal workspace, branch, worktree, and worker
survive the transition, and cleanup stays an explicit, separate seven-day
operation.

### States that intentionally pause progression

A non-terminal waiting state keeps the run active and suppresses new claims, so
the queue stays blocked until a person or infrastructure resolves it. The
reason is published in the editable status comment.

| Pause                                   | Who resolves it                                     |
| --------------------------------------- | --------------------------------------------------- |
| Clarification request                   | An authorized <code>/factory answer</code> comment.  |
| Ready pull request awaiting disposition | A human merge or close.                              |
| Repeated blocker or exhausted budget    | A human decision on the finding.                     |
| Policy rejection                        | An authorized command or an issue change.            |
| Harness rate limit or expired auth      | Harness infrastructure; the coordinator retries.     |
| Invocation needing <code>factory attach</code> | An operator attaching the resumed session.    |
| Ambiguous recovery discrepancy          | A human reconciliation decision.                     |

### Activity in status output

The <code>agent-running</code> issue label covers every active run, so it
cannot say whether a harness is executing. <code>factory status</code> and the
editable status comment publish a separate activity value:

| Activity                          | Meaning                                                             |
| --------------------------------- | ------------------------------------------------------------------- |
| <code>invocation-active</code>    | One harness invocation is executing; its identity is published too. |
| <code>run-active</code>           | The run is active and the coordinator owns the next transition.     |
| <code>waiting-for-human</code>    | Progression stopped until a person answers or disposes.             |
| <code>waiting-for-harness</code>  | Progression stopped until harness infrastructure recovers.          |
| <code>terminal</code>             | The run is complete, cancelled, or failed.                          |

### One-shot commands

<code>factory issue</code>, <code>factory agent</code>,
<code>factory agent-report</code>, and <code>factory draft-pr</code> remain
available for diagnosis and deliberate manual operation. They are not part of
routine unattended progression. Likewise, <code>factory poll</code> is the
explicit one-shot command for issue/PR lifecycle observation and structured
GitHub command handling. It does not consume human reviews: the
<code>CHANGES_REQUESTED</code> repair trigger belongs to the persistent
coordinator.

## Evaluation and cleanup

### Local evaluation summaries

Factory records content-free local evaluation metadata: run outcome, stage
durations, invocation counts, gate counts, retry counts, exemptions,
escalation categories, dispositions, and reliable usage values when supplied.
It does not copy issue text, prompts, transcripts, diffs, source files, command
output, logs, or credentials into the evaluation projection.

View summaries and aggregates:

~~~sh
factory evaluation \
  --config /Users/me/.config/factory/config.yaml

factory evaluation \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>
~~~

Test disputes and review findings can be given an explicit human disposition:

~~~sh
factory evaluation-disposition \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --event-id <event-id> \
  --category test_dispute \
  --disposition upheld
~~~

Valid dispositions are <code>upheld</code>, <code>advisory</code>, and
<code>overturned</code>. Event IDs are hashed before persistence.

Evaluation summaries are never deleted implicitly. Delete only selected
terminal summaries with an explicit RFC3339 cutoff and confirmation:

~~~sh
factory evaluation-delete \
  --config /Users/me/.config/factory/config.yaml \
  --before 2026-01-01T00:00:00Z \
  --confirm
~~~

### Run-artifact cleanup

Terminal run artifacts become eligible seven days after the run enters its
current terminal state. An open pull request keeps its run. Active and waiting
runs are not selected. Cleanup never deletes a remote branch and retains
factory-managed credential volumes and evaluation summaries.

Always preview the exact targets first:

~~~sh
factory cleanup --config /Users/me/.config/factory/config.yaml
~~~

The preview lists each selected worktree, local branch, worker target, role
volume, stored output, and terminal workspace. The preview intentionally
returns exit status <code>2</code> when no <code>--confirm</code> flag is
supplied. Confirm the displayed plan:

~~~sh
factory cleanup \
  --config /Users/me/.config/factory/config.yaml \
  --confirm
~~~

To target one terminal run:

~~~sh
factory cleanup \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --confirm
~~~

A pending effect, malformed run identity, malformed terminal workspace handle,
unproven path scope, or a still-open pull request blocks cleanup for that run.
The confirmation pass refuses a changed plan, so the resources displayed in the
preview are the resources being authorized for removal.

Cleanup also closes the terminal workspaces the removed runs created, because
the deleted invocation rows are the only durable record of those handles. A
workspace close never blocks local deletion: when the terminal is unavailable
or refuses the close, cleanup removes the local resources anyway and prints
<code>cleanup retained terminal workspace</code> for each workspace the
operator has to close by hand.

## Security and data boundaries

The worker is the execution boundary and the host coordinator is the mutation
boundary.

### Worker mounts and capabilities

Workers use stable paths:

| Path                             | Access         | Contents                                                      |
| -------------------------------- | -------------- | ------------------------------------------------------------- |
| <code>/work</code>               | read/write     | The isolated run worktree.                                    |
| <code>/git</code>                | read-only      | A credential-free Git projection used by repository commands. |
| <code>/cache/&lt;name&gt;</code> | policy-defined | Only explicitly declared caches.                              |
| <code>/invocation</code>         | read-only      | The frozen invocation packet.                                 |
| <code>/results</code>            | read/write     | The current invocation's structured report directory.         |
| <code>/run/factory-auth</code>   | read-only      | A managed credential volume for the selected harness role.    |

The worker runs as non-root UID <code>10001</code>, drops all capabilities,
enables <code>no-new-privileges</code>, and does not receive:

- the host home directory;
- the host <code>.git</code> directory or Git credentials;
- GitHub tokens or the host <code>gh</code> credential store;
- SSH keys, macOS Keychain access, or the Docker socket;
- host Codex/Claude directories, plugins, hooks, MCP configuration, or
  personal settings; or
- arbitrary host paths outside the declared mounts.

Workers cannot commit, create branches, push, call GitHub, or mutate the
operational store. The host <code>GitWorkspace</code> performs checkpoint, push,
and cleanup effects after the coordinator validates the result.

### Reports, state, and telemetry

Reports are bounded, schema-versioned, and identity-checked. The coordinator
accepts structured handoffs, not terminal text. The SQLite store uses private
directory/file permissions, rejects newer or unknown schemas, and makes a
timestamped backup before a supported migration.

Factory has no outbound telemetry. Evaluation data is deliberately a separate,
content-free projection. Host authentication sources are read-only, and the
managed worker credential volume is separate for each role/run as required by
the runtime.

## Command reference

All host commands accept <code>--config &lt;path&gt;</code> after the command
name. Use <code>factory &lt;command&gt; --help</code> for the exact flags
supported by the installed binary.

| Command                                     | Purpose                                                                                                                                                           |
| ------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| <code>factory init</code>                   | Create an empty host configuration.                                                                                                                               |
| <code>factory register</code>               | Register the one repository, GitHub identity, authorized users, polling settings, cmux settings, auth sources, repository policy path, and SQLite path.           |
| <code>factory bootstrap-labels</code>       | Create the six factory-owned GitHub labels explicitly and idempotently.                                                                                           |
| <code>factory doctor</code>                 | Run the complete startup diagnosis and print every problem/action.                                                                                                |
| <code>factory start</code>                  | Run the persistent queue/lease supervisor, drive each claimed run through review, repair, and readiness, and continue with the next issue after a terminal outcome. |
| <code>factory stop</code>                   | Stop a running supervisor without cancelling the active run.                                                                                                      |
| <code>factory issue [--issue N] N</code>    | Diagnostic: claim one issue and run its baseline. The number may be positional or supplied with <code>--issue</code>, but not both.                               |
| <code>factory agent</code>                  | Diagnostic: start the active stage's visible role, or select a validated role/stage/harness/model/reasoning override.                                             |
| <code>factory agent-report</code>           | Diagnostic: validate and accept a report already written by one invocation. Requires <code>--run-id</code> and <code>--invocation-id</code>.                      |
| <code>factory draft-pr</code>               | Diagnostic: checkpoint the implementation, push the branch, run gates, and create/update the draft PR. <code>--intervention</code> records a one-line marker.     |
| <code>factory poll</code>                   | Process one lifecycle and structured-command observation for the current run.                                                                                     |
| <code>factory status</code>                 | Show the selected host configuration, repository, latest run, its activity, and recovery diagnosis.                                                                |
| <code>factory resume</code>                 | Perform explicit native-session or harness-capacity recovery, or re-enter a recovery-paused check stage.                                                          |
| <code>factory attach</code>                 | Restore worker/terminal topology after a manual resume and clear the attach gate.                                                                                 |
| <code>factory auth refresh</code>           | Reseed a managed worker credential for the active invocation harness.                                                                                             |
| <code>factory reconcile</code>              | Run restart reconciliation, or abandon one inspected pending effect with <code>--abandon-effect</code> and <code>--reason</code>.                                 |
| <code>factory evaluation</code>             | Read local content-free run summaries and aggregates.                                                                                                             |
| <code>factory evaluation-disposition</code> | Record a human disposition for a <code>test_dispute</code> or <code>review_finding</code> event.                                                                  |
| <code>factory evaluation-delete</code>      | Explicitly delete terminal evaluation summaries before an RFC3339 cutoff with <code>--confirm</code>.                                                             |
| <code>factory cleanup</code>                | Preview or, with <code>--confirm</code>, remove eligible local run artifacts.                                                                                     |

### Useful factory agent flags

~~~text
--run-id <id>
--role <factory-owned-role>
--stage <stage>
--harness codex|claude
--model <declared-or-permitted-model>
--reasoning-effort <declared-or-permitted-effort>
--codex-auth <absolute-source-path>
--claude-auth <absolute-source-path>
--permitted-path <repository-relative-prefix>   # repeatable
~~~

The coordinator validates all overrides against the frozen repository policy.
An arbitrary role is never accepted.

### factory-report flags

The worker-facing command supports:

~~~text
--outcome completed|needs_clarification|cannot_proceed
--summary <short-observable-summary>
--change-summary <implementation-or-architecture-summary>
--acceptance criterion=evidence                         # repeatable
--production-file <repository-relative-path>            # repeatable
--focused-command <command>                              # repeatable
--known-limitation <limitation>                          # repeatable
--test-file <repository-relative-path>                   # repeatable
--infrastructure-file <repository-relative-path>        # repeatable
--focused-test-command <command>
--expected-failure-reason <text>
--observed-failure kind=detail                            # repeatable
--test-objection test|claim|evidence                      # repeatable, implementation only
--objection-decision accepted|rejected                   # test revision only
--objection-reason <reason>                              # test revision only
--uncovered-criterion <criterion>                        # repeatable
--finding location|claim|evidence|severity|category|resolution|owner  # repeatable
--question id=text                                        # repeatable
--evidence kind=detail                                    # repeatable
--exemption human|technical|baseline                     # repeatable
--escalation test_dispute|review_finding                 # repeatable
--blocker setup|gate|test|review|harness|budget|unknown  # repeatable
--budget-exhausted
--native-session-id <harness-native-id>
~~~

Usage flags are optional and include <code>--input-tokens</code>,
<code>--output-tokens</code>, <code>--total-tokens</code>,
<code>--cost-micros</code>, and <code>--cost-currency</code>.

### Exit status

- <code>0</code>: command completed successfully;
- <code>1</code>: operational failure or a blocking diagnosis; and
- <code>2</code>: missing, unknown, or invalid command arguments. A cleanup
  preview without <code>--confirm</code> also returns <code>2</code> by design,
  after printing the plan and making no mutation.

## Development

The repository is a Go module named
<code>github.com/Stevie1704/sw-factory</code>. Important package boundaries are:

~~~text
cmd/factory                 host CLI entrypoint
cmd/factory-report          worker report entrypoint
cmd/factory-worker-attach   worker PTY attachment entrypoint
internal/cli                command parsing and rendering
internal/factory            orchestration and lifecycle transitions
internal/config             host/repository policy loading and validation
internal/store              private SQLite operational state
internal/git                worktrees, checkpoints, push, and cleanup seams
internal/github             GitHub issue, label, comment, status, and PR seams
internal/worker              pinned Docker worker runtime
internal/terminal            visible cmux terminal runtime
internal/harness             Codex and Claude Code adapters
internal/report              structured report schema and validation
internal/workflow            factory-owned roles, stages, and transitions
~~~

Run the standard local checks before submitting a change:

~~~sh
make fmt-check
make vet
make test
make test-race
make build
~~~

Or run the normal aggregate:

~~~sh
make check
~~~

The worker build is itself a useful integration check because it sanitizes the
checkout, verifies the pinned image contract, and runs setup plus the
repository's configured gates inside Docker:

~~~sh
make worker-build
~~~

The domain vocabulary and ownership model are recorded in
[CONTEXT.md](CONTEXT.md). Architecture decisions are in
[docs/adr/](docs/adr/).

## Troubleshooting

### factory doctor is blocked

Read every reported <code>problem</code> and <code>action</code>; the command
continues through all checks instead of stopping at the first error. Common
causes are an unreachable Docker daemon, a missing pinned image digest, an
unavailable cmux socket, missing <code>gh</code> permissions, an unsupported
harness executable, or a repository/operational path that is not absolute and
safely scoped.

### The issue was not claimed

Check that the issue is open, is not a pull request, has the exact
<code>agent-ready</code> label, and is not already in another factory state.
Confirm that the command's GitHub username is in <code>authorized_users</code>.
Run <code>factory bootstrap-labels</code> once if the labels do not exist.

### Baseline or a gate fails

The baseline is intentionally evaluated before agent edits. Inspect the gate
name, blocking flag, environment policy, setup fingerprint, and exact command.
Do not interpret a baseline failure as an implementation failure. A frozen
<code>factory-baseline-target</code> exemption is a policy-controlled
exception; use it only when the issue explicitly explains why the selected
baseline target is appropriate.

### The run is waiting for a human

Inspect the issue or PR status comment and run <code>factory status</code>. For
a pending question, answer with the authorized
<code>/factory answer ...</code> command. For a required-mode test dispute or
review blocker, inspect the evidence and record an evaluation disposition if
appropriate. For a recovery discrepancy, use <code>factory reconcile</code> and
resolve the external state before continuing.

### The run is waiting for harness capacity or authentication

Use <code>factory resume</code> for a retryable harness problem. Refresh the
selected credential with <code>factory auth refresh</code> when authentication
has expired. If a manual native resume was performed, finish with
<code>factory attach</code> so the coordinator can verify the visible worker and
terminal topology.

### draft-pr refuses to run

The implementation invocation must have a terminal accepted report, the run
must be <code>implementation/active</code> or in an allowed <code>check</code>
wait. A <code>check/waiting_for_human</code> run may continue only when its
lifecycle reason begins with <code>restart reconciliation paused:</code>; use
<code>factory reconcile</code> first, then <code>factory resume</code> or
<code>factory draft-pr</code>. The worktree must be clean at the recorded
checkpoint, and protected test paths in required mode must still match. Submit or
correct the report first; do not create a manual checkpoint on the run branch.

### The worker image does not match

Run <code>make worker-build</code>, verify the printed local image ID, and
update <code>worker_build.digest</code> in <code>factory.yaml</code>. A tag alone
is not sufficient for a run. Keep the image definition and digest change
together.

### Cleanup skips a run

Check that the run is terminal and at least seven days past its current
terminal transition, that its PR is not still open, and that no pending effect
or run-scoping discrepancy exists. Use the preview output as the source of
truth. Cleanup never removes a remote branch.

## Further reading

- [factory.yaml](factory.yaml) — checked-in policy for this Go repository.
- [docs/configuration.md](docs/configuration.md) — complete host/repository
  configuration, claim protocol, gates, worker mounts, recovery, and cleanup.
- [docs/agent-runtime.md](docs/agent-runtime.md) — visible harness lifecycle,
  invocation packets, reports, test handoffs, draft PRs, and the demonstration
  path.
- [docs/worker-runtime.md](docs/worker-runtime.md) — Docker worker contract,
  stable paths, identity, resume behavior, and security boundary.
- [CONTEXT.md](CONTEXT.md) — domain vocabulary, invariants, ownership, and
  lifecycle model.
- [docs/adr/0001-local-evaluation-without-outbound-telemetry.md](docs/adr/0001-local-evaluation-without-outbound-telemetry.md)
  — local evaluation and telemetry boundary.
- [docs/adr/0002-per-role-harness-selection-fails-closed.md](docs/adr/0002-per-role-harness-selection-fails-closed.md)
  — per-role harness selection policy.
- [docs/adr/0003-factory-owned-workflow-registry.md](docs/adr/0003-factory-owned-workflow-registry.md)
  — factory-owned workflow authority.

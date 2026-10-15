---
name: factory-onboarding
description: "Use when the operator wants to prepare the current repository for Software Factory runs, or to rebuild its worker image after a Factory upgrade."
---

# Factory onboarding

This skill starts the Software Factory onboarding in the current agent session.
It contains no procedure. The installed Factory release ships the procedure, and
that procedure is always correct when it disagrees with this file.

## 1. Find the installed Factory

Run `command -v factory`. If the operator names a different `factory`
executable, use that one. Run `<that path> version --json` and show the
`version` and `revision` to the operator. Ask which build to use if they are
not the build the operator expects. If no `factory` command exists, name
installation of a Factory release as the remedy; perform it when explicitly
authorized and permitted by the environment. Interactive login or consent is
a named human step.

Read `<that path> guide` and `<that path> guide authorization` before acting.
The installed guide owns scoped session authorization, including when to use
existing authority and when a new decision is needed. Discovering this skill
does not itself authorize paid calls or external actions.

## 2. Prepare the release resources

Choose a new directory below the home directory of the operator, for example
`$HOME/.cache/sw-factory/onboarding-<version>`. A VM-backed Docker daemon can
mount only paths in the home directory. Run:

```sh
<factory path> onboard prepare --output "<directory>" --json
```

A development build also needs `--worker-base-image <image>@sha256:<digest>`.
Ask the operator for that value. Never use a mutable tag.

If the command refuses the directory, choose a new directory. Do not remove or
change files in the directory to make it accept the directory.

The command prints a JSON manifest. Use only its values from now on:

| Manifest field | Procedure placeholder |
| --- | --- |
| `factory_command` | `<FACTORY_COMMAND>` |
| `resources` | `<FACTORY_RESOURCES>` |
| `worker_base_image` | `<FACTORY_BASE_IMAGE>` |
| top level of the current Git checkout | `<TARGET_REPO_PATH>` |

Run every `factory` command as `factory_command`. A `factory` on `PATH` can be
another build. Do not use files from an earlier preparation or from this
skill's directory instead of the prepared resources.

## 3. Continue an interrupted onboarding

Look for an onboarding plan of this repository at
`$HOME/.cache/sw-factory/onboarding-<repository name>.json`, and ask the
operator whether an earlier session kept one at another path. If a plan
exists, run this before any other step:

```sh
<factory path> onboard status --plan "<plan file>" --json
```

Do not do a step again that is `verified`. Continue with the first step that
is `pending` or `stale`, and follow its `action`. A `blocked` live proof has
an unknown outcome: find the issue that carries its marker, and never create
another proof issue. The procedure's section "Keep the onboarding plan"
describes the plan. Record the operator's stated scope and constraints with
`onboard plan --authorization-note <text>` as that procedure describes. Treat a
stored note as context, not authority for a new session. Only `onboard plan`
and `onboard record` change the plan; never edit the plan file.

## 4. Follow the procedure

Read the file at the manifest's `procedure` in full. Follow its ordered steps
from the first to the last, and run every step yourself. Make each
repository-specific choice, for example gates or the harness and model of each
worker role, using the operator's decisions already given in this session.
Apply `factory guide authorization` to each effect; ask only for missing scope
or decisions, with the reviewable action prepared. Never read a credential file.

## 5. Verify the result

The onboarding is complete only when the procedure's completion proof
succeeds. Report the result and each step that the operator skipped.

The prepared directory stays after the session. No committed file may refer
to it. Tell the operator that they can remove it when the onboarding is
complete.

# Spike: nested harness TTYs through cmux and Docker

Throwaway exploration for issue #2. **Not product code.** The deliverable is
`docs/spikes/0002-nested-harness-ttys.md`; this directory only exists so the
findings can be reproduced.

## Frozen 2026-08-20

These scripts are pinned to Claude Code 2.1.232, Codex 0.148.0, cmux 0.64.22,
and Docker server 29.5.2. Nothing maintains them and no CI runs them.

Both harnesses release often. If a probe fails later, the first assumption is
that a harness changed, not that something regressed. Re-verify the finding
against the current versions and update
`docs/spikes/0002-nested-harness-ttys.md`.

Delete this directory once #14 and #69 carry the same checks as real
`WorkerRuntime` contract tests.

```
./worker build      # build the image and pin its id
./worker up         # start the container from the pinned image
./worker recreate   # destroy and rebuild, keeping harness home volumes
./worker shell      # interactive pseudo-terminal into the container
./worker claude     # Claude Code in a nested pseudo-terminal
./worker codex      # Codex in a nested pseudo-terminal
./worker reset      # remove the container, home volumes, and .run

./cmux-surfaces     # create the cmux run workspace (run from inside cmux)
python3 bin/tty-probe   # automated nested-TTY checks
```

`bin/factory-report` is the stage reporting command installed into the image.

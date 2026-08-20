# Spike: nested harness TTYs through cmux and Docker

Throwaway exploration for issue #2. **Not product code.** The deliverable is
`docs/spikes/0002-nested-harness-ttys.md`; this directory only exists so the
findings can be reproduced.

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

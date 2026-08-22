#!/bin/sh

# Run one repository Go command without the coordinator's worker Git routing
# variables. This keeps local Git fixtures in Go tests independent of /git.
set -eu

unset GIT_DIR GIT_WORK_TREE GIT_CONFIG_COUNT \
  GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0 \
  GIT_CONFIG_KEY_1 GIT_CONFIG_VALUE_1 \
  GIT_CONFIG_KEY_2 GIT_CONFIG_VALUE_2

export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_TERMINAL_PROMPT=0
export GIT_ASKPASS=/bin/false
export SSH_ASKPASS=/bin/false
export GOFLAGS=-buildvcs=false

exec go "$@"

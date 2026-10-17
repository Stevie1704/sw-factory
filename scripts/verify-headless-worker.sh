#!/bin/sh

# Exercise the detached worker protocol through a real Docker container for
# every production harness adapter. The fake harnesses are deterministic and
# offline, so this verifies the worker boundary without spending a harness
# request or requiring credentials. From a Factory source checkout, it then runs
# the real-worker Go checks for command lifetime and resource limits against
# the same image.
set -eu

DOCKER="${DOCKER:-docker}"
REPOSITORY_ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)"

if [ "$#" -ne 1 ]; then
  echo "usage: $0 IMAGE@DIGEST" >&2
  exit 2
fi

WORKER_REFERENCE="$1"
temporary_root="$(mktemp -d "$REPOSITORY_ROOT/.headless-verify.XXXXXX")"
fake_codex="$temporary_root/codex"
fake_claude="$temporary_root/claude"
fake_copilot="$temporary_root/copilot"
# A sentinel, not a credential: it proves the launcher's projection path and
# fails GitHub validation if the real CLI ever sends it. It carries a
# supported token-type prefix because the pinned CLI ignores
# COPILOT_GITHUB_TOKEN without one (observed offline with 1.0.98).
copilot_token="$temporary_root/copilot-token"
copilot_sentinel="github_pat_factory_verification_sentinel_not_a_token"
results_directory="$temporary_root/results"
container_name="factory-headless-verify-$$"

cleanup() {
  "$DOCKER" rm -f "$container_name" >/dev/null 2>&1 || true
  rm -rf "$temporary_root"
}
trap cleanup EXIT HUP INT TERM

mkdir "$results_directory"
# Docker's fixed worker uid must be able to write the host-mounted result
# directory. It contains only disposable verification output.
chmod 0777 "$results_directory"

cat > "$fake_codex" <<'EOF'
#!/bin/sh

# This executable stands in for Codex's network-facing binary. It checks the
# image-provided skill roots, preserves the exact prompt, emits the native
# thread event, and uses the real in-image report command for completion.
set -eu

for terminal_binary in cmux tmux; do
  if command -v "$terminal_binary" >/dev/null 2>&1; then
    echo "headless Codex unexpectedly resolved $terminal_binary" >&2
    exit 1
  fi
done
if [ -t 0 ] || [ -t 1 ] || [ -t 2 ]; then
  echo "headless Codex unexpectedly inherited a TTY" >&2
  exit 1
fi

session="headless-verification-session"
prompt=""
resume=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    exec|--json)
      ;;
    -s|-c|-m)
      shift
      ;;
    resume)
      resume=1
      shift
      session="$1"
      ;;
    *)
      prompt="$1"
      ;;
  esac
  shift
done

test -s /home/factory/.codex/skills/implement/SKILL.md
test -s /home/factory/.claude/skills/implement/SKILL.md
printf '%s\n' "loaded" > /results/headless-verification-skill
if [ "$resume" -eq 1 ]; then
  printf '%s' "$prompt" > /results/headless-verification-resume-prompt
else
  printf '%s' "$prompt" > /results/headless-verification-prompt
fi
printf '{"type":"thread.started","thread_id":"%s"}\n' "$session"

case "${FAKE_HARNESS_MODE:-complete}" in
  complete)
    printf '{"type":"turn.completed"}\n'
    /usr/local/bin/factory-report \
      --outcome completed \
      --summary "headless Docker lifecycle verification" \
      --change-summary "verified the worker lifecycle" \
      --acceptance "lifecycle=real Docker verification" \
      --focused-command "scripts/verify-headless-worker.sh" \
      --native-session-id "$session"
    ;;
  hold)
    # Ignore TERM so the worker helper must prove its SIGKILL escalation path.
    trap '' TERM
    while :; do
      sleep 1
    done
    ;;
  *)
    echo "unknown FAKE_HARNESS_MODE" >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "$fake_codex"

cat > "$fake_claude" <<'EOF'
#!/bin/sh

# This executable stands in for the Claude Code CLI in its documented
# non-interactive mode. It refuses a launch that is not terminal-free machine
# readable, checks the image-provided skill roots, preserves the exact prompt,
# emits the stream-json init event carrying the factory-assigned session, and
# uses the real in-image report command for completion.
set -eu

for terminal_binary in cmux tmux; do
  if command -v "$terminal_binary" >/dev/null 2>&1; then
    echo "headless Claude unexpectedly resolved $terminal_binary" >&2
    exit 1
  fi
done
if [ -t 0 ] || [ -t 1 ] || [ -t 2 ]; then
  echo "headless Claude unexpectedly inherited a TTY" >&2
  exit 1
fi

session=""
prompt=""
resume=0
print_mode=0
output_format=""
verbose=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p)
      print_mode=1
      ;;
    --verbose)
      verbose=1
      ;;
    --output-format)
      shift
      output_format="$1"
      ;;
    --session-id)
      shift
      session="$1"
      ;;
    --resume)
      resume=1
      shift
      session="$1"
      ;;
    --dangerously-skip-permissions|--strict-mcp-config)
      ;;
    --mcp-config|--model|--effort)
      shift
      ;;
    *)
      prompt="$1"
      ;;
  esac
  shift
done

test "$print_mode" -eq 1
test "$output_format" = "stream-json"
test "$verbose" -eq 1
test -n "$session"
test -s /home/factory/.claude/skills/implement/SKILL.md
printf '%s\n' "loaded" > /results/headless-verification-skill
if [ "$resume" -eq 1 ]; then
  printf '%s' "$prompt" > /results/headless-verification-resume-prompt
else
  printf '%s' "$prompt" > /results/headless-verification-prompt
fi
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$session"

case "${FAKE_HARNESS_MODE:-complete}" in
  complete)
    /usr/local/bin/factory-report \
      --outcome completed \
      --summary "headless Docker lifecycle verification" \
      --change-summary "verified the worker lifecycle" \
      --acceptance "lifecycle=real Docker verification" \
      --focused-command "scripts/verify-headless-worker.sh" \
      --native-session-id "$session"
    printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s"}\n' "$session"
    ;;
  hold)
    # Ignore TERM so the worker helper must prove its SIGKILL escalation path.
    trap '' TERM
    while :; do
      sleep 1
    done
    ;;
  *)
    echo "unknown FAKE_HARNESS_MODE" >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "$fake_claude"

cat > "$fake_copilot" <<'EOF'
#!/bin/sh

# This executable stands in for the pinned Copilot CLI behind the real
# in-image launcher factory-copilot. It refuses a launch the launcher did not
# prepare: the sentinel token must arrive only as COPILOT_GITHUB_TOKEN, no
# other GitHub token or Copilot control may survive, and the private Copilot
# home must hold the managed settings and the curated skills. It preserves the
# exact prompt, emits the adapter's provisional identity evidence, and uses
# the real in-image report command for completion.
set -eu

for terminal_binary in cmux tmux; do
  if command -v "$terminal_binary" >/dev/null 2>&1; then
    echo "headless Copilot unexpectedly resolved $terminal_binary" >&2
    exit 1
  fi
done
if [ -t 0 ] || [ -t 1 ] || [ -t 2 ]; then
  echo "headless Copilot unexpectedly inherited a TTY" >&2
  exit 1
fi

session=""
prompt=""
resume=0
output_format=""
for argument in "$@"; do
  case "$argument" in
    --prompt=*) prompt="${argument#--prompt=}" ;;
    --output-format=*) output_format="${argument#--output-format=}" ;;
    --session-id=*) session="${argument#--session-id=}" ;;
    --resume=*)
      resume=1
      session="${argument#--resume=}"
      ;;
    --allow-all|--yolo|--allow-all-paths|--allow-all-urls|--continue|--resume|--remote|--share*|--add-dir*|--plugin-dir*)
      echo "headless Copilot received a forbidden option" >&2
      exit 1
      ;;
  esac
done

test "$output_format" = "json"
test -n "$session"
test "${COPILOT_GITHUB_TOKEN:-}" = "github_pat_factory_verification_sentinel_not_a_token"
test "${COPILOT_HOME:-}" = /home/factory/.copilot
test "${COPILOT_AUTO_UPDATE:-}" = false
test -z "${GH_TOKEN:-}${GITHUB_TOKEN:-}${COPILOT_ALLOW_ALL:-}"
cmp -s /usr/local/share/factory-copilot/settings.json "$COPILOT_HOME/settings.json"
test -s "$COPILOT_HOME/skills/implement/SKILL.md"
printf '%s\n' "loaded" > /results/headless-verification-skill
if [ "$resume" -eq 1 ]; then
  printf '%s' "$prompt" > /results/headless-verification-resume-prompt
else
  printf '%s' "$prompt" > /results/headless-verification-prompt
fi
printf '{"sessionId":"%s"}\n' "$session"

case "${FAKE_HARNESS_MODE:-complete}" in
  complete)
    /usr/local/bin/factory-report \
      --outcome completed \
      --summary "headless Docker lifecycle verification" \
      --change-summary "verified the worker lifecycle" \
      --acceptance "lifecycle=real Docker verification" \
      --focused-command "scripts/verify-headless-worker.sh" \
      --native-session-id "$session"
    ;;
  hold)
    # Ignore TERM so the worker helper must prove its SIGKILL escalation path.
    trap '' TERM
    while :; do
      sleep 1
    done
    ;;
  *)
    echo "unknown FAKE_HARNESS_MODE" >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "$fake_copilot"
printf '%s\n' "$copilot_sentinel" > "$copilot_token"
# The launcher runs as the worker user and reads the projection like the
# factory-managed 0444 copy.
chmod 0444 "$copilot_token"

# An image built before the Copilot harness has no launcher. It stays
# verifiable for Codex and Claude, and the Copilot checks say they did not run.
copilot_supported=no
if "$DOCKER" run --rm --pull=never --network none --entrypoint /bin/sh \
    "$WORKER_REFERENCE" -c 'test -x /usr/local/bin/factory-copilot' >/dev/null 2>&1; then
  copilot_supported=yes
fi

set -- --mount "type=bind,src=$fake_codex,dst=/tmp/factory-verification-codex,readonly" \
  --mount "type=bind,src=$fake_claude,dst=/tmp/factory-verification-claude,readonly" \
  --mount "type=bind,src=$results_directory,dst=/results"
if [ "$copilot_supported" = yes ]; then
  # The fake replaces only the network-facing CLI; the launcher under test is
  # the image's own factory-copilot.
  set -- "$@" \
    --mount "type=bind,src=$fake_copilot,dst=/usr/local/bin/copilot,readonly" \
    --mount "type=bind,src=$copilot_token,dst=/run/factory-auth/copilot-token,readonly"
fi

echo "Starting headless verification container from $WORKER_REFERENCE"
"$DOCKER" run -d --pull=never \
  --name "$container_name" \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --network none \
  "$@" \
  --workdir /work \
  "$WORKER_REFERENCE" sleep infinity >/dev/null

if "$DOCKER" exec "$container_name" /bin/sh -c \
    'command -v cmux >/dev/null 2>&1 || command -v tmux >/dev/null 2>&1'; then
  echo "the pinned worker image exposes a retired terminal multiplexer" >&2
  exit 1
fi

inspection_status() {
  invocation_id="$1"
  state_dir="/home/factory/.factory-headless/$invocation_id"
  "$DOCKER" exec "$container_name" /bin/sh -c \
    "/usr/local/bin/factory-worker-headless inspect --state-dir '$state_dir' | jq -r .status"
}

wait_for_status() {
  invocation_id="$1"
  expected="$2"
  attempt=0
  status="missing"
  while [ "$attempt" -lt 100 ]; do
    status="$(inspection_status "$invocation_id" 2>/dev/null || true)"
    if [ "$status" = "$expected" ]; then
      return 0
    fi
    sleep 0.1
    attempt=$((attempt + 1))
  done
  echo "headless invocation $invocation_id did not reach $expected (last: $status)" >&2
  exit 1
}

assert_session_event() {
  invocation_id="$1"
  marker="$2"
  state_dir="/home/factory/.factory-headless/$invocation_id"
  "$DOCKER" exec "$container_name" /bin/sh -c \
    "/usr/local/bin/factory-worker-headless inspect --state-dir '$state_dir' \
      | jq -r .stdout | base64 -d \
      | grep -q '$marker'"
}

# harness_command prints the argv one adapter builds, so the verification runs
# the same command translation the coordinator uses.
harness_command() {
  harness="$1"
  resume="$2"
  session="$3"
  prompt="$4"
  case "$harness" in
    codex)
      if [ "$resume" = yes ]; then
        printf '%s\n' /tmp/factory-verification-codex exec --json \
          -s danger-full-access -c project_doc_max_bytes=0 resume "$session" "$prompt"
      else
        printf '%s\n' /tmp/factory-verification-codex exec --json \
          -s danger-full-access -c project_doc_max_bytes=0 "$prompt"
      fi
      ;;
    claude)
      if [ "$resume" = yes ]; then
        printf '%s\n' /tmp/factory-verification-claude -p --output-format stream-json --verbose \
          --dangerously-skip-permissions --strict-mcp-config --mcp-config '{"mcpServers":{}}' \
          --resume "$session" "$prompt"
      else
        printf '%s\n' /tmp/factory-verification-claude -p --output-format stream-json --verbose \
          --dangerously-skip-permissions --strict-mcp-config --mcp-config '{"mcpServers":{}}' \
          --session-id "$session" "$prompt"
      fi
      ;;
    copilot)
      printf '%s\n' factory-copilot "--prompt=$prompt" --output-format=json --allow-all-tools \
        --no-ask-user --no-auto-update --no-remote --no-remote-export --disable-builtin-mcps \
        --no-custom-instructions --secret-env-vars=COPILOT_GITHUB_TOKEN
      if [ "$resume" = yes ]; then
        printf '%s\n' "--resume=$session"
      else
        printf '%s\n' "--session-id=$session"
      fi
      ;;
  esac
}

run_headless() {
  harness="$1"
  invocation_id="$2"
  mode="$3"
  replace="$4"
  session="$5"
  prompt="$6"
  state_dir="/home/factory/.factory-headless/$invocation_id"
  set -- "$container_name" /usr/local/bin/factory-worker-headless run --state-dir "$state_dir"
  if [ "$replace" = yes ]; then
    set -- "$@" --replace
  fi
  set -- "$@" --
  # The adapter argv is newline separated, so the field separator is narrowed
  # while it becomes positional arguments and restored immediately after.
  original_ifs="$IFS"
  IFS='
'
  # shellcheck disable=SC2046
  set -- "$@" $(harness_command "$harness" "$replace" "$session" "$prompt")
  IFS="$original_ifs"
  # A host token or a trust switch in the caller's environment must not reach
  # Copilot; the launcher removes both. Only this verification plants them,
  # because the coordinator's worker policy already refuses token variables.
  planted=""
  if [ "$harness" = copilot ]; then
    planted="--env GH_TOKEN=planted-host-token --env COPILOT_ALLOW_ALL=true"
  fi
  # shellcheck disable=SC2086
  "$DOCKER" exec -d $planted \
    --env "FACTORY_INVOCATION_ID=$invocation_id" \
    --env FACTORY_RUN_ID=headless-verification-run \
    --env "FACTORY_HARNESS=$harness" \
    --env FACTORY_ROLE=implementation \
    --env FACTORY_STAGE=implementation \
    --env FACTORY_RESULT_DIR=/results \
    --env "FAKE_HARNESS_MODE=$mode" \
    "$@" >/dev/null
}

verify_harness() {
  harness="$1"
  session="$2"
  session_marker="$3"
  echo "Verifying the $harness headless adapter"
  "$DOCKER" exec "$container_name" rm -f \
    /results/headless-verification-prompt /results/headless-verification-resume-prompt \
    /results/headless-verification-skill /results/report.json

  complete_invocation="verify-$harness-complete"
  complete_prompt="FROZEN_HEADLESS_PROMPT_${harness}_v164"
  run_headless "$harness" "$complete_invocation" complete no "$session" "$complete_prompt"
  wait_for_status "$complete_invocation" exited
  assert_session_event "$complete_invocation" "$session_marker"
  "$DOCKER" exec "$container_name" test -s /results/headless-verification-skill
  received="$("$DOCKER" exec "$container_name" cat /results/headless-verification-prompt)"
  test "$received" = "$complete_prompt"
  "$DOCKER" exec "$container_name" jq -e \
    --arg invocation "$complete_invocation" --arg session "$session" \
    '.invocation_id == $invocation and .run_id == "headless-verification-run" and
     .native_session_id == $session and .outcome == "completed"' \
    /results/report.json >/dev/null
  echo "  skill loading, frozen prompt delivery, and report production verified"

  cancel_invocation="verify-$harness-cancel"
  cancel_prompt="FROZEN_CANCELLATION_PROMPT_${harness}_v164"
  run_headless "$harness" "$cancel_invocation" hold no "$session" "$cancel_prompt"
  wait_for_status "$cancel_invocation" running
  # This inspection is deliberately issued by a new container process: the
  # coordinator can restart while the worker and its durable process state live.
  assert_session_event "$cancel_invocation" "$session_marker"
  "$DOCKER" exec "$container_name" /usr/local/bin/factory-worker-headless cancel \
    --state-dir "/home/factory/.factory-headless/$cancel_invocation" >/dev/null
  wait_for_status "$cancel_invocation" cancelled
  echo "  cancellation and coordinator-restart recovery verified"

  resume_prompt="FROZEN_NATIVE_RESUME_PROMPT_${harness}_v164"
  run_headless "$harness" "$cancel_invocation" complete yes "$session" "$resume_prompt"
  wait_for_status "$cancel_invocation" exited
  received="$("$DOCKER" exec "$container_name" cat /results/headless-verification-resume-prompt)"
  test "$received" = "$resume_prompt"
  "$DOCKER" exec "$container_name" jq -e \
    --arg invocation "$cancel_invocation" --arg session "$session" \
    '.invocation_id == $invocation and .native_session_id == $session and
     .outcome == "completed"' \
    /results/report.json >/dev/null
  echo "  native resume verified"
}

# assert_claude_session_contract runs the real pinned Claude Code binary in the
# offline container with the exact options the adapter builds. Asserting that
# skill files exist would only prove the image copied them, and asserting that
# an option was passed would only prove the adapter spelled it; the harness's
# own init event and a planted hook are the evidence that the launch actually
# loads every curated skill and actually refuses worktree-declared commands.
# The run needs no credential: both effects land before the request that fails.
assert_claude_session_contract() {
  echo "Verifying the pinned Claude Code session contract"
  stream="$temporary_root/claude-init.jsonl"
  # A repository controls its own worktree, so the hostile case is a worktree
  # that both declares a hook and tries to re-enable hooks for itself.
  "$DOCKER" exec "$container_name" /bin/sh -c \
    "mkdir -p /work/.claude && rm -f /tmp/worktree-hook-ran && cat > /work/.claude/settings.json <<'SETTINGS'
{\"disableAllHooks\": false,
 \"hooks\": {\"SessionStart\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"touch /tmp/worktree-hook-ran\"}]}]}}
SETTINGS"
  "$DOCKER" exec "$container_name" /bin/sh -c \
    "claude -p --output-format stream-json --verbose \
       --dangerously-skip-permissions --strict-mcp-config --mcp-config '{\"mcpServers\":{}}' \
       --settings '{\"disableAllHooks\":true}' \
       --session-id 5d1f2a83-0c4e-4f7a-9b2e-6a1c8d3e5f70 'noop' 2>/dev/null" > "$stream" || true
  if "$DOCKER" exec "$container_name" test -f /tmp/worktree-hook-ran; then
    echo "the pinned Claude Code binary executed a worktree-declared hook" >&2
    exit 1
  fi
  "$DOCKER" exec "$container_name" rm -rf /work/.claude
  for skill in $("$DOCKER" exec "$container_name" ls /home/factory/.claude/skills); do
    if ! jq -e --arg skill "$skill" \
        'select(.type == "system" and .subtype == "init") | any(.skills[]?; . == $skill)' \
        "$stream" >/dev/null; then
      echo "the pinned Claude Code binary did not load the curated skill $skill" >&2
      exit 1
    fi
  done
  echo "  worktree-declared hooks refused and every curated worker skill loaded"
}

# assert_copilot_launcher_contract runs the image's real launcher and pinned
# Copilot CLI in disposable offline containers. It needs no credential: a
# missing projection fails in the launcher, and the sentinel reaches the
# CLI's own token validation, which proves the projected token, not a host or
# gh credential, is the one the CLI read. Live suppression of repository
# hooks, MCP servers, plugins, and instructions stays release blocker RB-7.
assert_copilot_launcher_contract() {
  echo "Verifying the pinned Copilot CLI behind factory-copilot"
  copilot_session=6c2b8e4f-1d3a-4b7c-9e5f-0a8d2c4b6e19
  set -- run --rm --pull=never --cap-drop ALL --security-opt no-new-privileges --network none \
    --workdir /work --entrypoint /bin/sh

  missing_status=0
  missing_stderr="$("$DOCKER" "$@" "$WORKER_REFERENCE" -c 'factory-copilot --version 2>&1 >/dev/null')" || missing_status=$?
  if [ "$missing_status" -ne 78 ] || [ "$missing_stderr" != "factory-copilot: credential unavailable" ]; then
    echo "factory-copilot without a credential did not report the one seam line" >&2
    exit 1
  fi

  # The planted user-level MCP file and settings must not survive the launch,
  # project skills must stay hidden, and the CLI must stay on its pinned
  # release with no downloaded update.
  probe='set -eu
mkdir -p /home/factory/.copilot /work/.github/skills/planted-repository-skill
printf "{\"mcpServers\":{\"planted\":{\"command\":\"/bin/false\"}}}\n" > /home/factory/.copilot/mcp-config.json
printf "{\"disableAllHooks\":false}\n" > /home/factory/.copilot/settings.json
printf -- "---\nname: planted-repository-skill\ndescription: planted\n---\nbody\n" \
  > /work/.github/skills/planted-repository-skill/SKILL.md
status=0
factory-copilot --prompt=noop --output-format=json --allow-all-tools --no-ask-user --no-auto-update \
  --no-remote --no-remote-export --disable-builtin-mcps --no-custom-instructions \
  --secret-env-vars=COPILOT_GITHUB_TOKEN --session-id='"$copilot_session"' \
  >/tmp/copilot.stdout 2>/tmp/copilot.stderr || status=$?
test "$status" -eq 1
test ! -s /tmp/copilot.stdout
test "$(head -n 1 /tmp/copilot.stderr)" = "Error: Authentication token found but could not be validated."
if grep -q "'"$copilot_sentinel"'" /tmp/copilot.stderr; then exit 1; fi
test ! -e /home/factory/.copilot/mcp-config.json
cmp -s /usr/local/share/factory-copilot/settings.json /home/factory/.copilot/settings.json
test "$(ls /home/factory/.cache/copilot/pkg/*/)" = "$(copilot --version | sed -n "s/^GitHub Copilot CLI \(.*\)\.$/\1/p")"
factory-copilot skill list > /tmp/copilot.skills
if grep -q planted-repository-skill /tmp/copilot.skills; then exit 1; fi
for skill in $(ls /home/factory/.copilot/skills); do
  grep -q "$skill" /tmp/copilot.skills
done'
  if ! "$DOCKER" "$@" \
      --mount "type=bind,src=$copilot_token,dst=/run/factory-auth/copilot-token,readonly" \
      "$WORKER_REFERENCE" -c "$probe"; then
    echo "the pinned Copilot CLI behind factory-copilot broke the worker contract" >&2
    exit 1
  fi
  echo "  credential refusal, token projection, managed settings, pinned release, and curated skills verified"
}

verify_harness codex headless-verification-session '"type":"thread.started"'
verify_harness claude 8f14e45f-ceea-467a-9575-1b0a4b2a4bd9 '"subtype":"init"'
assert_claude_session_contract
if [ "$copilot_supported" = yes ]; then
  verify_harness copilot 3b9e1c7a-5f2d-4e8b-a6c0-9d4f2e1b7a53 '"sessionId":"3b9e1c7a-5f2d-4e8b-a6c0-9d4f2e1b7a53"'
  assert_copilot_launcher_contract
else
  echo "Copilot checks did not run: $WORKER_REFERENCE predates the Copilot harness (no factory-copilot)"
fi

# The resources a release exports carry no Go source. The release automation
# already ran these checks against the base image the release names.
if [ -f "$REPOSITORY_ROOT/go.mod" ]; then
  echo "Verifying real worker command lifetime and resource limits"
  (cd "$REPOSITORY_ROOT" && FACTORY_DOCKER_WORKER_IMAGE="$WORKER_REFERENCE" "${GO:-go}" test -count=1 -run '^TestRealWorker' ./internal/worker)
else
  echo "Skipping the Go command lifetime and resource limit checks: they need the Factory source, and the release verified its base image with them"
fi
echo "Headless worker lifecycle verification passed for $WORKER_REFERENCE"

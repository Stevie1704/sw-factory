#!/bin/sh

# Exercise the detached worker protocol through a real Docker container. The
# fake Codex is deterministic and offline, so this verifies the worker boundary
# without spending a harness request or requiring credentials.
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

case "${FAKE_CODEX_MODE:-complete}" in
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
    echo "unknown FAKE_CODEX_MODE" >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "$fake_codex"

echo "Starting headless verification container from $WORKER_REFERENCE"
"$DOCKER" run -d --pull=never \
  --name "$container_name" \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --network none \
  --mount "type=bind,src=$fake_codex,dst=/tmp/factory-verification-codex,readonly" \
  --mount "type=bind,src=$results_directory,dst=/results" \
  --workdir /work \
  "$WORKER_REFERENCE" sleep infinity >/dev/null

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

assert_thread_event() {
  invocation_id="$1"
  state_dir="/home/factory/.factory-headless/$invocation_id"
  "$DOCKER" exec "$container_name" /bin/sh -c \
    "/usr/local/bin/factory-worker-headless inspect --state-dir '$state_dir' \
      | jq -r .stdout | base64 -d \
      | grep -q '\"type\":\"thread.started\"'"
}

run_headless() {
  invocation_id="$1"
  mode="$2"
  replace="$3"
  prompt="$4"
  state_dir="/home/factory/.factory-headless/$invocation_id"
  if [ "$replace" = yes ]; then
    "$DOCKER" exec -d \
      --env "FACTORY_INVOCATION_ID=$invocation_id" \
      --env FACTORY_RUN_ID=headless-verification-run \
      --env FACTORY_HARNESS=codex \
      --env FACTORY_ROLE=implementation \
      --env FACTORY_STAGE=implementation \
      --env FACTORY_RESULT_DIR=/results \
      --env "FAKE_CODEX_MODE=$mode" \
      "$container_name" /usr/local/bin/factory-worker-headless run \
      --state-dir "$state_dir" --replace -- \
      /tmp/factory-verification-codex exec --json \
      -s danger-full-access -c project_doc_max_bytes=0 \
      resume headless-verification-session "$prompt" >/dev/null
  else
    "$DOCKER" exec -d \
      --env "FACTORY_INVOCATION_ID=$invocation_id" \
      --env FACTORY_RUN_ID=headless-verification-run \
      --env FACTORY_HARNESS=codex \
      --env FACTORY_ROLE=implementation \
      --env FACTORY_STAGE=implementation \
      --env FACTORY_RESULT_DIR=/results \
      --env "FAKE_CODEX_MODE=$mode" \
      "$container_name" /usr/local/bin/factory-worker-headless run \
      --state-dir "$state_dir" -- \
      /tmp/factory-verification-codex exec --json \
      -s danger-full-access -c project_doc_max_bytes=0 "$prompt" >/dev/null
  fi
}

complete_invocation="verify-complete"
complete_prompt="FROZEN_HEADLESS_PROMPT_v163"
run_headless "$complete_invocation" complete no "$complete_prompt"
wait_for_status "$complete_invocation" exited
assert_thread_event "$complete_invocation"
"$DOCKER" exec "$container_name" test -s /results/headless-verification-skill
complete_received_prompt="$("$DOCKER" exec "$container_name" cat /results/headless-verification-prompt)"
test "$complete_received_prompt" = "$complete_prompt"
"$DOCKER" exec "$container_name" jq -e \
  '.invocation_id == "verify-complete" and .run_id == "headless-verification-run" and
   .native_session_id == "headless-verification-session" and .outcome == "completed"' \
  /results/report.json >/dev/null
echo "  skill loading, frozen prompt delivery, and report production verified"

cancel_invocation="verify-cancel"
cancel_prompt="FROZEN_CANCELLATION_PROMPT_v163"
run_headless "$cancel_invocation" hold no "$cancel_prompt"
wait_for_status "$cancel_invocation" running
# This inspection is deliberately issued by a new container process: the
# coordinator can restart while the worker and its durable process state live.
assert_thread_event "$cancel_invocation"
"$DOCKER" exec "$container_name" /usr/local/bin/factory-worker-headless cancel \
  --state-dir "/home/factory/.factory-headless/$cancel_invocation" >/dev/null
wait_for_status "$cancel_invocation" cancelled
echo "  cancellation and coordinator-restart recovery verified"

resume_prompt="FROZEN_NATIVE_RESUME_PROMPT_v163"
run_headless "$cancel_invocation" complete yes "$resume_prompt"
wait_for_status "$cancel_invocation" exited
"$DOCKER" exec "$container_name" test -s /results/headless-verification-resume-prompt
resume_received_prompt="$("$DOCKER" exec "$container_name" cat /results/headless-verification-resume-prompt)"
test "$resume_received_prompt" = "$resume_prompt"
"$DOCKER" exec "$container_name" jq -e \
  '.invocation_id == "verify-cancel" and .native_session_id == "headless-verification-session" and
   .outcome == "completed"' \
  /results/report.json >/dev/null
echo "  native resume verified"
echo "Headless worker lifecycle verification passed for $WORKER_REFERENCE"

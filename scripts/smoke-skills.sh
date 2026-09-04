#!/bin/sh

# Prove each shipped harness can load and use the role-mandated worker skills
# inside the pinned worker image, and record the result keyed by the image
# digest and the harness version. Startup diagnostics read that record instead
# of repeating this paid, nondeterministic model call.
set -eu

DOCKER="${DOCKER:-docker}"
REPOSITORY_ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)"
EVIDENCE_FILE="${EVIDENCE_FILE:-$REPOSITORY_ROOT/worker/skill-smoke.json}"
WORKER_IMAGE="${WORKER_IMAGE:-$(sed -n 's/^  image: \(.*\)$/\1/p' "$REPOSITORY_ROOT/factory.yaml")}"
WORKER_DIGEST="${WORKER_DIGEST:-$(sed -n 's/^  digest: \(.*\)$/\1/p' "$REPOSITORY_ROOT/factory.yaml")}"
CODEX_AUTH_PATH="${CODEX_AUTH_PATH:-$HOME/.codex/auth.json}"
CLAUDE_AUTH_PATH="${CLAUDE_AUTH_PATH:-$HOME/.claude/.credentials.json}"
HARNESSES="${HARNESSES:-codex claude}"
MANDATORY_SKILLS="${MANDATORY_SKILLS:-implement specification-review standards-review}"
WORKER_REFERENCE="$WORKER_IMAGE@$WORKER_DIGEST"

# credential_path returns the host credential file one harness authenticates
# with, and the in-worker file it is streamed to.
credential_path() {
  case "$1" in
    codex) echo "$CODEX_AUTH_PATH /home/factory/.codex/auth.json" ;;
    claude) echo "$CLAUDE_AUTH_PATH /home/factory/.claude/.credentials.json" ;;
    *) echo "unsupported harness $1" >&2; exit 1 ;;
  esac
}

# harness_invocation returns the non-interactive command one harness answers a
# single prompt with. The worker is the security boundary, so both harnesses
# run without their redundant inner approval gates, exactly as a run does.
harness_invocation() {
  case "$1" in
    codex) echo "codex exec --skip-git-repo-check -s danger-full-access -c project_doc_max_bytes=0" ;;
    claude) echo "claude -p --dangerously-skip-permissions" ;;
    *) echo "unsupported harness $1" >&2; exit 1 ;;
  esac
}

# skill_sentinel returns the first instruction line of one skill body. The line
# exists only inside the installed SKILL.md, so a reply that quotes it could
# not have been produced without loading the skill.
skill_sentinel() {
  awk 'BEGIN { markers = 0 }
    /^---$/ { markers++; next }
    markers >= 2 && NF { print; exit }' "$REPOSITORY_ROOT/worker/skills/$1/SKILL.md"
}

# ask_harness streams one host credential into a disposable worker and asks
# that harness one prompt there. The credential never reaches a Docker
# argument, and the prompt reaches the harness through the environment rather
# than through in-container shell quoting.
ask_harness() {
  harness_name="$1"
  smoke_prompt="$2"
  invocation="$(harness_invocation "$harness_name")"
  set -- $(credential_path "$harness_name")
  host_credential="$1"
  worker_credential="$2"
  "$DOCKER" run --rm -i --pull=never --user 10001:10001 \
    --cap-drop ALL --security-opt no-new-privileges \
    --env "SMOKE_PROMPT=$smoke_prompt" \
    --entrypoint /bin/sh "$WORKER_REFERENCE" -c "
      umask 077
      mkdir -p \"\$(dirname $worker_credential)\"
      cat > $worker_credential
      chmod 0400 $worker_credential
      $invocation \"\$SMOKE_PROMPT\"
    " < "$host_credential"
}

# record_evidence merges one verified smoke result into the evidence file.
record_evidence() {
  jq --arg digest "$WORKER_DIGEST" \
     --arg harness "$1" \
     --arg version "$2" \
     --arg verified "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
     --argjson skills "$3" \
     '.schema_version = 1
      | .records = ((.records // [])
        | map(select(.image_digest != $digest or .harness != $harness))
        + [{image_digest: $digest, harness: $harness, harness_version: $version, skills: $skills, verified_at: $verified}]
        | sort_by(.image_digest, .harness))' \
     "$EVIDENCE_FILE" > "$EVIDENCE_FILE.next"
  mv "$EVIDENCE_FILE.next" "$EVIDENCE_FILE"
}

test -f "$EVIDENCE_FILE" || printf '{\n  "schema_version": 1,\n  "records": []\n}\n' > "$EVIDENCE_FILE"

for harness_name in $HARNESSES; do
  set -- $(credential_path "$harness_name")
  if [ ! -f "$1" ]; then
    echo "No $harness_name credential at $1; skipping its smoke." >&2
    echo "Startup stays blocked for $harness_name until its result is recorded." >&2
    continue
  fi

  version="$("$DOCKER" run --rm --pull=never --user 10001:10001 \
    --cap-drop ALL --security-opt no-new-privileges --network none \
    --entrypoint /bin/sh "$WORKER_REFERENCE" -c "$harness_name --version" | head -n 1 | tr -d '\r')"
  echo "Smoking $harness_name $version in $WORKER_REFERENCE"

  verified='[]'
  for skill in $MANDATORY_SKILLS; do
    sentinel="$(skill_sentinel "$skill")"
    prompt="Use the \`$skill\` skill. Reply with the first instruction line of that skill, verbatim, and nothing else."
    reply="$(ask_harness "$harness_name" "$prompt" 2>/dev/null || true)"
    case "$reply" in
      *"$sentinel"*) ;;
      *)
        echo "$harness_name could not load the \`$skill\` skill in $WORKER_REFERENCE" >&2
        exit 1
        ;;
    esac
    verified="$(printf '%s' "$verified" | jq --arg skill "$skill" '. + [$skill]')"
    echo "  $skill loaded"
  done

  record_evidence "$harness_name" "$version" "$verified"
  echo "Recorded $harness_name evidence for $WORKER_DIGEST in $EVIDENCE_FILE"
done

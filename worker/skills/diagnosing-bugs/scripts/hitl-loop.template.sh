#!/usr/bin/env bash
# Human-in-the-loop reproduction loop.
# Copy this file, edit the steps below, and run it.
# The agent runs the script; the user follows prompts in their terminal.
#
# Usage:
#   bash hitl-loop.template.sh
#
# Two helpers:
#   step "<instruction>"                    → show instruction, wait for Enter
#   capture VAR "<question>"                → show question, read one line into VAR
#   capture_multiline VAR "<question>" END  → read lines into VAR until END
#
# At the end, captured values are printed as KEY=VALUE for the agent to parse.
#
# `capture` prints its value back to the terminal, where the agent reads it,
# so capture observations, and leave signing in to the user as a `step`.

set -euo pipefail

step() {
  printf '\n>>> %s\n' "$1"
  read -r -p "    [Enter when done] " _
}

capture() {
  local var="$1" question="$2" answer
  printf '\n>>> %s\n' "$question"
  read -r -p "    > " answer
  printf -v "$var" '%s' "$answer"
}

capture_multiline() {
  local var="$1" question="$2" sentinel="$3" answer="" line separator=""
  printf '\n>>> %s\n' "$question"
  printf '    Finish with a line containing only %s.\n' "$sentinel"
  while IFS= read -r -p "    > " line; do
    [[ "$line" == "$sentinel" ]] && break
    answer+="${separator}${line}"
    separator=$'\n'
  done
  printf -v "$var" '%s' "$answer"
}

redact() {
  sed -E \
    -e 's/([Aa]uthorization:[[:space:]]*)[^[:space:]]+([[:space:]]+[^[:space:]]+)?/\1<REDACTED>/g' \
    -e 's/([Bb]earer[[:space:]]+)[A-Za-z0-9._~+\/=:-]+/\1<REDACTED>/g' \
    -e 's/(([Aa][Pp][Ii][_-]?[Kk][Ee][Yy]|[Aa]ccess[_-]?[Tt]oken|[Rr]efresh[_-]?[Tt]oken|[Tt]oken|[Pp]assword|[Ss]ecret)[[:space:]]*[:=][[:space:]]*)("[^"]*"|'"'"'[^'"'"']*'"'"'|[^[:space:],;]+)/\1<REDACTED>/g' \
    -e 's/(gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|AKIA[A-Z0-9]{16})/<REDACTED>/g'
}

# --- edit below ---------------------------------------------------------

step "Open the app at http://localhost:3000 and sign in."

capture ERRORED "Click the 'Export' button. Did it throw an error? (y/n)"

capture_multiline ERROR_MSG "Paste the error message (or 'none'):" "END_ERROR"

# --- edit above ---------------------------------------------------------

printf '\n--- Captured ---\n'
printf 'ERRORED=%s\n' "$ERRORED"
redacted_error_msg="$(printf '%s' "$ERROR_MSG" | redact)"
printf 'ERROR_MSG=%q\n' "$redacted_error_msg"

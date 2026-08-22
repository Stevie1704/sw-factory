#!/usr/bin/env bash

# Start the Understand Anything dashboard for this project or a supplied path.

set -euo pipefail

PROJECT_DIR="${1:-$(pwd -P)}"
PROJECT_DIR="$(cd "$PROJECT_DIR" 2>/dev/null && pwd -P)" || {
  printf 'Error: project directory not found: %s\n' "${1:-$(pwd -P)}" >&2
  exit 1
}

if [[ -d "$PROJECT_DIR/.understand-anything" ]]; then
  UA_DIR="$PROJECT_DIR/.understand-anything"
else
  UA_DIR="$PROJECT_DIR/.ua"
fi

if [[ ! -f "$UA_DIR/knowledge-graph.json" ]]; then
  printf 'No knowledge graph found. Run /understand first to analyze this project.\n' >&2
  exit 1
fi

PLUGIN_ROOT=""
for candidate in \
  "${CLAUDE_PLUGIN_ROOT:-}" \
  "$HOME/.understand-anything-plugin" \
  "$HOME/.codex/understand-anything/understand-anything-plugin" \
  "$HOME/.opencode/understand-anything/understand-anything-plugin" \
  "$HOME/.pi/understand-anything/understand-anything-plugin" \
  "$HOME/understand-anything/understand-anything-plugin"; do
  if [[ -x "$candidate/packages/dashboard/node_modules/.bin/vite" ]]; then
    PLUGIN_ROOT="$candidate"
    break
  fi
done

if [[ -z "$PLUGIN_ROOT" ]]; then
  printf 'Error: Cannot find the installed Understand Anything dashboard.\n' >&2
  exit 1
fi

DASHBOARD_DIR="$PLUGIN_ROOT/packages/dashboard"
VITE_BIN="$DASHBOARD_DIR/node_modules/.bin/vite"

printf 'Viewing: %s/knowledge-graph.json\n' "$UA_DIR"
printf 'Starting dashboard...\n'

cd "$DASHBOARD_DIR"
GRAPH_DIR="$PROJECT_DIR" exec "$VITE_BIN" --host 127.0.0.1

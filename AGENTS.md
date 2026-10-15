## Agent skills

### Issue tracker

Issues and specs live as GitHub issues; use the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Uses the five canonical labels unchanged. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout with the root domain glossary `GLOSSARY.md` and `docs/adr/`. See `docs/agents/domain.md`.

### Verification

Run `make check` before you commit. The README section "Where the contracts live" maps each contract to its canonical document.

### Documentation

During coding create docstrings for functions and definitions.

A missing or incomplete docstring is an advisory finding, never a blocker. It does not gate a checkpoint on its own.